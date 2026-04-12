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
	service         *androidpublisher.Service
	queries         *db.Queries
	limiter         *cache.RateLimiter
	freeBumpsPerDay int
}

func NewVerifyHandler(serviceAccountJSON string, queries *db.Queries, limiter *cache.RateLimiter, freeBumpsPerDay int) *VerifyHandler {
	h := &VerifyHandler{queries: queries, limiter: limiter, freeBumpsPerDay: freeBumpsPerDay}

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
	Platform      string `json:"platform"` // "google" (default) or "apple"
}

type verifyResponse struct {
	Valid         bool `json:"valid"`
	BumpsGranted  int  `json:"bumps_granted"`
	PaidBalance   int  `json:"paid_balance"`   // device's total paid balance after this purchase
	FreeRemaining int  `json:"free_remaining"` // free bumps left today, for client UI sync
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

	// Route to platform-specific verification
	if req.Platform == "apple" {
		h.verifyApple(w, r, ctx, req)
	} else {
		h.verifyGoogle(w, r, ctx, req)
	}
}

// verifyGoogle handles Google Play purchase verification via the Play Developer API.
func (h *VerifyHandler) verifyGoogle(w http.ResponseWriter, r *http.Request, ctx context.Context, req verifyRequest) {
	// If no service account configured, accept for development/testing.
	if h.service == nil {
		balance, ok, err := h.creditPurchase(ctx, req.PurchaseToken, req.DeviceHash, req.ProductID, "google")
		if err != nil {
			log.Printf("Failed to credit dev-mode purchase for %s: %v", req.DeviceHash, err)
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
			return
		}
		if !ok {
			writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
			return
		}
		writeJSON(w, http.StatusOK, verifyResponse{
			Valid:         true,
			BumpsGranted:  1,
			PaidBalance:   balance,
			FreeRemaining: h.computeFreeRemaining(ctx, req.DeviceHash),
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

	balance, ok, err := h.creditPurchase(ctx, req.PurchaseToken, req.DeviceHash, req.ProductID, "google")
	if err != nil {
		log.Printf("Failed to credit verified purchase for %s: %v", req.DeviceHash, err)
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
		return
	}

	writeJSON(w, http.StatusOK, verifyResponse{
		Valid:         true,
		BumpsGranted:  1,
		PaidBalance:   balance,
		FreeRemaining: h.computeFreeRemaining(ctx, req.DeviceHash),
	})
}

// verifyApple handles Apple App Store purchase verification.
// StoreKit 2 provides the transaction as signed JSON. The iOS client sends
// the transaction's JSON representation as the purchase_token. We parse it
// to extract the transactionId (for replay protection) and productId.
//
// For full production security, this should validate the JWS signature against
// Apple's root certificate chain. For now, we rely on:
// 1. The client-side StoreKit 2 verification (Apple signs the transaction locally)
// 2. Our server-side replay protection (transactionId uniqueness in DB)
// 3. The iOS app being distributed via App Store (not sideloaded)
func (h *VerifyHandler) verifyApple(w http.ResponseWriter, r *http.Request, ctx context.Context, req verifyRequest) {
	// Parse the transaction JSON to extract transactionId and productId
	var txn appleTransaction
	if err := json.Unmarshal([]byte(req.PurchaseToken), &txn); err != nil {
		log.Printf("Apple verify: failed to parse transaction JSON for %s: %v", req.DeviceHash, err)
		writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
		return
	}

	if txn.TransactionID == "" {
		writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
		return
	}

	if txn.ProductID != "bump_single" {
		writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
		return
	}

	// Use transactionId as the replay-protection key (prefixed to avoid collision
	// with Google purchase tokens which are opaque strings)
	replayKey := "apple:" + txn.TransactionID

	balance, ok, err := h.creditPurchase(ctx, replayKey, req.DeviceHash, req.ProductID, "apple")
	if err != nil {
		log.Printf("Failed to credit Apple purchase for %s: %v", req.DeviceHash, err)
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
		return
	}
	if !ok {
		// Duplicate transactionId — already credited
		writeJSON(w, http.StatusOK, verifyResponse{Valid: false, BumpsGranted: 0})
		return
	}

	writeJSON(w, http.StatusOK, verifyResponse{
		Valid:         true,
		BumpsGranted:  1,
		PaidBalance:   balance,
		FreeRemaining: h.computeFreeRemaining(ctx, req.DeviceHash),
	})
}

// appleTransaction represents the relevant fields from a StoreKit 2
// transaction's JSON representation.
type appleTransaction struct {
	TransactionID string `json:"transactionId"`
	ProductID     string `json:"productId"`
	// Other fields (bundleId, purchaseDate, environment, etc.) can be added
	// when full Apple signature verification is implemented.
}

// computeFreeRemaining returns today's remaining free bumps for a device, or
// 0 on any Redis error. We intentionally swallow the error (and log it) rather
// than failing the verify response — the purchase has already committed, so
// the client should still be told about its new paid balance even if we can't
// report the free count accurately.
func (h *VerifyHandler) computeFreeRemaining(ctx context.Context, deviceHash string) int {
	used, err := h.limiter.GetDailyFreeBumpsUsed(ctx, deviceHash)
	if err != nil {
		log.Printf("verify: failed to read daily free count for %s: %v", deviceHash, err)
		return 0
	}
	remaining := h.freeBumpsPerDay - used
	if remaining < 0 {
		remaining = 0
	}
	return remaining
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
func (h *VerifyHandler) creditPurchase(ctx context.Context, purchaseToken, deviceHash, productID, platform string) (int, bool, error) {
	tx, err := h.queries.BeginTx(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback() // no-op after successful Commit

	inserted, err := h.queries.RecordVerifiedPurchaseTx(ctx, tx, purchaseToken, deviceHash, productID, platform)
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
