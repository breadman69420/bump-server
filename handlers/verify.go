package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/ttalvac/bump-server/middleware"
)

// VerifyHandler handles Google Play purchase verification.
// In production, this calls the Google Play Developer API.
// For now, it accepts and validates the request structure,
// returning a success response for testing.
type VerifyHandler struct{}

func NewVerifyHandler() *VerifyHandler {
	return &VerifyHandler{}
}

type verifyRequest struct {
	PurchaseToken string `json:"purchase_token"`
	ProductID     string `json:"product_id"`
	DeviceHash    string `json:"device_hash"`
}

type verifyResponse struct {
	Valid        bool `json:"valid"`
	BumpsGranted int  `json:"bumps_granted"`
}

func (h *VerifyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req verifyRequest
	if err := json.NewDecoder(limitedBody(r)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}

	// Validate inputs
	if !middleware.IsValidDeviceHash(req.DeviceHash) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid device_hash"})
		return
	}
	if req.PurchaseToken == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing purchase_token"})
		return
	}
	if req.ProductID != "bump_single" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid product_id"})
		return
	}

	// TODO: Phase 5 - Call Google Play Developer API to verify purchase:
	// POST https://androidpublisher.googleapis.com/androidpublisher/v3/applications/{packageName}/purchases/products/{productId}/tokens/{token}
	// For now, assume valid for development/testing.

	writeJSON(w, http.StatusOK, verifyResponse{
		Valid:        true,
		BumpsGranted: 1,
	})
}
