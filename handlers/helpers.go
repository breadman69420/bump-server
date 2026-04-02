package handlers

import (
	"encoding/json"
	"io"
	"net/http"
)

// maxRequestBody limits request body size to 1KB to prevent memory exhaustion.
const maxRequestBody = 1024

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// limitedBody returns a reader that limits the request body to maxRequestBody bytes.
func limitedBody(r *http.Request) io.Reader {
	return http.MaxBytesReader(nil, r.Body, maxRequestBody)
}
