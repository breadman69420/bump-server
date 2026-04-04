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

// Lua script: atomically increment daily free-bump counter if below limit.
// KEYS[1] = the daily counter key
// ARGV[1] = limit (free bumps per day)
// ARGV[2] = TTL seconds until midnight UTC
// Returns: new count (>=1 and <=limit) on success, -1 if already at limit.
var consumeFreeBumpScript = redis.NewScript(`
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])

local current = tonumber(redis.call('GET', key) or '0')
if current >= limit then
  return -1
end

local count = redis.call('INCR', key)
if count == 1 then
  redis.call('EXPIRE', key, ttl)
end
return count
`)

// TryConsumeFreeBump atomically consumes one free bump for today if the
// daily limit has not been reached. Returns (newCount, true) on success
// or (currentCount, false) if already at limit.
func (r *RateLimiter) TryConsumeFreeBump(ctx context.Context, deviceHash string, limit int) (int, bool, error) {
	key := "daily:" + deviceHash

	// Calculate seconds until midnight UTC (key resets at midnight)
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	ttl := int(midnight.Sub(now).Seconds())

	result, err := consumeFreeBumpScript.Run(ctx, r.client, []string{key}, limit, ttl).Int()
	if err != nil {
		return 0, false, err
	}
	if result < 0 {
		return limit, false, nil
	}
	return result, true, nil
}

func (r *RateLimiter) Close() error {
	return r.client.Close()
}
