package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestLimiter wires a RateLimiter against an in-memory miniredis
// instance so tests can run without a real redis server. miniredis
// supports Lua EVAL well enough for the scripts used by the reserve and
// commit paths.
func newTestLimiter(t *testing.T) (*RateLimiter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return &RateLimiter{client: client}, mr
}

const (
	testDeviceHash = "aabbccdd11223344aabbccdd11223344"
	testSessionID  = "deadbeef00000000deadbeef00000000"
	testLimit      = 3
)

func TestTryReserveFreeBump_UnderLimit(t *testing.T) {
	r, _ := newTestLimiter(t)
	ctx := context.Background()

	used, ok, err := r.TryReserveFreeBump(ctx, testDeviceHash, testSessionID, testLimit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("expected reservation to succeed")
	}
	if used != 0 {
		t.Fatalf("expected currentUsed=0 (no commits yet), got %d", used)
	}

	// Daily counter must NOT have been incremented — only on commit.
	if got, _ := r.GetDailyFreeBumpsUsed(ctx, testDeviceHash); got != 0 {
		t.Fatalf("daily counter should still be 0 after reserve, got %d", got)
	}
}

func TestTryReserveFreeBump_AtLimit(t *testing.T) {
	r, _ := newTestLimiter(t)
	ctx := context.Background()

	// Prime the daily counter at the limit so a fresh reserve is denied.
	r.client.Set(ctx, "daily:"+testDeviceHash, testLimit, 0)

	used, ok, err := r.TryReserveFreeBump(ctx, testDeviceHash, testSessionID, testLimit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("expected reservation to be denied at limit")
	}
	if used != testLimit {
		t.Fatalf("expected used=%d, got %d", testLimit, used)
	}
}

func TestTryReserveFreeBump_DuplicateSessionID(t *testing.T) {
	r, _ := newTestLimiter(t)
	ctx := context.Background()

	if _, _, err := r.TryReserveFreeBump(ctx, testDeviceHash, testSessionID, testLimit); err != nil {
		t.Fatalf("first reserve failed: %v", err)
	}
	// Reuse of the same session id is rejected (NX guard against sid
	// replay). The error is intentional — callers never generate
	// duplicate sids under normal flow.
	_, _, err := r.TryReserveFreeBump(ctx, testDeviceHash, testSessionID, testLimit)
	if err == nil {
		t.Fatalf("expected duplicate session id to error")
	}
}

func TestCommitFreeBump_Success(t *testing.T) {
	r, _ := newTestLimiter(t)
	ctx := context.Background()

	if _, _, err := r.TryReserveFreeBump(ctx, testDeviceHash, testSessionID, testLimit); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	newUsed, committed, err := r.CommitFreeBump(ctx, testDeviceHash, testSessionID)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !committed {
		t.Fatalf("expected committed=true")
	}
	if newUsed != 1 {
		t.Fatalf("expected newUsed=1, got %d", newUsed)
	}

	// Daily counter has incremented now.
	if got, _ := r.GetDailyFreeBumpsUsed(ctx, testDeviceHash); got != 1 {
		t.Fatalf("expected daily counter=1 after commit, got %d", got)
	}
}

func TestCommitFreeBump_Idempotent(t *testing.T) {
	r, _ := newTestLimiter(t)
	ctx := context.Background()

	if _, _, err := r.TryReserveFreeBump(ctx, testDeviceHash, testSessionID, testLimit); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, _, err := r.CommitFreeBump(ctx, testDeviceHash, testSessionID); err != nil {
		t.Fatalf("first commit: %v", err)
	}

	// Second commit must NOT double-increment.
	newUsed, committed, err := r.CommitFreeBump(ctx, testDeviceHash, testSessionID)
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if !committed {
		t.Fatalf("expected committed=true on idempotent replay")
	}
	if newUsed != 1 {
		t.Fatalf("expected daily count to stay at 1, got %d", newUsed)
	}
}

func TestCommitFreeBump_Expired(t *testing.T) {
	r, mr := newTestLimiter(t)
	ctx := context.Background()

	if _, _, err := r.TryReserveFreeBump(ctx, testDeviceHash, testSessionID, testLimit); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// Fast-forward past the 60s reservation TTL.
	mr.FastForward(time.Duration(reservationTTLSeconds+1) * time.Second)

	_, committed, err := r.CommitFreeBump(ctx, testDeviceHash, testSessionID)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if committed {
		t.Fatalf("expected committed=false for expired reservation")
	}
	// Counter must not have advanced.
	if got, _ := r.GetDailyFreeBumpsUsed(ctx, testDeviceHash); got != 0 {
		t.Fatalf("expired reservation should not increment daily counter, got %d", got)
	}
}

