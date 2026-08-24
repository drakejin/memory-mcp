package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Envelope is the mandatory response shape for every endpoint (§7):
// {success, data, error}.
type Envelope struct {
	Success bool   `json:"success"`
	Data    any    `json:"data"`
	Error   string `json:"error,omitempty"`
}

// writeJSON emits a success envelope. Scaffold-owned; use from every handler.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(Envelope{Success: true, Data: data}); err != nil {
		slog.Error("respond: encode failed", "error", err)
	}
}

// writeError emits a failure envelope. Messages must be user-facing and never
// leak internals; log details server-side instead.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(Envelope{Success: false, Error: message}); err != nil {
		slog.Error("respond: encode failed", "error", err)
	}
}

// notImplemented is the shared stub body for handlers awaiting module agents.
func notImplemented(w http.ResponseWriter) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}
