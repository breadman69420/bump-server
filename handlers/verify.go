package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/androidpublisher/v3"
	"google.golang.org/api/option"

	"github.com/ttalvac/bump-server/cache"
	"github.com/ttalvac/bump-server/db"
	"github.com/ttalvac/bump-server/middleware"
)

const packageName = "me.getbump.app"

// VerifyHandler handles Google Play purchase verification
// using the Google Play Developer API.
type VerifyHandler struct {
	service *androidpublisher.Service
	queries *db.Queries
	limiter *cache.RateLimiter
}

func NewVerifyHandler(serviceAccountJSON string, queries *db.Queries, limiter *cache.RateLimiter) *VerifyHandler {
	h := &VerifyHandler{queries: queries, limiter: limiter}

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

	ctx := r.Context()

	// Rate limit: 5 base × 2 (hard block multiplier in CheckRateLimit) = hard block at 10/hour
	allowed, _, delay := h.limiter.CheckRateLimit(ctx, "verify:"+req.DeviceHash, 5)
	if !allowed {
		writeJSON(w, http.StatusTooManyRequests, errorResponse{Error: "too many requests"})
		return
	}
	if delay > 0 {
		time.Sleep(delay)
	}

	// Replay protection: reject already-verified purchase tokens
	alreadyVerified, err := h.queries.IsPurchaseVerified(ctx, req.PurchaseToken)
	if err != nil {
		log.Printf("Database error checking purchase replay for %s: %v", req.DeviceHash, err)
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
		return
	}
	if alreadyVerified {
		writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
		return
	}

	// If no service account configured, accept for development/testing
	if h.service == nil {
		if err := h.queries.RecordVerifiedPurchase(ctx, req.PurchaseToken, req.DeviceHash, req.ProductID); err != nil {
		log.Printf("Failed to record verified purchase for %s: %v", req.DeviceHash, err)
	}
		writeJSON(w, http.StatusOK, verifyResponse{
			Valid:        true,
			BumpsGranted: 1,
		})
		return
	}

	// Verify with Google Play Developer API
	purchase, err := h.service.Purchases.Products.Get(
		packageName, req.ProductID, req.PurchaseToken,
	).Context(ctx).Do()
	if err != nil {
		log.Printf("Google Play verification error for device %s: %v", req.DeviceHash, err)
		writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
		return
	}

	// PurchaseState: 0 = Purchased, 1 = Canceled, 2 = Pending
	if purchase.PurchaseState != 0 {
		writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
		return
	}

	// Record verified token to prevent replay
	if err := h.queries.RecordVerifiedPurchase(ctx, req.PurchaseToken, req.DeviceHash, req.ProductID); err != nil {
		log.Printf("Failed to record verified purchase for %s: %v", req.DeviceHash, err)
	}

	writeJSON(w, http.StatusOK, verifyResponse{
		Valid:        true,
		BumpsGranted: 1,
	})
}
