package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type RateLimiter struct {
	client *redis.Client
}

// NewRateLimiterForTest wraps an existing *redis.Client (e.g. one
// pointed at a miniredis instance) in a RateLimiter. Only intended for
// tests in other packages; production code must use NewRateLimiter.
func NewRateLimiterForTest(client *redis.Client) *RateLimiter {
	return &RateLimiter{client: client}
}

func NewRateLimiter(redisURL string) (*RateLimiter, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return &RateLimiter{client: client}, nil
}

// CheckRateLimit returns (allowed bool, currentCount int, throttleDelay time.Duration).
// Uses a sliding window of 1 hour per device hash.
func (r *RateLimiter) CheckRateLimit(ctx context.Context, deviceHash string, maxPerHour int) (bool, int, time.Duration) {
	key := "rate:" + deviceHash

	count, err := r.client.Incr(ctx, key).Result()
	if err != nil {
		// On Redis error, allow the request (fail open)
		return true, 0, 0
	}

	// Set expiry on first request in the window
	if count == 1 {
		r.client.Expire(ctx, key, time.Hour)
	}

	currentCount := int(count)

	// Hard block above 2x the limit
	if currentCount > maxPerHour*2 {
		return false, currentCount, 0
	}

	// Progressive throttle above the limit
	if currentCount > maxPerHour {
		excess := currentCount - maxPerHour
		delay := time.Duration(excess*5) * time.Second
		return true, currentCount, delay
	}

	return true, currentCount, 0
}

// GetDailyFreeBumpsUsed returns the number of free bumps used today for a device.
// Counter resets at midnight UTC.
func (r *RateLimiter) GetDailyFreeBumpsUsed(ctx context.Context, deviceHash string) (int, error) {
	key := "daily:" + deviceHash
	count, err := r.client.Get(ctx, key).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return count, err
}

// --- Reserve + commit ---
//
// /session no longer atomically charges a bump. It reserves one: a
// short-TTL Redis key is created, the daily counter is NOT incremented,
// and the client is free to attempt the BLE handshake. Only after the
// client confirms a successful match (SessionState.Ready) does it call
// /session/commit, which verifies the reservation and atomically
// increments the counter. Unclaimed reservations expire naturally, so
// failed connections cost nothing.

// secondsUntilMidnightUTC returns the TTL (in seconds) a daily counter
// should carry so it resets at the next UTC midnight. Used by both the
// reserve and commit paths so a reservation created just before midnight
// that commits after midnight still lands in the correct day's counter
// (EXPIRE is re-applied on every commit INCR).
func secondsUntilMidnightUTC() int {
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	return int(midnight.Sub(now).Seconds())
}

// Reservation TTL — a bump reservation held between /session and
// /session/commit. 60s is comfortably longer than the worst-case BLE
// handshake under real-world conditions (scan + match + encrypted
// exchange), and short enough that abandoned reservations can't
// meaningfully fill Redis.
const reservationTTLSeconds = 60

// Committed-sentinel TTL — once a reservation has been committed, we keep
// a short-lived sentinel around so an accidental commit retry (client
// network glitch, background task re-fires) is idempotent instead of
// returning "reservation expired". 10s is well above realistic retry
// windows and still prunes the key quickly.
const committedSentinelTTLSeconds = 10

// Lua script: atomically reserve a free bump slot for today.
// KEYS[1] = daily counter key          (daily:<dh>)
// KEYS[2] = reservation key             (reserve:<sessionId>)
// ARGV[1] = daily free-bump limit
// ARGV[2] = reservation TTL seconds
// ARGV[3] = reservation value (`<deviceHash>|free`)
// Returns: current used count (>=0) on success, -1 if already at limit,
//          -2 if the reservation key already exists (duplicate session id).
//
// NOTE: this does NOT increment the daily counter. That happens at commit
// time. The reserve only gates on "is there room" and plants an intent
// marker so the commit can verify the reservation originated here.
var reserveFreeBumpScript = redis.NewScript(`
local daily = KEYS[1]
local resKey = KEYS[2]
local limit = tonumber(ARGV[1])
local resTTL = tonumber(ARGV[2])
local value = ARGV[3]

local current = tonumber(redis.call('GET', daily) or '0')
if current >= limit then
  return -1
end

-- SET NX guards against duplicate session id replay (a new reservation
-- with a recycled sid would otherwise silently clobber an in-flight one).
local ok = redis.call('SET', resKey, value, 'NX', 'EX', resTTL)
if not ok then
  return -2
end

return current
`)

