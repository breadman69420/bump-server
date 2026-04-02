package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/ttalvac/bump-server/db"
	"github.com/ttalvac/bump-server/middleware"
)

type ReportHandler struct {
	queries *db.Queries
}

func NewReportHandler(queries *db.Queries) *ReportHandler {
	return &ReportHandler{queries: queries}
}

type reportRequest struct {
	ReporterHash string `json:"reporter_hash"`
	ReportedHash string `json:"reported_hash"`
	Reason       string `json:"reason"`
	Timestamp    int64  `json:"timestamp"`
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

	// Insert report (deduplicated per reporter+reported pair)
	inserted, err := h.queries.InsertReport(ctx, req.ReporterHash, req.ReportedHash, req.Reason)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to submit report"})
		return
	}

	// Check if reported hash has reached threshold (only on new reports)
	if inserted {
		count, err := h.queries.ReportCount(ctx, req.ReportedHash)
		if err == nil && count >= 3 {
			_ = h.queries.AddToBlocklist(ctx, req.ReportedHash)
		}
	}

	writeJSON(w, http.StatusCreated, struct{}{})
}
