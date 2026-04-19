package api

import (
	"log"
	"net/http"

	servercfg "syna/internal/server/config"
	"syna/internal/server/db"
	"syna/internal/server/hub"
	"syna/internal/server/objectstore"
)

type API struct {
	cfg    servercfg.Config
	db     *db.DB
	store  *objectstore.Store
	hub    *hub.Hub
	logger *log.Logger
}

func New(cfg servercfg.Config, database *db.DB, store *objectstore.Store, eventHub *hub.Hub, logger *log.Logger) *API {
	return &API{
		cfg:    cfg,
		db:     database,
		store:  store,
		hub:    eventHub,
		logger: logger,
	}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealthz)
	mux.HandleFunc("/readyz", a.handleReadyz)
	mux.HandleFunc("/v1/session/start", a.handleSessionStart)
	mux.HandleFunc("/v1/session/finish", a.handleSessionFinish)
	mux.HandleFunc("/v1/bootstrap", a.withSession(a.handleBootstrap))
	mux.HandleFunc("/v1/events", a.withSession(a.handleEvents))
	mux.HandleFunc("/v1/snapshots", a.withSession(a.handleSnapshotSubmit))
	mux.HandleFunc("/v1/ws", a.handleWS)
	mux.HandleFunc("/v1/objects/", a.handleObjects)
	return mux
}
