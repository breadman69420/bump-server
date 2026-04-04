package handlers

import (
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
	FreeRemaining int    `json:"free_remaining"` // free bumps left today after consuming this one
	PaidBalance   int    `json:"paid_balance"`   // paid bumps left after consuming this one
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

	// Rate limiting (hourly request cap, independent of daily bump quota)
	allowed, _, delay := h.limiter.CheckRateLimit(ctx, req.DeviceHash, h.maxHour)
	if !allowed {
		writeJSON(w, http.StatusTooManyRequests, errorResponse{RetryAfter: 60})
		return
	}

	// Progressive throttle
	if delay > 0 {
		time.Sleep(delay)
	}

	// ---- Daily bump enforcement ----
	// Dev devices bypass all quota checks so we can test freely in prod.
	isDev := h.devAllowlist[req.DeviceHash]

	var freeRemaining, paidBalance int
	if !isDev {
		// Try to consume a free bump first.
		freeUsed, freeOk, err := h.limiter.TryConsumeFreeBump(ctx, req.DeviceHash, h.freeBumpsPerDay)
		if err != nil {
			log.Printf("free bump check failed for %s: %v", req.DeviceHash, err)
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
			return
		}

		if freeOk {
			// Successfully consumed a free bump.
			freeRemaining = h.freeBumpsPerDay - freeUsed
			paidBalance, _ = h.queries.GetPaidBumps(ctx, req.DeviceHash)
		} else {
			// No free bumps left — try paid balance.
			consumed, err := h.queries.TryConsumePaidBump(ctx, req.DeviceHash)
			if err != nil {
				log.Printf("paid bump consume failed for %s: %v", req.DeviceHash, err)
				writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
				return
			}
			if !consumed {
				// Out of free AND paid bumps. Deny. Use the dedicated struct
				// so zero counts are not stripped by json:",omitempty".
				paid, _ := h.queries.GetPaidBumps(ctx, req.DeviceHash)
				writeJSON(w, http.StatusForbidden, outOfBumpsResponse{
					Error:         "out_of_bumps",
					FreeRemaining: 0,
					PaidBalance:   paid, // should be 0, echo for client sync
				})
				return
			}
			// Paid consumed successfully.
			freeRemaining = 0
			paidBalance, _ = h.queries.GetPaidBumps(ctx, req.DeviceHash)
		}
	} else {
		// Dev bypass — return sentinel "unlimited" values.
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
		FreeRemaining: freeRemaining,
		PaidBalance:   paidBalance,
	})
}
