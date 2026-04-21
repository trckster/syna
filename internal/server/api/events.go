package api

import (
	"errors"
	"net/http"
	"strconv"

	"syna/internal/common/protocol"
	"syna/internal/server/db"
)

func (a *API) handleEvents(w http.ResponseWriter, r *http.Request, sess *db.Session) {
	switch r.Method {
	case http.MethodGet:
		a.handleEventFetch(w, r, sess)
	case http.MethodPost:
		a.handleEventSubmit(w, r, sess)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
	}
}

func (a *API) handleEventFetch(w http.ResponseWriter, r *http.Request, sess *db.Session) {
	afterSeq, limit, err := a.parseEventFetchQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_query", err.Error())
		return
	}
	events, currentSeq, err := a.db.FetchEvents(sess.WorkspaceID, afterSeq, limit)
	if err != nil {
		var resyncErr *db.ResyncRequiredError
		if errors.As(err, &resyncErr) {
			writeJSON(w, http.StatusGone, protocol.ErrorResponse{
				Code:             "resync_required",
				RetainedFloorSeq: resyncErr.RetainedFloorSeq,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.EventFetchResponse{
		Events:     events,
		CurrentSeq: currentSeq,
	})
}

func (a *API) parseEventFetchQuery(r *http.Request) (int64, int, error) {
	query := r.URL.Query()
	afterSeq := int64(0)
	if rawAfterSeq := query.Get("after_seq"); rawAfterSeq != "" {
		parsed, err := strconv.ParseInt(rawAfterSeq, 10, 64)
		if err != nil || parsed < 0 {
			return 0, 0, errors.New("after_seq must be a non-negative integer")
		}
		afterSeq = parsed
	}

	limit := 100
	if rawLimit := query.Get("limit"); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed <= 0 {
			return 0, 0, errors.New("limit must be a positive integer")
		}
		limit = parsed
	}
	if a.cfg.MaxEventFetchPage > 0 && limit > a.cfg.MaxEventFetchPage {
		limit = a.cfg.MaxEventFetchPage
	}
	return afterSeq, limit, nil
}

func (a *API) handleEventSubmit(w http.ResponseWriter, r *http.Request, sess *db.Session) {
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.MaxEventBodyBytes)
	var req protocol.EventSubmitRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	result, err := a.db.SubmitEvent(sess, req)
	if err != nil {
		var mismatch *db.PathHeadMismatchError
		if errors.As(err, &mismatch) {
			writeJSON(w, http.StatusConflict, protocol.ErrorResponse{
				Code:       "path_head_mismatch",
				CurrentSeq: mismatch.CurrentSeq,
			})
			return
		}
		writeError(w, http.StatusBadRequest, "event_rejected", err.Error())
		return
	}
	a.hub.Publish(sess.WorkspaceID, protocol.WSMessage{
		Type:  "event",
		Event: &result.Event,
	})
	writeJSON(w, http.StatusOK, protocol.EventSubmitResponse{
		AcceptedSeq:  result.AcceptedSeq,
		WorkspaceSeq: result.WorkspaceSeq,
	})
}
