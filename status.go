package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/shellsecrets/peekastokk/internal/forward"
)

// newStatusHandler serves the minimal monitoring listener on status-addr:
// /healthz always, /statusz with the forwarder's state when one is
// configured. It exposes no log content, so like /healthz it is
// deliberately unauthenticated.
func newStatusHandler(logger *slog.Logger, fwd *forward.Client) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	if fwd != nil {
		mux.HandleFunc("GET /statusz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			json.NewEncoder(w).Encode(fwd.Status())
		})
	}
	return mux
}
