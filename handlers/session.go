package handlers

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/ttalvac/bump-server/cache"
	"github.com/ttalvac/bump-server/crypto"
	"github.com/ttalvac/bump-server/db"
	"github.com/ttalvac/bump-server/middleware"
)

type SessionHandler struct {
	signer  *crypto.Signer
	queries *db.Queries
	limiter *cache.RateLimiter
	maxHour int
}

func NewSessionHandler(signer *crypto.Signer, queries *db.Queries, limiter *cache.RateLimiter, maxHour int) *SessionHandler {
	return &SessionHandler{
		signer:  signer,
		queries: queries,
		limiter: limiter,
		maxHour: maxHour,
	}
}

type sessionRequest struct {
	DeviceHash     string `json:"device_hash"`
	ClientTime     int64  `json:"client_time"`
	IntegrityToken string `json:"integrity_token"`
}

type sessionResponse struct {
	Token      string `json:"token"`
	ServerTime int64  `json:"server_time"`
}

type errorResponse struct {
	Error      string `json:"error,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
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

	// Rate limiting
	allowed, count, delay := h.limiter.CheckRateLimit(ctx, req.DeviceHash, h.maxHour)
	if !allowed {
		writeJSON(w, http.StatusTooManyRequests, errorResponse{RetryAfter: 60})
		return
	}

	// Progressive throttle
	if delay > 0 {
		time.Sleep(delay)
	}

	// Log session for pattern detection
	_ = h.queries.LogSession(ctx, req.DeviceHash)

	// Increment daily counter
	_ = h.limiter.IncrementDailySession(ctx, req.DeviceHash)

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

	_ = count // tracked internally

	writeJSON(w, http.StatusOK, sessionResponse{
		Token:      base64.StdEncoding.EncodeToString(token),
		ServerTime: serverTime,
	})
}
