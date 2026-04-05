package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ttalvac/bump-server/cache"
	"github.com/ttalvac/bump-server/config"
	"github.com/ttalvac/bump-server/crypto"
	"github.com/ttalvac/bump-server/db"
	"github.com/ttalvac/bump-server/handlers"
	"github.com/ttalvac/bump-server/middleware"
)

func main() {
	cfg := config.Load()

	// Ed25519 signer
	if cfg.Ed25519PrivKey == "" {
		log.Fatal("ED25519_PRIVATE_KEY environment variable is required")
	}
	signer, err := crypto.NewSigner(cfg.Ed25519PrivKey)
	if err != nil {
		log.Fatalf("Failed to init signer: %v", err)
	}

	// Production safety check: without a Google Play service account, the
	// /verify handler accepts every purchase token as valid (credits the
	// device's paid balance without contacting Google Play). This fail-open
	// behavior exists so local development works without a real service
	// account, but it is a launch-blocker if it leaks into production.
	//
	// Rule: if GOOGLE_PLAY_SERVICE_ACCOUNT_JSON is empty, we refuse to start
	// unless BUMP_DEV_MODE=true is explicitly set. Production environments
	// must never set BUMP_DEV_MODE; local dev workflows opt in on purpose.
	if cfg.GooglePlayServiceAcctJSON == "" {
		if !cfg.DevMode {
			log.Fatal(
				"GOOGLE_PLAY_SERVICE_ACCOUNT_JSON is required. " +
					"Without it, /verify will accept any purchase token without contacting Google Play. " +
					"Set the env var, or (local dev only) set BUMP_DEV_MODE=true to override.",
			)
		}
		log.Printf("WARNING: BUMP_DEV_MODE=true and GOOGLE_PLAY_SERVICE_ACCOUNT_JSON is empty — /verify will accept all purchase tokens. DO NOT RUN THIS MODE IN PRODUCTION.")
	}

	// Database
	database, err := db.Connect(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer database.Close()

	if err := db.RunMigrations(database); err != nil {
		log.Fatalf("Failed to run migrations: %v", err)
	}

	queries := db.NewQueries(database)

	// Redis
	limiter, err := cache.NewRateLimiter(cfg.RedisURL)
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	defer limiter.Close()

	// Start session log cleanup goroutine (runs daily)
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			deleted, err := queries.CleanupOldSessions(context.Background())
			if err != nil {
				log.Printf("Session cleanup error: %v", err)
			} else if deleted > 0 {
				log.Printf("Cleaned up %d old session logs", deleted)
			}
		}
	}()

	// Log dev allowlist size at startup so it's obvious if the env var is
	// set wrong. Critically: log the "0 entries" case explicitly so that
	// absence of the log line cannot be confused with a misconfigured
	// allowlist that silently bypasses quotas in production.
	if len(cfg.DevDeviceHashes) > 0 {
		log.Printf("Dev device allowlist active: %d hash(es) bypass daily bump limit", len(cfg.DevDeviceHashes))
	} else {
		log.Printf("Dev device allowlist: 0 entries (production mode)")
	}

	// Log the server's Ed25519 public key in the Kotlin byteArrayOf format
	// used by the Android client at TokenVerifier.kt:71-76. This lets ops
	// verify that the hardcoded client public key matches the server's
	// signing key by comparing a deploy log line against the committed
	// constant — no ssh console, no extra tooling. A mismatch means every
	// BLE session on that deploy will fail signature verification.
	log.Printf("Server public key (Kotlin byteArrayOf format for TokenVerifier.SERVER_PUBLIC_KEY):\n%s", crypto.FormatKotlinByteArray(signer.PublicKeyBytes()))

	// Routes
	mux := http.NewServeMux()
	mux.Handle("/session", handlers.NewSessionHandler(signer, queries, limiter, cfg.MaxSessionsHour, cfg.FreeBumpsPerDay, cfg.DevDeviceHashes))
	mux.Handle("/session/commit", handlers.NewSessionCommitHandler(queries, limiter, cfg.FreeBumpsPerDay, cfg.DevDeviceHashes))
	mux.Handle("/bumps", handlers.NewBumpsHandler(queries, limiter, cfg.FreeBumpsPerDay, cfg.DevDeviceHashes))
	mux.Handle("/config", handlers.NewConfigHandler(queries, cfg.TimeWindowSec, cfg.MinRSSI, cfg.MinAppVersion, cfg.MaxSessionsHour, cfg.KillSwitch))
	mux.Handle("/report", handlers.NewReportHandler(queries, limiter))
	mux.Handle("/verify", handlers.NewVerifyHandler(cfg.GooglePlayServiceAcctJSON, queries, limiter, cfg.FreeBumpsPerDay))

	// Health check for Fly.io
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Static site (landing page + privacy policy)
	mux.HandleFunc("/privacy", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "static/privacy/index.html")
	})
	mux.HandleFunc("/privacy/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "static/privacy/index.html")
	})
	// Public video assets (e.g., permission-declaration demos for Play Console).
	// Served from static/videos/ via http.FileServer so new files can be dropped
	// in without a code change. Scoped tightly to /videos/ so it cannot be used
	// to escape the static directory.
	mux.Handle("/videos/", http.StripPrefix("/videos/", http.FileServer(http.Dir("static/videos"))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "static/index.html")
	})

	// Apply middleware
	handler := middleware.Recovery(middleware.Logger(mux))

	server := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh

		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	log.Printf("Bump server starting on :%s", cfg.Port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
