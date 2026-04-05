package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/ttalvac/bump-server/cache"
	"github.com/ttalvac/bump-server/db"
	"github.com/ttalvac/bump-server/middleware"
)

// autoBlocklistThreshold is the number of distinct reports against a device
// that trigger an automatic entry in the blocklist. Raised from 3 to 5 after
// the pre-launch audit flagged the low threshold as a mass-report abuse
// vector; combined with per-reporter rate limiting (see reportRateLimit
// below) this materially raises the bar for coordinated abuse.
const autoBlocklistThreshold = 5

// reportRateLimit is the soft hourly rate limit applied per reporter_hash.
// The underlying RateLimiter hard-blocks at 2x this value (see
// cache/redis.go). Five per hour is generous for a legitimate user — a
// normal person reports at most a handful of bumps in their entire lifetime
// — while capping how fast a single compromised device can drive reports
// against arbitrary targets.
const reportRateLimit = 5

type ReportHandler struct {
	queries *db.Queries
	limiter *cache.RateLimiter
}

func NewReportHandler(queries *db.Queries, limiter *cache.RateLimiter) *ReportHandler {
	return &ReportHandler{queries: queries, limiter: limiter}
}

type reportRequest struct {
	ReporterHash string `json:"reporter_hash"`
	ReportedHash string `json:"reported_hash"`
	Reason       string `json:"reason"`
}

var validReasons = map[string]bool{
	"harassment": true,
	"spam":       true,
	"safety":     true,
	"other":      true,
}

func (h *ReportHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req reportRequest
	if err := json.NewDecoder(limitedBody(r)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}

	// Validate inputs
	if !middleware.IsValidDeviceHash(req.ReporterHash) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid reporter_hash"})
		return
	}
	if !middleware.IsValidDeviceHash(req.ReportedHash) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid reported_hash"})
		return
	}
	if !validReasons[req.Reason] {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid reason"})
		return
	}
	if req.ReporterHash == req.ReportedHash {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "cannot report self"})
		return
	}

	ctx := r.Context()

	// Per-reporter rate limit. CheckRateLimit is keyed by arbitrary string so
	// we namespace with "report:" to keep report counters independent of the
	// device's /session and /verify rate buckets. Hard block at 2x the soft
	// limit returns 429; between soft and hard the limiter injects a
	// progressive delay. This is the primary defense against a single
	// compromised device (or a small set) mass-reporting to drive arbitrary
	// users past the auto-blocklist threshold.
	allowed, _, delay := h.limiter.CheckRateLimit(ctx, "report:"+req.ReporterHash, reportRateLimit)
	if !allowed {
		writeJSON(w, http.StatusTooManyRequests, errorResponse{Error: "too many reports", RetryAfter: 3600})
		return
	}
	if delay > 0 {
		time.Sleep(delay)
	}

	// Insert report (deduplicated per reporter+reported pair via unique index)
	inserted, err := h.queries.InsertReport(ctx, req.ReporterHash, req.ReportedHash, req.Reason)
	if err != nil {
		log.Printf("insert report failed for %s -> %s: %v", req.ReporterHash, req.ReportedHash, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to submit report"})
		return
	}

	// Check if reported hash has reached the auto-blocklist threshold. We
	// only increment the count for genuinely new reports (inserted==true),
	// so replayed reports from the same reporter can't march the target
	// toward a block. Combined with the per-reporter rate limit above and
	// the unique (reporter_hash, reported_hash) index in schema.sql, a
	// successful auto-block requires at least autoBlocklistThreshold
	// distinct reporters.
	if inserted {
		count, err := h.queries.ReportCount(ctx, req.ReportedHash)
		if err == nil && count >= autoBlocklistThreshold {
			_ = h.queries.AddToBlocklist(ctx, req.ReportedHash)
		}
	}

	writeJSON(w, http.StatusCreated, struct{}{})
}
