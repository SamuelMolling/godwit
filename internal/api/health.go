package api

import (
	"context"
	"net/http"
	"time"
)

const readyTimeout = 2 * time.Second

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func readyz(ping func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := ping(ctx); err != nil {
			http.Error(w, "store unavailable: "+err.Error(), http.StatusServiceUnavailable)

			return
		}
		_, _ = w.Write([]byte("ok\n"))
	}
}
