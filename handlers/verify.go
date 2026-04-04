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
	PaidBalance  int  `json:"paid_balance"` // device's total paid balance after this purchase
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

	// If no service account configured, accept for development/testing.
	// Same transactional credit path as the real Google Play branch.
	if h.service == nil {
		balance, ok, err := h.creditPurchase(ctx, req.PurchaseToken, req.DeviceHash, req.ProductID)
		if err != nil {
			log.Printf("Failed to credit dev-mode purchase for %s: %v", req.DeviceHash, err)
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
			return
		}
		if !ok {
			// Duplicate token — already verified by a prior (concurrent) request.
			writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
			return
		}
		writeJSON(w, http.StatusOK, verifyResponse{
			Valid:        true,
			BumpsGranted: 1,
			PaidBalance:  balance,
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

	balance, ok, err := h.creditPurchase(ctx, req.PurchaseToken, req.DeviceHash, req.ProductID)
	if err != nil {
		log.Printf("Failed to credit verified purchase for %s: %v", req.DeviceHash, err)
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
		return
	}
	if !ok {
		// Duplicate — a concurrent /verify beat us to the insert. Don't double-credit.
		writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
		return
	}

	writeJSON(w, http.StatusOK, verifyResponse{
		Valid:        true,
		BumpsGranted: 1,
		PaidBalance:  balance,
	})
}

// creditPurchase atomically records a verified purchase and credits the
// device's paid balance inside a single transaction. Returns:
//
//   - newBalance: the post-increment paid_balance (valid only when ok=true)
//   - ok: true if this call actually recorded+credited the purchase; false
//     if the purchase_token was already in verified_purchases (a concurrent
//     /verify won the race), in which case we must NOT double-credit
//   - err: any database error; if non-nil the transaction rolled back and no
//     state was persisted
//
// Fixes audit findings C1 (double-credit race), H1 (no rollback on
// post-replay-lock failure), and H2 (non-atomic balance read).
func (h *VerifyHandler) creditPurchase(ctx context.Context, purchaseToken, deviceHash, productID string) (int, bool, error) {
	tx, err := h.queries.BeginTx(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback() // no-op after successful Commit

	inserted, err := h.queries.RecordVerifiedPurchaseTx(ctx, tx, purchaseToken, deviceHash, productID)
	if err != nil {
		return 0, false, err
	}
	if !inserted {
		// Already recorded by a concurrent request. Commit (no-op) and let
		// caller surface the duplicate. Critical: do NOT increment.
		if err := tx.Commit(); err != nil {
			return 0, false, err
		}
		return 0, false, nil
	}

	newBalance, err := h.queries.IncrementPaidBumpsTx(ctx, tx, deviceHash, 1)
	if err != nil {
		return 0, false, err
	}

	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return newBalance, true, nil
}