func TestCommitFreeBump_WrongDeviceHash(t *testing.T) {
	r, _ := newTestLimiter(t)
	ctx := context.Background()

	if _, _, err := r.TryReserveFreeBump(ctx, testDeviceHash, testSessionID, testLimit); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// A commit call for the same session id but a different device hash
	// lands in the -2 branch inside the Lua script (expectedPrefix
	// mismatch). CommitFreeBump maps both -1 and -2 to committed=false
	// so the handler is forced to resolve ownership via GetReservation
	// first — which is exactly what session_commit.go does.
	other := "ffeeddccbbaa9988ffeeddccbbaa9988"
	_, committed, err := r.CommitFreeBump(ctx, other, testSessionID)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if committed {
		t.Fatalf("expected committed=false for wrong device hash")
	}
}

func TestGetReservation_FreeExists(t *testing.T) {
	r, _ := newTestLimiter(t)
	ctx := context.Background()

	if _, _, err := r.TryReserveFreeBump(ctx, testDeviceHash, testSessionID, testLimit); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	owner, resType, exists, err := r.GetReservation(ctx, testSessionID)
	if err != nil {
		t.Fatalf("get reservation: %v", err)
	}
	if !exists {
		t.Fatalf("expected reservation to exist")
	}
	if owner != testDeviceHash {
		t.Fatalf("expected owner=%s, got %s", testDeviceHash, owner)
	}
	if resType != ReservationFree {
		t.Fatalf("expected type=free, got %s", resType)
	}
}

func TestGetReservation_Missing(t *testing.T) {
	r, _ := newTestLimiter(t)
	ctx := context.Background()

	_, _, exists, err := r.GetReservation(ctx, testSessionID)
	if err != nil {
		t.Fatalf("get reservation: %v", err)
	}
	if exists {
		t.Fatalf("expected missing reservation")
	}
}

func TestReservePaidBump_And_GetReservation(t *testing.T) {
	r, _ := newTestLimiter(t)
	ctx := context.Background()

	if err := r.ReservePaidBump(ctx, testDeviceHash, testSessionID); err != nil {
		t.Fatalf("reserve paid: %v", err)
	}
	owner, resType, exists, err := r.GetReservation(ctx, testSessionID)
	if err != nil || !exists {
		t.Fatalf("paid reservation missing: %v", err)
	}
	if owner != testDeviceHash {
		t.Fatalf("owner mismatch: %s", owner)
	}
	if resType != ReservationPaid {
		t.Fatalf("type mismatch: %s", resType)
	}
}

func TestAcquirePaidCommitLock_Serializes(t *testing.T) {
	r, _ := newTestLimiter(t)
	ctx := context.Background()

	gotA, err := r.AcquirePaidCommitLock(ctx, testSessionID)
	if err != nil || !gotA {
		t.Fatalf("first lock should succeed: got=%v err=%v", gotA, err)
	}
	gotB, err := r.AcquirePaidCommitLock(ctx, testSessionID)
	if err != nil {
		t.Fatalf("second lock err: %v", err)
	}
	if gotB {
		t.Fatalf("second concurrent lock should fail (lock already held)")
	}
}

func TestMarkPaidCommitted_Idempotency(t *testing.T) {
	r, _ := newTestLimiter(t)
	ctx := context.Background()

	if err := r.ReservePaidBump(ctx, testDeviceHash, testSessionID); err != nil {
		t.Fatalf("reserve paid: %v", err)
	}
	if err := r.MarkPaidCommitted(ctx, testDeviceHash, testSessionID); err != nil {
		t.Fatalf("mark committed: %v", err)
	}
	_, resType, exists, err := r.GetReservation(ctx, testSessionID)
	if err != nil || !exists {
		t.Fatalf("reservation gone after mark: %v", err)
	}
	if resType != ReservationCommittedPaid {
		t.Fatalf("expected committed-paid sentinel, got %s", resType)
	}
}

func TestReservationAutoExpiry(t *testing.T) {
	r, mr := newTestLimiter(t)
	ctx := context.Background()

	if _, _, err := r.TryReserveFreeBump(ctx, testDeviceHash, testSessionID, testLimit); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	mr.FastForward(time.Duration(reservationTTLSeconds+1) * time.Second)
	_, _, exists, _ := r.GetReservation(ctx, testSessionID)
	if exists {
		t.Fatalf("reservation should have expired")
	}
}
