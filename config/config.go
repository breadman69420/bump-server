package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port                      string
	DatabaseURL               string
	RedisURL                  string
	Ed25519PrivKey            string // base64-encoded private key
	MaxSessionsHour           int
	FreeBumpsPerDay           int
	TimeWindowSec             int
	MinRSSI                   int
	MinAppVersion             int
	KillSwitch                bool
	GooglePlayServiceAcctJSON string          // JSON credentials for Google Play Developer API
	DevDeviceHashes           map[string]bool // allowlist of dev device hashes that bypass daily limits
	// DevMode, when true, disables the production safety check that refuses
	// to start without GOOGLE_PLAY_SERVICE_ACCOUNT_JSON. Set explicitly via
	// BUMP_DEV_MODE=true for local development only. In production this
	// MUST be unset; if it is set in production every /verify request will
	// credit paid bumps without contacting Google Play.
	DevMode bool
}

func Load() *Config {
	return &Config{
		Port:                      envOrDefault("PORT", "8080"),
		DatabaseURL:               envOrDefault("DATABASE_URL", "postgres://bump:bump@localhost:5432/bump?sslmode=disable"),
		RedisURL:                  envOrDefault("REDIS_URL", "redis://localhost:6379"),
		Ed25519PrivKey:            os.Getenv("ED25519_PRIVATE_KEY"),
		MaxSessionsHour:           envIntOrDefault("MAX_SESSIONS_HOUR", 10),
		FreeBumpsPerDay:           envIntOrDefault("FREE_BUMPS_PER_DAY", 3),
		TimeWindowSec:             envIntOrDefault("TIME_WINDOW_SEC", 15),
		MinRSSI:                   envIntOrDefault("MIN_RSSI", -75),
		MinAppVersion:             envIntOrDefault("MIN_APP_VERSION", 1),
		KillSwitch:                os.Getenv("KILL_SWITCH") == "true",
		GooglePlayServiceAcctJSON: os.Getenv("GOOGLE_PLAY_SERVICE_ACCOUNT_JSON"),
		DevDeviceHashes:           parseDeviceHashList(os.Getenv("BUMP_DEV_DEVICE_HASHES")),
		DevMode:                   os.Getenv("BUMP_DEV_MODE") == "true",
	}
}

// parseDeviceHashList parses a comma-separated list of device hashes.
// Used for BUMP_DEV_DEVICE_HASHES — devices that bypass the daily
// bump limit so developers can test without exhausting quotas.
func parseDeviceHashList(raw string) map[string]bool {
	result := make(map[string]bool)
	for _, h := range strings.Split(raw, ",") {
		h = strings.TrimSpace(h)
		if h != "" {
			result[h] = true
		}
	}
	return result
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
