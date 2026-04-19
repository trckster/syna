package api

import (
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"

	commoncrypto "syna/internal/common/crypto"
	"syna/internal/server/objectstore"
)

func (a *API) handleObjects(w http.ResponseWriter, r *http.Request) {
	objectID := path.Base(r.URL.Path)
	switch r.Method {
	case http.MethodPut:
		if _, err := a.sessionFromRequest(r); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}
		kind, plainSize, maxUploadBytes, err := a.validateObjectUpload(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "object_upload_failed", err.Error())
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
		created, err := a.store.Put(a.db.SQL, objectID, kind, plainSize, maxUploadBytes, r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "object_upload_failed", err.Error())
			return
		}
		if created {
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	case http.MethodGet:
		if _, err := a.sessionFromRequest(r); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}
		if !objectstore.ValidObjectID(objectID) {
			writeError(w, http.StatusNotFound, "object_not_found", "")
			return
		}
		file, err := a.store.Get(a.db.SQL, objectID)
		if err != nil {
			writeError(w, http.StatusNotFound, "object_not_found", "")
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, file)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
	}
}

func (a *API) validateObjectUpload(r *http.Request) (string, int64, int64, error) {
	kind := r.Header.Get("X-Syna-Object-Kind")
	switch kind {
	case "file_chunk", "snapshot":
	default:
		return "", 0, 0, fmt.Errorf("invalid X-Syna-Object-Kind")
	}
	plainSizeHeader := r.Header.Get("X-Syna-Plain-Size")
	if plainSizeHeader == "" {
		return "", 0, 0, fmt.Errorf("missing X-Syna-Plain-Size")
	}
	plainSize, err := strconv.ParseInt(plainSizeHeader, 10, 64)
	if err != nil || plainSize <= 0 {
		return "", 0, 0, fmt.Errorf("invalid X-Syna-Plain-Size")
	}
	switch kind {
	case "file_chunk":
		if plainSize > a.cfg.MaxPlainChunkSize {
			return "", 0, 0, fmt.Errorf("file chunk exceeds %d bytes", a.cfg.MaxPlainChunkSize)
		}
	case "snapshot":
		if plainSize > a.cfg.MaxSnapshotPlain {
			return "", 0, 0, fmt.Errorf("snapshot exceeds %d bytes", a.cfg.MaxSnapshotPlain)
		}
	}
	maxUploadBytes, err := commoncrypto.EncryptedSize(plainSize)
	if err != nil {
		return "", 0, 0, err
	}
	return kind, plainSize, maxUploadBytes, nil
}
