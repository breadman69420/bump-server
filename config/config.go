package config

import (
	"os"
	"strconv"
)

type Config struct {
	Port            string
	DatabaseURL     string
	RedisURL        string
	Ed25519PrivKey  string // base64-encoded private key
	MaxSessionsHour int
	TimeWindowSec   int
	MinRSSI         int
	MinAppVersion   int
	KillSwitch      bool
}

func Load() *Config {
	return &Config{
		Port:            envOrDefault("PORT", "8080"),
		DatabaseURL:     envOrDefault("DATABASE_URL", "postgres://bump:bump@localhost:5432/bump?sslmode=disable"),
		RedisURL:        envOrDefault("REDIS_URL", "redis://localhost:6379"),
		Ed25519PrivKey:  os.Getenv("ED25519_PRIVATE_KEY"),
		MaxSessionsHour: envIntOrDefault("MAX_SESSIONS_HOUR", 10),
		TimeWindowSec:   envIntOrDefault("TIME_WINDOW_SEC", 15),
		MinRSSI:         envIntOrDefault("MIN_RSSI", -75),
		MinAppVersion:   envIntOrDefault("MIN_APP_VERSION", 1),
		KillSwitch:      os.Getenv("KILL_SWITCH") == "true",
	}
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
