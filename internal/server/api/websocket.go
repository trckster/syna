package api

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"

	"syna/internal/common/protocol"
)

const (
	wsWriteWait = 10 * time.Second
	wsPongWait  = 60 * time.Second
	wsPingEvery = 45 * time.Second
)

func (a *API) handleWS(w http.ResponseWriter, r *http.Request) {
	sess, err := a.sessionFromRequest(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: a.checkWSOrigin}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	sub, err := a.hub.Subscribe(sess.WorkspaceID)
	if err != nil {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "workspace websocket client limit reached"), time.Now().Add(wsWriteWait))
		return
	}
	defer a.hub.Unsubscribe(sess.WorkspaceID, sub)
	conn.SetReadLimit(protocol.MaxWSMessageBytes)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		for {
			if _, reader, err := conn.NextReader(); err != nil {
				return
			} else if _, err := io.Copy(io.Discard, reader); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(wsPingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-sub.C:
			if !ok {
				return
			}
			if err := conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return
			}
			if err := conn.WriteJSON(msg); err != nil {
				return
			}
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(wsWriteWait)); err != nil {
				return
			}
		}
	}
}

func (a *API) checkWSOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || a.cfg.PublicBaseURL == "" {
		return true
	}
	originURL, err := url.Parse(origin)
	if err != nil {
		return false
	}
	publicURL, err := url.Parse(a.cfg.PublicBaseURL)
	if err != nil {
		return false
	}
	return originURL.Scheme == publicURL.Scheme && originURL.Host == publicURL.Host
}
