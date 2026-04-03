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

	// Routes
	mux := http.NewServeMux()
	mux.Handle("/session", handlers.NewSessionHandler(signer, queries, limiter, cfg.MaxSessionsHour))
	mux.Handle("/config", handlers.NewConfigHandler(queries, cfg.TimeWindowSec, cfg.MinRSSI, cfg.MinAppVersion, cfg.MaxSessionsHour, cfg.KillSwitch))
	mux.Handle("/report", handlers.NewReportHandler(queries))
	mux.Handle("/verify", handlers.NewVerifyHandler(cfg.GooglePlayServiceAcctJSON))

	// Health check for Fly.io
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
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
