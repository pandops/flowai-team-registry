// Package probe serves /v1/livez and /v1/readyz and nothing else.
package probe

import (
	"context"
	"net/http"
	"time"
)

// Check reports one readiness dependency.
type Check func(ctx context.Context) error

// Handler returns the probe handler. Liveness depends on nothing; readiness
// is 200 only while every check passes. Every other request gets 404.
func Handler(service string, checks ...Check) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/v1/livez":
			write(w, http.StatusOK, `{"status":"ok","service":"`+service+`"}`)
		case "/v1/readyz":
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			for _, c := range checks {
				if err := c(ctx); err != nil {
					write(w, http.StatusServiceUnavailable, `{"status":"unavailable","service":"`+service+`"}`)
					return
				}
			}
			write(w, http.StatusOK, `{"status":"ready","service":"`+service+`"}`)
		default:
			http.NotFound(w, r)
		}
	})
}

func write(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}
