package handlers

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/ttalvac/bump-server/cache"
	"github.com/ttalvac/bump-server/crypto"
	"github.com/ttalvac/bump-server/db"
	"github.com/ttalvac/bump-server/middleware"
)

type SessionHandler struct {
	signer          *crypto.Signer
	queries         *db.Queries
	limiter         *cache.RateLimiter
	maxHour         int
	freeBumpsPerDay int
	devAllowlist    map[string]bool
}

func NewSessionHandler(
	signer *crypto.Signer,
	queries *db.Queries,
	limiter *cache.RateLimiter,
	maxHour int,
	freeBumpsPerDay int,
	devAllowlist map[string]bool,
) *SessionHandler {
	return &SessionHandler{
		signer:          signer,
		queries:         queries,
		limiter:         limiter,
		maxHour:         maxHour,
		freeBumpsPerDay: freeBumpsPerDay,
		devAllowlist:    devAllowlist,
	}
}

type sessionRequest struct {
	DeviceHash     string `json:"device_hash"`
	ClientTime     int64  `json:"client_time"`
	IntegrityToken string `json:"integrity_token"`
}

type sessionResponse struct {
	Token         string `json:"token"`
	ServerTime    int64  `json:"server_time"`
	SessionID     string `json:"session_id"`     // opaque id the client echoes back to /session/commit
	FreeRemaining int    `json:"free_remaining"` // committed balance at /session time (not decremented until commit)
	PaidBalance   int    `json:"paid_balance"`   // committed balance at /session time
}

// newSessionID generates an opaque 32-char hex token used as the
// reservation/commit handle. 16 bytes of randomness is more than enough
// to make guessing a live sid infeasible and is symmetric with the
// existing hex device_hash format so middleware.IsValidDeviceHash can
// double as the validator on the commit endpoint.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type errorResponse struct {
	Error      string `json:"error,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

// outOfBumpsResponse is the dedicated 403 body for /session when a device
// has no bumps left. Fields do NOT use `omitempty` because the client relies
// on seeing literal "free_remaining":0/"paid_balance":0 for UI sync — dropping
// those zeros would break the contract (audit finding C2).
type outOfBumpsResponse struct {
	Error         string `json:"error"`
	FreeRemaining int    `json:"free_remaining"`
	PaidBalance   int    `json:"paid_balance"`
}

func (h *SessionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req sessionRequest
	if err := json.NewDecoder(limitedBody(r)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}

	// Validate device hash format
	if !middleware.IsValidDeviceHash(req.DeviceHash) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid device_hash"})
		return
	}

	// Validate clock sync: |client_time - server_time| < 2 seconds
	now := time.Now().UnixMilli()
	clockDiff := math.Abs(float64(req.ClientTime - now))
	if clockDiff > 2000 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "clock out of sync"})
		return
	}

	ctx := r.Context()

	// Check blocklist
	blocked, err := h.queries.IsBlocked(ctx, req.DeviceHash)
	if err == nil && blocked {
		// Silent denial -- return a valid-looking delayed response
		// that will never match anyone (the token is valid but the device
		// is ignored by all other clients via remote config blocklist)
		time.Sleep(5 * time.Second)
	}

	// Dev devices bypass ALL quota checks (hourly rate limit AND daily bump
	// quota) so we can test freely in prod. Keeping the isDev check above
	// CheckRateLimit matches the promise in the startup log message: dev
	// allowlist devices are fully exempt, not just quota-exempt.
	isDev := h.devAllowlist[req.DeviceHash]

	// Generate a fresh session id up front — every /session response carries
	// one, even for dev-bypass paths, so the client's reserve→commit flow is
	// uniform regardless of allowlist status.
	sessionID, err := newSessionID()
	if err != nil {
		log.Printf("session id generation failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "session id generation failed"})
		return
	}

	var freeRemaining, paidBalance int
	if !isDev {
		// Rate limiting (hourly request cap, independent of daily bump quota).
		// We keep this on /session (not /session/commit) so a malicious
		// client that never commits is still bounded — they can create at
		// most maxPerHour reservations per hour, all of which expire in 60s.
		allowed, _, delay := h.limiter.CheckRateLimit(ctx, req.DeviceHash, h.maxHour)
		if !allowed {
			writeJSON(w, http.StatusTooManyRequests, errorResponse{RetryAfter: 60})
			return
		}

		// Progressive throttle
		if delay > 0 {
			time.Sleep(delay)
		}

		// ---- Daily bump reservation ----
		// /session no longer decrements any counter — it reserves a slot.
		// The atomic decrement happens at /session/commit after the client
		// proves a successful BLE match (SessionState.Ready). Unclaimed
		// reservations expire naturally in 60s. The response's
		// free_remaining / paid_balance reflect the CURRENT COMMITTED
		// balance so the UI doesn't lie about post-commit state.
		//
		// Order: try free first (reserve), else try paid (reserve marker
		// only — paid balance check is at commit time because Postgres
		// can't cheaply peek without mutating).
		freeUsed, freeOk, err := h.limiter.TryReserveFreeBump(ctx, req.DeviceHash, sessionID, h.freeBumpsPerDay)
		if err != nil {
			log.Printf("free bump reserve failed for %s: %v", req.DeviceHash, err)
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
			return
		}

		if freeOk {
			freeRemaining = h.freeBumpsPerDay - freeUsed
			paidBalance, _ = h.queries.GetPaidBumps(ctx, req.DeviceHash)
		} else {
			// Daily free limit reached — check paid balance. We do a
			// non-atomic read here to decide whether to issue a paid
			// reservation; the atomic check-and-decrement happens at
			// commit time. A rare TOCTOU loses a reservation to a 409 at
			// commit, which the client treats as OutOfBumps. Acceptable.
			paid, err := h.queries.GetPaidBumps(ctx, req.DeviceHash)
			if err != nil {
				log.Printf("paid bump read failed for %s: %v", req.DeviceHash, err)
				writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
				return
			}
			if paid <= 0 {
				// Out of free AND paid bumps. Deny. Use the dedicated struct
				// so zero counts are not stripped by json:",omitempty".
				writeJSON(w, http.StatusForbidden, outOfBumpsResponse{
					Error:         "out_of_bumps",
					FreeRemaining: 0,
					PaidBalance:   paid, // should be 0, echo for client sync
				})
				return
			}
			if err := h.limiter.ReservePaidBump(ctx, req.DeviceHash, sessionID); err != nil {
				log.Printf("paid bump reserve failed for %s: %v", req.DeviceHash, err)
				writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
				return
			}
			freeRemaining = 0
			paidBalance = paid
		}
	} else {
		// Dev bypass — return sentinel "unlimited" values. No reservation
		// is written; /session/commit for a dev device hash is a no-op.
		freeRemaining = h.freeBumpsPerDay
		paidBalance, _ = h.queries.GetPaidBumps(ctx, req.DeviceHash)
	}

	// Log session for pattern detection (post-enforcement to avoid logging denied attempts as if they succeeded)
	_ = h.queries.LogSession(ctx, req.DeviceHash)

	// Decode device hash to bytes for token payload
	deviceHashBytes, err := hex.DecodeString(req.DeviceHash)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid device_hash encoding"})
		return
	}

	// Sign token
	token, serverTime, err := h.signer.SignToken(deviceHashBytes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "token generation failed"})
		return
	}

	writeJSON(w, http.StatusOK, sessionResponse{
		Token:         base64.StdEncoding.EncodeToString(token),
		ServerTime:    serverTime,
		SessionID:     sessionID,
		FreeRemaining: freeRemaining,
		PaidBalance:   paidBalance,
	})
}
