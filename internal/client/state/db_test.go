package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestQueueRootRescanDedupes(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	first := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	second := first.Add(10 * time.Second)
	if err := db.QueueRootRescan("root-1", first); err != nil {
		t.Fatalf("QueueRootRescan(first): %v", err)
	}
	if err := db.QueueRootRescan("root-1", second); err != nil {
		t.Fatalf("QueueRootRescan(second): %v", err)
	}

	ops, err := db.ListPendingOps()
	if err != nil {
		t.Fatalf("ListPendingOps: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("expected 1 queued op, got %d", len(ops))
	}
	if ops[0].CreatedAt.UTC() != first {
		t.Fatalf("expected original created_at to be preserved, got %s", ops[0].CreatedAt.UTC())
	}
	if ops[0].OpType != "rescan_root" {
		t.Fatalf("unexpected op type %q", ops[0].OpType)
	}
}

func TestPendingOpsPersistBackoffAndReadyFiltering(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	first := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	second := first.Add(time.Second)
	if err := db.QueueRootRescan("root-1", second); err != nil {
		t.Fatalf("QueueRootRescan(root-1): %v", err)
	}
	if err := db.QueueRootRescan("root-2", first); err != nil {
		t.Fatalf("QueueRootRescan(root-2): %v", err)
	}
	ops, err := db.ListPendingOps()
	if err != nil {
		t.Fatalf("ListPendingOps: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("expected 2 pending ops, got %d", len(ops))
	}
	if got := []string{ops[0].RootID, ops[1].RootID}; got[0] != "root-2" || got[1] != "root-1" {
		t.Fatalf("pending ops not ordered by creation time: %v", got)
	}

	now := first.Add(10 * time.Second)
	if err := db.BumpPendingOpRetry(ops[0].OpID, "connection refused", now); err != nil {
		t.Fatalf("BumpPendingOpRetry: %v", err)
	}
	ready, err := db.ListPendingOpsReady(now.Add(500 * time.Millisecond))
	if err != nil {
		t.Fatalf("ListPendingOpsReady(before): %v", err)
	}
	if len(ready) != 1 || ready[0].RootID != "root-1" {
		t.Fatalf("expected only unbacked-off op to be ready, got %+v", ready)
	}
	ready, err = db.ListPendingOpsReady(now.Add(time.Second))
	if err != nil {
		t.Fatalf("ListPendingOpsReady(after): %v", err)
	}
	if len(ready) != 2 || ready[0].RootID != "root-2" || ready[0].RetryCount != 1 || ready[0].LastError != "connection refused" {
		t.Fatalf("unexpected ready ops after backoff: %+v", ready)
	}
	if !ready[0].NextRetryAt.Equal(now.Add(time.Second)) {
		t.Fatalf("next retry = %s want %s", ready[0].NextRetryAt, now.Add(time.Second))
	}
}

func TestIgnoreRuleStoresExpectedState(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	now := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	if err := db.SetIgnore("root-1", "notes.txt", now.Add(2*time.Second), Entry{
		Kind:          "file",
		ContentSHA256: "abc123",
		Mode:          0o644,
		MTimeNS:       12345,
	}); err != nil {
		t.Fatalf("SetIgnore: %v", err)
	}

	rule, ignored, err := db.IgnoreRule("root-1", "notes.txt", now)
	if err != nil {
		t.Fatalf("IgnoreRule: %v", err)
	}
	if !ignored {
		t.Fatalf("expected ignore rule to be active")
	}
	if rule.Kind != "file" || rule.ContentSHA256 != "abc123" || rule.Mode != 0o644 || rule.MTimeNS != 12345 || rule.Deleted {
		t.Fatalf("unexpected ignore rule %+v", rule)
	}

	if err := db.SetIgnoreDelete("root-1", "gone.txt", now.Add(2*time.Second)); err != nil {
		t.Fatalf("SetIgnoreDelete: %v", err)
	}
	deleteRule, ignored, err := db.IgnoreRule("root-1", "gone.txt", now)
	if err != nil {
		t.Fatalf("IgnoreRule(delete): %v", err)
	}
	if !ignored || !deleteRule.Deleted {
		t.Fatalf("expected delete ignore rule, got %+v", deleteRule)
	}
}
