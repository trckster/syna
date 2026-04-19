package api

import (
	"net/http"

	"syna/internal/common/protocol"
	"syna/internal/server/db"
)

func (a *API) handleBootstrap(w http.ResponseWriter, r *http.Request, sess *db.Session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	roots, err := a.db.ActiveRoots(sess.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	currentSeq, err := a.db.CurrentSeq(sess.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	resp := protocol.BootstrapResponse{
		WorkspaceID:       sess.WorkspaceID,
		CurrentSeq:        currentSeq,
		BootstrapAfterSeq: currentSeq,
	}
	if len(roots) > 0 {
		var minSet bool
		for _, root := range roots {
			item := protocol.BootstrapRoot{
				RootID:         root.RootID,
				Kind:           root.Kind,
				DescriptorBlob: string(root.DescriptorBlob),
				CreatedSeq:     root.CreatedSeq,
			}
			if root.RemovedSeq.Valid {
				seq := root.RemovedSeq.Int64
				item.RemovedSeq = &seq
			}
			if root.LatestSnapshotObjectID.Valid {
				item.LatestSnapshotObjectID = root.LatestSnapshotObjectID.String
			}
			if root.LatestSnapshotSeq.Valid {
				item.LatestSnapshotSeq = root.LatestSnapshotSeq.Int64
			}
			resp.Roots = append(resp.Roots, item)

			contrib := root.CreatedSeq - 1
			if root.LatestSnapshotSeq.Valid {
				contrib = root.LatestSnapshotSeq.Int64
			}
			if !minSet || contrib < resp.BootstrapAfterSeq {
				resp.BootstrapAfterSeq = contrib
				minSet = true
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
