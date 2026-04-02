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

// GetDailySessionCount returns the number of sessions today for a device.
// Uses a separate key with midnight expiry.
func (r *RateLimiter) GetDailySessionCount(ctx context.Context, deviceHash string) (int, error) {
	key := "daily:" + deviceHash

	count, err := r.client.Get(ctx, key).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return count, err
}

// IncrementDailySession increments the daily session counter.
func (r *RateLimiter) IncrementDailySession(ctx context.Context, deviceHash string) error {
	key := "daily:" + deviceHash

	pipe := r.client.Pipeline()
	pipe.Incr(ctx, key)

	// Calculate seconds until midnight UTC
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	ttl := midnight.Sub(now)
	pipe.Expire(ctx, key, ttl)

	_, err := pipe.Exec(ctx)
	return err
}

func (r *RateLimiter) Close() error {
	return r.client.Close()
}
