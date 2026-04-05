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

// Recovery recovers from panics and returns 500. We log the request method
// and path alongside the panic value so a post-mortem can pinpoint which
// handler blew up without needing a stack trace (which we intentionally
// don't expose — %v not %+v — to avoid leaking internal paths to
// operators sharing log excerpts).
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("PANIC: %s %s: %v", r.Method, r.URL.Path, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
