package api

import (
	"net/http"

	"syna/internal/common/protocol"
)

func (a *API) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, protocol.HealthResponse{
		OK:           true,
		Capabilities: []string{protocol.CapabilityRootIncarnationBinding},
	})
}

func (a *API) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := a.db.SQL.PingContext(r.Context()); err != nil {
		a.writeLoggedError(w, http.StatusServiceUnavailable, "db_unavailable", "database unavailable", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
