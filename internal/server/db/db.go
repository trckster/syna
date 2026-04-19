package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"syna/internal/common/protocol"
)

type DB struct {
	SQL *sql.DB
}

const LatestSchemaVersion = 3

type Session struct {
	WorkspaceID string
	DeviceID    string
	ExpiresAt   time.Time
}

type RootInfo struct {
	RootID                 string
	Kind                   protocol.RootKind
	DescriptorBlob         []byte
	CreatedSeq             int64
	RemovedSeq             sql.NullInt64
	LatestSnapshotObjectID sql.NullString
	LatestSnapshotSeq      sql.NullInt64
}

func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL&_synchronous=FULL", path)
	sqlDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	return &DB{SQL: sqlDB}, nil
}

func (db *DB) Close() error {
	return db.SQL.Close()
}

func (db *DB) Begin(ctx context.Context) (*sql.Tx, error) {
	return db.SQL.BeginTx(ctx, &sql.TxOptions{})
}
