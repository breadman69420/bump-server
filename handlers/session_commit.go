package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/ttalvac/bump-server/cache"
	"github.com/ttalvac/bump-server/middleware"
)

// CommitQueries is the narrow DB surface the commit handler needs. We
// depend on an interface rather than *db.Queries so tests can inject a
// fake for the paid-balance methods without standing up Postgres.
// *db.Queries satisfies this interface structurally.
type CommitQueries interface {
	GetPaidBumps(ctx context.Context, deviceHash string) (int, error)
	TryConsumePaidBump(ctx context.Context, deviceHash string) (bool, error)
}

// SessionCommitHandler turns a reservation from /session into a charged
// bump once the client has confirmed a successful BLE match. The commit
// endpoint exists so failed connections (no peer, handshake error,
// network drop) don't burn a bump — only matches that actually reach
// SessionState.Ready on the client charge the user.
type SessionCommitHandler struct {
	queries         CommitQueries
	limiter         *cache.RateLimiter
	freeBumpsPerDay int
	devAllowlist    map[string]bool
}

func NewSessionCommitHandler(
	queries CommitQueries,
	limiter *cache.RateLimiter,
	freeBumpsPerDay int,
	devAllowlist map[string]bool,
) *SessionCommitHandler {
	return &SessionCommitHandler{
		queries:         queries,
		limiter:         limiter,
		freeBumpsPerDay: freeBumpsPerDay,
		devAllowlist:    devAllowlist,
	}
}

type sessionCommitRequest struct {
	DeviceHash string `json:"device_hash"`
	SessionID  string `json:"session_id"`
}

type sessionCommitResponse struct {
	FreeRemaining int `json:"free_remaining"`
	PaidBalance   int `json:"paid_balance"`
}

func (h *SessionCommitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req sessionCommitRequest
	if err := json.NewDecoder(limitedBody(r)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}
	if !middleware.IsValidDeviceHash(req.DeviceHash) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid device_hash"})
		return
	}
	// Session ID shares the "32-char lowercase hex" format with device
	// hashes, so the same validator covers it.
	if !middleware.IsValidDeviceHash(req.SessionID) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid session_id"})
		return
	}

	ctx := r.Context()

	// Dev bypass — dev devices don't write reservations, so commit is a
	// no-op that reports current (effectively unlimited) counts. Keeps
	// the client flow uniform between dev and production paths.
	if h.devAllowlist[req.DeviceHash] {
		paid, _ := h.queries.GetPaidBumps(ctx, req.DeviceHash)
		writeJSON(w, http.StatusOK, sessionCommitResponse{
			FreeRemaining: h.freeBumpsPerDay,
			PaidBalance:   paid,
		})
		return
	}

	// Look up the reservation to decide the commit path. Absent or
	// expired reservation → 410 Gone so the client can log and move on.
	resOwner, resType, exists, err := h.limiter.GetReservation(ctx, req.SessionID)
	if err != nil {
		log.Printf("get reservation failed for %s: %v", req.SessionID, err)
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
		return
	}
	if !exists {
		writeJSON(w, http.StatusGone, errorResponse{Error: "reservation_expired"})
		return
	}
	// A reservation must belong to the caller. If it doesn't, someone is
	// guessing session ids — reject with 403 and do not leak existence.
	if resOwner != req.DeviceHash {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "reservation_not_owned"})
		return
	}

	switch resType {
	case cache.ReservationFree:
		newUsed, committed, err := h.limiter.CommitFreeBump(ctx, req.DeviceHash, req.SessionID)
		if err != nil {
			log.Printf("commit free bump failed for %s/%s: %v", req.DeviceHash, req.SessionID, err)
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
			return
		}
		if !committed {
			// Lost a race — reservation expired between GetReservation and
			// CommitFreeBump. Treat as 410 so the client logs and moves on.
			writeJSON(w, http.StatusGone, errorResponse{Error: "reservation_expired"})
			return
		}
		paid, _ := h.queries.GetPaidBumps(ctx, req.DeviceHash)
		writeJSON(w, http.StatusOK, sessionCommitResponse{
			FreeRemaining: h.freeBumpsPerDay - newUsed,
			PaidBalance:   paid,
		})

	case cache.ReservationCommittedFree:
		// Idempotent replay: the commit already happened. Report current
		// committed counts without touching any counter.
		used, _ := h.limiter.GetDailyFreeBumpsUsed(ctx, req.DeviceHash)
		paid, _ := h.queries.GetPaidBumps(ctx, req.DeviceHash)
		writeJSON(w, http.StatusOK, sessionCommitResponse{
			FreeRemaining: h.freeBumpsPerDay - used,
			PaidBalance:   paid,
		})

	case cache.ReservationPaid:
		// Serialize concurrent paid commits for the same session id so we
		// can't double-decrement. The lock holder does the DB work; any
		// concurrent caller returns current balances and treats the
		// commit as already-in-flight.
		locked, err := h.limiter.AcquirePaidCommitLock(ctx, req.SessionID)
		if err != nil {
			log.Printf("acquire paid commit lock failed for %s: %v", req.SessionID, err)
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
			return
		}
		if !locked {
			// Another caller is committing right now. Return current
			// balances — by the time our client retries, the real result
			// will be reflected.
			used, _ := h.limiter.GetDailyFreeBumpsUsed(ctx, req.DeviceHash)
			paid, _ := h.queries.GetPaidBumps(ctx, req.DeviceHash)
			writeJSON(w, http.StatusOK, sessionCommitResponse{
				FreeRemaining: h.freeBumpsPerDay - used,
				PaidBalance:   paid,
			})
			return
		}

		// We own the lock. Decrement paid balance atomically in Postgres.
		consumed, err := h.queries.TryConsumePaidBump(ctx, req.DeviceHash)
		if err != nil {
			log.Printf("paid bump consume failed for %s: %v", req.DeviceHash, err)
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "service temporarily unavailable"})
			return
		}
		if !consumed {
			// Balance was 0 by the time we committed — the user had a
			// stale local cache or a concurrent paid commit drained
			// them. Report OutOfBumps-equivalent.
			writeJSON(w, http.StatusConflict, errorResponse{Error: "no_paid_balance"})
			return
		}
		if err := h.limiter.MarkPaidCommitted(ctx, req.DeviceHash, req.SessionID); err != nil {
			// The DB decrement landed but we couldn't write the sentinel.
			// Worst case: a retry within 10s will see the still-active
			// reservation and try to decrement again, returning 409.
			// Log and continue.
			log.Printf("mark paid committed failed for %s: %v", req.SessionID, err)
		}
		used, _ := h.limiter.GetDailyFreeBumpsUsed(ctx, req.DeviceHash)
		paid, _ := h.queries.GetPaidBumps(ctx, req.DeviceHash)
		writeJSON(w, http.StatusOK, sessionCommitResponse{
			FreeRemaining: h.freeBumpsPerDay - used,
			PaidBalance:   paid,
		})

	case cache.ReservationCommittedPaid:
		used, _ := h.limiter.GetDailyFreeBumpsUsed(ctx, req.DeviceHash)
		paid, _ := h.queries.GetPaidBumps(ctx, req.DeviceHash)
		writeJSON(w, http.StatusOK, sessionCommitResponse{
			FreeRemaining: h.freeBumpsPerDay - used,
			PaidBalance:   paid,
		})

	default:
		log.Printf("unknown reservation type %q for %s", resType, req.SessionID)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "invalid reservation"})
	}
}
