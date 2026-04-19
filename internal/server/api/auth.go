package api

import (
	"fmt"
	"net/http"
	"strings"

	"syna/internal/common/protocol"
	"syna/internal/server/db"
)

func (a *API) withSession(next func(http.ResponseWriter, *http.Request, *db.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := a.sessionFromRequest(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}
		next(w, r, sess)
	}
}

func (a *API) sessionFromRequest(r *http.Request) (*db.Session, error) {
	if r.Header.Get(protocol.VersionHeader) != "1" {
		return nil, fmt.Errorf("missing or invalid protocol version")
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		return nil, fmt.Errorf("missing bearer token")
	}
	return a.db.Authenticate(strings.TrimPrefix(authz, "Bearer "))
}
