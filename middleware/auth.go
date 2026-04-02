package middleware

import (
	"log"
	"net/http"
	"regexp"
	"time"
)

// validDeviceHash matches a 32-character hex string.
var validDeviceHash = regexp.MustCompile(`^[a-f0-9]{32}$`)

// IsValidDeviceHash checks if a device hash is a 32-char lowercase hex string.
func IsValidDeviceHash(hash string) bool {
	return validDeviceHash.MatchString(hash)
}

// Logger wraps an http.Handler with request logging.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// Recovery recovers from panics and returns 500.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("PANIC: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