// TryReserveFreeBump atomically reserves a free-bump slot for today
// without incrementing the daily counter. On success, the returned
// currentUsed is unchanged from the committed state so callers can report
// an honest free_remaining = limit - currentUsed to the client.
//
// Returns (currentUsed, true, nil) on success.
// Returns (limit, false, nil) if the daily limit is already reached.
// Returns (0, false, error) on Redis errors or duplicate session id.
func (r *RateLimiter) TryReserveFreeBump(ctx context.Context, deviceHash, sessionID string, limit int) (int, bool, error) {
	dailyKey := "daily:" + deviceHash
	resKey := "reserve:" + sessionID
	value := deviceHash + "|free"

	result, err := reserveFreeBumpScript.Run(
		ctx, r.client,
		[]string{dailyKey, resKey},
		limit, reservationTTLSeconds, value,
	).Int()
	if err != nil {
		return 0, false, err
	}
	if result == -1 {
		return limit, false, nil
	}
	if result == -2 {
		return 0, false, fmt.Errorf("duplicate session id: %s", sessionID)
	}
	return result, true, nil
}

// ReservePaidBump writes a paid-reservation intent marker. Does NOT check
// the paid balance in Postgres — that check happens at commit time where
// the atomic decrement lives. The reservation exists only to let the
// commit handler verify the session id originated from /session and to
// carry the device hash for the commit lookup.
func (r *RateLimiter) ReservePaidBump(ctx context.Context, deviceHash, sessionID string) error {
	resKey := "reserve:" + sessionID
	value := deviceHash + "|paid"
	ok, err := r.client.SetNX(ctx, resKey, value, time.Duration(reservationTTLSeconds)*time.Second).Result()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("duplicate session id: %s", sessionID)
	}
	return nil
}

// ReservationType identifies how a reservation should be committed.
type ReservationType string

const (
	ReservationFree          ReservationType = "free"
	ReservationPaid          ReservationType = "paid"
	ReservationCommittedFree ReservationType = "committed-free"
	ReservationCommittedPaid ReservationType = "committed-paid"
)

