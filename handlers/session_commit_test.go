package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/ttalvac/bump-server/cache"
)

// fakeCommitQueries satisfies CommitQueries without requiring Postgres.
// paidBalance is mutated by TryConsumePaidBump to model the atomic
// decrement; tests set the initial value and assert the post-state.
type fakeCommitQueries struct {
	paidBalance int
	consumeErr  error
	readErr     error
}

func (f *fakeCommitQueries) GetPaidBumps(_ context.Context, _ string) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	return f.paidBalance, nil
}

func (f *fakeCommitQueries) TryConsumePaidBump(_ context.Context, _ string) (bool, error) {
	if f.consumeErr != nil {
		return false, f.consumeErr
	}
	if f.paidBalance <= 0 {
		return false, nil
	}
	f.paidBalance--
	return true, nil
}

const (
	tDeviceHash = "aabbccdd11223344aabbccdd11223344"
	tSessionID  = "deadbeef00000000deadbeef00000000"
	tFreeLimit  = 3
)

func newTestCommitHandler(t *testing.T, fake *fakeCommitQueries, dev map[string]bool) (*SessionCommitHandler, *cache.RateLimiter) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	// Reuse the package-private RateLimiter constructor by allocating the
	// struct directly — same trick used in cache/redis_test.go.
	limiter := cache.NewRateLimiterForTest(client)
	h := NewSessionCommitHandler(fake, limiter, tFreeLimit, dev)
	return h, limiter
}

func doCommit(t *testing.T, h *SessionCommitHandler, deviceHash, sessionID string) (int, sessionCommitResponse) {
	t.Helper()
	body, _ := json.Marshal(sessionCommitRequest{DeviceHash: deviceHash, SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/session/commit", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var out sessionCommitResponse
	if rr.Code == http.StatusOK {
		_ = json.NewDecoder(rr.Body).Decode(&out)
	}
	return rr.Code, out
}

func TestSessionCommit_FreeSuccess(t *testing.T) {
	h, limiter := newTestCommitHandler(t, &fakeCommitQueries{paidBalance: 0}, nil)
	ctx := context.Background()

	if _, _, err := limiter.TryReserveFreeBump(ctx, tDeviceHash, tSessionID, tFreeLimit); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	code, resp := doCommit(t, h, tDeviceHash, tSessionID)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.FreeRemaining != tFreeLimit-1 {
		t.Fatalf("expected free_remaining=%d, got %d", tFreeLimit-1, resp.FreeRemaining)
	}
	if resp.PaidBalance != 0 {
		t.Fatalf("expected paid_balance=0, got %d", resp.PaidBalance)
	}

	// Daily counter reflects the commit.
	if used, _ := limiter.GetDailyFreeBumpsUsed(ctx, tDeviceHash); used != 1 {
		t.Fatalf("expected daily used=1, got %d", used)
	}
}

func TestSessionCommit_Idempotent(t *testing.T) {
	h, limiter := newTestCommitHandler(t, &fakeCommitQueries{paidBalance: 0}, nil)
	ctx := context.Background()

	if _, _, err := limiter.TryReserveFreeBump(ctx, tDeviceHash, tSessionID, tFreeLimit); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	if code, _ := doCommit(t, h, tDeviceHash, tSessionID); code != http.StatusOK {
		t.Fatalf("first commit: %d", code)
	}
	code, resp := doCommit(t, h, tDeviceHash, tSessionID)
	if code != http.StatusOK {
		t.Fatalf("second commit should be idempotent 200, got %d", code)
	}
	if resp.FreeRemaining != tFreeLimit-1 {
		t.Fatalf("expected free_remaining=%d on replay, got %d", tFreeLimit-1, resp.FreeRemaining)
	}

	// Daily counter must not have double-incremented.
	if used, _ := limiter.GetDailyFreeBumpsUsed(ctx, tDeviceHash); used != 1 {
		t.Fatalf("expected daily used=1 after idempotent replay, got %d", used)
	}
}

func TestSessionCommit_MissingReservation_410(t *testing.T) {
	h, _ := newTestCommitHandler(t, &fakeCommitQueries{}, nil)

	code, _ := doCommit(t, h, tDeviceHash, tSessionID)
	if code != http.StatusGone {
		t.Fatalf("expected 410, got %d", code)
	}
}

func TestSessionCommit_WrongDeviceHash_403(t *testing.T) {
	h, limiter := newTestCommitHandler(t, &fakeCommitQueries{}, nil)
	ctx := context.Background()

	if _, _, err := limiter.TryReserveFreeBump(ctx, tDeviceHash, tSessionID, tFreeLimit); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	other := "ffeeddccbbaa9988ffeeddccbbaa9988"
	code, _ := doCommit(t, h, other, tSessionID)
	if code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", code)
	}
}

func TestSessionCommit_PaidSuccess(t *testing.T) {
	h, limiter := newTestCommitHandler(t, &fakeCommitQueries{paidBalance: 2}, nil)
	ctx := context.Background()

	// Simulate /session having reserved a paid slot.
	if err := limiter.ReservePaidBump(ctx, tDeviceHash, tSessionID); err != nil {
		t.Fatalf("reserve paid: %v", err)
	}

	code, resp := doCommit(t, h, tDeviceHash, tSessionID)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.PaidBalance != 1 {
		t.Fatalf("expected paid_balance=1 post-commit, got %d", resp.PaidBalance)
	}
	if resp.FreeRemaining != tFreeLimit {
		t.Fatalf("expected free_remaining unchanged=%d, got %d", tFreeLimit, resp.FreeRemaining)
	}
}

func TestSessionCommit_PaidZeroBalance_409(t *testing.T) {
	// Reservation marker exists but paid balance has been drained by a
	// concurrent flow or was stale at reserve time.
	h, limiter := newTestCommitHandler(t, &fakeCommitQueries{paidBalance: 0}, nil)
	ctx := context.Background()

	if err := limiter.ReservePaidBump(ctx, tDeviceHash, tSessionID); err != nil {
		t.Fatalf("reserve paid: %v", err)
	}
	code, _ := doCommit(t, h, tDeviceHash, tSessionID)
	if code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", code)
	}
}

func TestSessionCommit_DevBypass(t *testing.T) {
	dev := map[string]bool{tDeviceHash: true}
	// No reservation, no paid balance — dev bypass should still return
	// unlimited-ish counts.
	h, _ := newTestCommitHandler(t, &fakeCommitQueries{paidBalance: 0}, dev)

	code, resp := doCommit(t, h, tDeviceHash, tSessionID)
	if code != http.StatusOK {
		t.Fatalf("expected 200 on dev bypass, got %d", code)
	}
	if resp.FreeRemaining != tFreeLimit {
		t.Fatalf("dev bypass should report full free quota, got %d", resp.FreeRemaining)
	}
}

func TestSessionCommit_InvalidDeviceHash(t *testing.T) {
	h, _ := newTestCommitHandler(t, &fakeCommitQueries{}, nil)
	code, _ := doCommit(t, h, "nothex", tSessionID)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", code)
	}
}

func TestSessionCommit_InvalidSessionID(t *testing.T) {
	h, _ := newTestCommitHandler(t, &fakeCommitQueries{}, nil)
	code, _ := doCommit(t, h, tDeviceHash, "tooshort")
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", code)
	}
}
