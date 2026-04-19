package hub

import (
	"io"
	"log"
	"testing"

	"syna/internal/common/protocol"
)

func TestSubscribeEnforcesPerWorkspaceLimit(t *testing.T) {
	h := New(1, log.New(io.Discard, "", 0))
	first, err := h.Subscribe("workspace-1")
	if err != nil {
		t.Fatalf("Subscribe(first): %v", err)
	}
	defer h.Unsubscribe("workspace-1", first)
	if _, err := h.Subscribe("workspace-1"); err == nil {
		t.Fatalf("expected second subscriber to be rejected")
	}
}

func TestPublishDropsSlowSubscribers(t *testing.T) {
	h := New(2, log.New(io.Discard, "", 0))
	slow, err := h.Subscribe("workspace-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	for i := 0; i < cap(slow.C); i++ {
		slow.C <- protocol.WSMessage{Type: "event"}
	}
	h.Publish("workspace-1", protocol.WSMessage{Type: "event"})
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.subscribers["workspace-1"]) != 0 {
		t.Fatalf("expected slow subscriber to be dropped")
	}
}
