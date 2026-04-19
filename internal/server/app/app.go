package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"syna/internal/buildinfo"
	"syna/internal/server/admin"
	"syna/internal/server/api"
	servercfg "syna/internal/server/config"
	"syna/internal/server/db"
	"syna/internal/server/hub"
	"syna/internal/server/objectstore"
)

const shutdownTimeout = 10 * time.Second

func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		usage(stdout)
		return 2
	}
	switch args[1] {
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, "syna-server", buildinfo.String())
		return 0
	}

	cfg, err := servercfg.Load()
	if err != nil {
		return fail(stderr, err)
	}
	if err := servercfg.EnsureDataDirs(cfg.DataDir); err != nil {
		return fail(stderr, err)
	}
	database, err := db.Open(filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		return fail(stderr, err)
	}
	defer database.Close()

	switch args[1] {
	case "migrate":
		return exitErr(stderr, database.Migrate())
	case "stats":
		if err := database.Migrate(); err != nil {
			return fail(stderr, err)
		}
		return exitErr(stderr, admin.Stats(database))
	case "doctor":
		if err := database.Migrate(); err != nil {
			return fail(stderr, err)
		}
		return exitErr(stderr, admin.Doctor(database, cfg.DataDir))
	case "gc":
		if err := database.Migrate(); err != nil {
			return fail(stderr, err)
		}
		return exitErr(stderr, admin.GC(database, objectstore.New(cfg.DataDir), time.Now().UTC(), cfg.EventRetention, cfg.ZeroRefRetention))
	case "serve":
		if err := database.Migrate(); err != nil {
			return fail(stderr, err)
		}
		return exitErr(stderr, runServer(cfg, database, stdout))
	default:
		usage(stdout)
		return 2
	}
}

func runServer(cfg servercfg.Config, database *db.DB, output io.Writer) error {
	logger := log.New(output, "syna-server ", log.LstdFlags|log.Lmsgprefix)
	for _, warning := range cfg.Warnings() {
		logger.Printf("warning: %s", warning)
	}
	serverAPI := api.New(cfg, database, objectstore.New(cfg.DataDir), hub.New(cfg.MaxWSClients, logger), logger)
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           serverAPI.Handler(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Printf("listening on %s", cfg.Listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: syna-server <serve|migrate|gc|stats|doctor|version>")
}

func exitErr(stderr io.Writer, err error) int {
	if err != nil {
		return fail(stderr, err)
	}
	return 0
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, err)
	return 1
}