// GetReservation looks up a reservation by session id. Returns
// (deviceHash, type, true, nil) if found, (,"", false, nil) if missing or
// expired, or (,"", false, err) on Redis errors.
func (r *RateLimiter) GetReservation(ctx context.Context, sessionID string) (string, ReservationType, bool, error) {
	resKey := "reserve:" + sessionID
	val, err := r.client.Get(ctx, resKey).Result()
	if err == redis.Nil {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	// Format: `<deviceHash>|<type>`
	for i := 0; i < len(val); i++ {
		if val[i] == '|' {
			return val[:i], ReservationType(val[i+1:]), true, nil
		}
	}
	return "", "", false, fmt.Errorf("malformed reservation value: %q", val)
}

// Lua script: atomically commit a free-bump reservation.
// KEYS[1] = daily counter key          (daily:<dh>)
// KEYS[2] = reservation key             (reserve:<sessionId>)
// ARGV[1] = dailyTTL seconds until midnight UTC
// ARGV[2] = committed sentinel TTL seconds
// ARGV[3] = committed sentinel value   (`<deviceHash>|committed-free`)
// ARGV[4] = expected reservation prefix (`<deviceHash>|free`)
//
// Returns:
//   >= 1  : new daily count after INCR (successful commit)
//   -1    : reservation missing/expired
//   -2    : reservation exists but belongs to a different device or wrong type
//   -3    : reservation already committed; returns existing daily count instead (via separate
//           path — to keep the script simple we return -3 and caller does a follow-up GET)
var commitFreeBumpScript = redis.NewScript(`
local daily = KEYS[1]
local resKey = KEYS[2]
local dailyTTL = tonumber(ARGV[1])
local sentinelTTL = tonumber(ARGV[2])
local sentinelValue = ARGV[3]
local expectedPrefix = ARGV[4]

local res = redis.call('GET', resKey)
if not res then
  return -1
end

-- Idempotency: if the reservation has already been marked committed with
-- the same device hash, return -3 so the caller can fetch the current
-- daily count (the commit has already happened).
local committedPrefix = expectedPrefix:gsub('|free$', '|committed-free')
if res == committedPrefix then
  return -3
end

if res ~= expectedPrefix then
  return -2
end

local count = redis.call('INCR', daily)
redis.call('EXPIRE', daily, dailyTTL)
redis.call('SET', resKey, sentinelValue, 'EX', sentinelTTL)
return count
`)

// CommitFreeBump atomically commits a free-bump reservation. On success
// increments the daily counter and rewrites the reservation to a
// committed sentinel (short TTL) so duplicate commits return the same
// result instead of 410.
//
// Returns:
//   (newUsed int, committed bool, err error)
// Where committed=true means "commit succeeded or was already committed"
// and committed=false means "reservation missing/expired" (caller should
// return 410 Gone) OR "reservation belongs to another device" (caller
// should return 403 — distinguished via GetReservation before calling).
func (r *RateLimiter) CommitFreeBump(ctx context.Context, deviceHash, sessionID string) (int, bool, error) {
	dailyKey := "daily:" + deviceHash
	resKey := "reserve:" + sessionID
	sentinelValue := deviceHash + "|committed-free"
	expectedPrefix := deviceHash + "|free"

	result, err := commitFreeBumpScript.Run(
		ctx, r.client,
		[]string{dailyKey, resKey},
		secondsUntilMidnightUTC(), committedSentinelTTLSeconds,
		sentinelValue, expectedPrefix,
	).Int()
	if err != nil {
		return 0, false, err
	}

	switch {
	case result == -1:
		// Reservation expired or never existed.
		return 0, false, nil
	case result == -2:
		// Reservation exists but is for a different device or wrong type.
		// Caller will have already filtered this via GetReservation; if we
		// land here it's a race — treat as missing.
		return 0, false, nil
	case result == -3:
		// Already committed. Return the current daily count (whatever it
		// happens to be now — the original increment has already landed).
		current, err := r.GetDailyFreeBumpsUsed(ctx, deviceHash)
		return current, true, err
	default:
		return result, true, nil
	}
}

// AcquirePaidCommitLock attempts to claim an exclusive lock on a paid
// reservation's commit path. Used to serialize the "check paid balance +
// decrement in Postgres + mark reservation committed" sequence so
// concurrent commits for the same session id can't double-decrement.
// Returns true if the caller owns the lock; false if another caller
// already owns it (caller should treat this as "already in flight" and
// return current balances).
func (r *RateLimiter) AcquirePaidCommitLock(ctx context.Context, sessionID string) (bool, error) {
	lockKey := "commitlock:" + sessionID
	return r.client.SetNX(ctx, lockKey, "1", 30*time.Second).Result()
}

// MarkPaidCommitted rewrites a paid reservation to the committed-paid
// sentinel so idempotent replays see "already committed" and skip the DB
// decrement.
func (r *RateLimiter) MarkPaidCommitted(ctx context.Context, deviceHash, sessionID string) error {
	resKey := "reserve:" + sessionID
	sentinelValue := deviceHash + "|committed-paid"
	return r.client.Set(ctx, resKey, sentinelValue, time.Duration(committedSentinelTTLSeconds)*time.Second).Err()
}

// DeleteReservation removes a reservation. Used only on rare explicit
// abort paths; natural TTL expiry is the normal cleanup.
func (r *RateLimiter) DeleteReservation(ctx context.Context, sessionID string) error {
	return r.client.Del(ctx, "reserve:"+sessionID).Err()
}

func (r *RateLimiter) Close() error {
	return r.client.Close()
}
