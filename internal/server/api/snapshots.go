package api

import (
	"net/http"

	"syna/internal/common/protocol"
	"syna/internal/server/db"
)

func (a *API) handleSnapshotSubmit(w http.ResponseWriter, r *http.Request, sess *db.Session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.MaxSnapshotBody)
	var req protocol.SnapshotSubmitRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if err := a.db.SaveSnapshot(sess, req); err != nil {
		writeError(w, http.StatusBadRequest, "snapshot_rejected", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.SnapshotSubmitResponse{
		RootID:   req.RootID,
		BaseSeq:  req.BaseSeq,
		ObjectID: req.ObjectID,
	})
}
