package handlers

import (
	"net/http"

	"github.com/ttalvac/bump-server/cache"
	"github.com/ttalvac/bump-server/db"
	"github.com/ttalvac/bump-server/middleware"
)

// BumpsHandler returns the current bump balance for a device.
// Used by the client on app startup to sync the UI with the
// server's source-of-truth counts.
type BumpsHandler struct {
	queries         *db.Queries
	limiter         *cache.RateLimiter
	freeBumpsPerDay int
	devAllowlist    map[string]bool
}

func NewBumpsHandler(
	queries *db.Queries,
	limiter *cache.RateLimiter,
	freeBumpsPerDay int,
	devAllowlist map[string]bool,
) *BumpsHandler {
	return &BumpsHandler{
		queries:         queries,
		limiter:         limiter,
		freeBumpsPerDay: freeBumpsPerDay,
		devAllowlist:    devAllowlist,
	}
}

type bumpsResponse struct {
	FreeRemaining int `json:"free_remaining"`
	PaidBalance   int `json:"paid_balance"`
}

func (h *BumpsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	deviceHash := r.URL.Query().Get("device_hash")
	if !middleware.IsValidDeviceHash(deviceHash) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid device_hash"})
		return
	}

	ctx := r.Context()

	// Dev devices see full quota regardless of usage
	if h.devAllowlist[deviceHash] {
		paid, _ := h.queries.GetPaidBumps(ctx, deviceHash)
		writeJSON(w, http.StatusOK, bumpsResponse{
			FreeRemaining: h.freeBumpsPerDay,
			PaidBalance:   paid,
		})
		return
	}

	used, err := h.limiter.GetDailyFreeBumpsUsed(ctx, deviceHash)
	if err != nil {
		// Fail open — return max free. Redis hiccups shouldn't lock users out of UI.
		used = 0
	}
	freeRemaining := h.freeBumpsPerDay - used
	if freeRemaining < 0 {
		freeRemaining = 0
	}

	paid, _ := h.queries.GetPaidBumps(ctx, deviceHash)

	writeJSON(w, http.StatusOK, bumpsResponse{
		FreeRemaining: freeRemaining,
		PaidBalance:   paid,
	})
}
