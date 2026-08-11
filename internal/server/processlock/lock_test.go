package processlock

import "testing"

func TestAcquireRejectsSecondOwnerAndAllowsReuse(t *testing.T) {
	dataDir := t.TempDir()
	first, err := Acquire(dataDir)
	if err != nil {
		t.Fatalf("Acquire(first): %v", err)
	}
	if _, err := Acquire(dataDir); err == nil {
		t.Fatal("expected second lock acquisition to fail")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first): %v", err)
	}
	second, err := Acquire(dataDir)
	if err != nil {
		t.Fatalf("Acquire(after close): %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close(second): %v", err)
	}
}
