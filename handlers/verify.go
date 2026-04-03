package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/androidpublisher/v3"
	"google.golang.org/api/option"

	"github.com/ttalvac/bump-server/middleware"
)

const packageName = "me.getbump.app"

// VerifyHandler handles Google Play purchase verification
// using the Google Play Developer API.
type VerifyHandler struct {
	service *androidpublisher.Service
}

func NewVerifyHandler(serviceAccountJSON string) *VerifyHandler {
	h := &VerifyHandler{}

	if serviceAccountJSON != "" {
		conf, err := google.JWTConfigFromJSON(
			[]byte(serviceAccountJSON),
			androidpublisher.AndroidpublisherScope,
		)
		if err != nil {
			log.Printf("WARN: Failed to parse service account JSON: %v", err)
			return h
		}

		client := conf.Client(context.Background())
		svc, err := androidpublisher.NewService(context.Background(), option.WithHTTPClient(client))
		if err != nil {
			log.Printf("WARN: Failed to create Android Publisher service: %v", err)
			return h
		}
		h.service = svc
	}

	return h
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

	// If no service account configured, accept for development/testing
	if h.service == nil {
		writeJSON(w, http.StatusOK, verifyResponse{
			Valid:        true,
			BumpsGranted: 1,
		})
		return
	}

	// Verify with Google Play Developer API
	purchase, err := h.service.Purchases.Products.Get(
		packageName, req.ProductID, req.PurchaseToken,
	).Context(r.Context()).Do()
	if err != nil {
		log.Printf("Google Play verification failed: %v", err)
		writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
		return
	}

	// PurchaseState: 0 = Purchased, 1 = Canceled, 2 = Pending
	if purchase.PurchaseState != 0 {
		writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
		return
	}

	writeJSON(w, http.StatusOK, verifyResponse{
		Valid:        true,
		BumpsGranted: 1,
	})
}
