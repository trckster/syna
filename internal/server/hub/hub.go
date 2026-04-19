package hub

import (
	"fmt"
	"log"
	"sync"

	"syna/internal/common/protocol"
)

type Subscriber struct {
	C chan protocol.WSMessage
}

type Hub struct {
	logger      *log.Logger
	maxPerWS    int
	mu          sync.RWMutex
	subscribers map[string]map[*Subscriber]struct{}
}

func New(maxPerWS int, logger *log.Logger) *Hub {
	return &Hub{
		logger:      logger,
		maxPerWS:    maxPerWS,
		subscribers: make(map[string]map[*Subscriber]struct{}),
	}
}

func (h *Hub) Subscribe(workspaceID string) (*Subscriber, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subscribers[workspaceID] != nil && len(h.subscribers[workspaceID]) >= h.maxPerWS {
		if h.logger != nil {
			h.logger.Printf("rejected websocket subscriber for workspace %s: limit %d reached", workspaceID, h.maxPerWS)
		}
		return nil, fmt.Errorf("workspace websocket client limit reached")
	}
	sub := &Subscriber{C: make(chan protocol.WSMessage, 32)}
	if h.subscribers[workspaceID] == nil {
		h.subscribers[workspaceID] = make(map[*Subscriber]struct{})
	}
	h.subscribers[workspaceID][sub] = struct{}{}
	if h.logger != nil {
		h.logger.Printf("accepted websocket subscriber for workspace %s: %d active", workspaceID, len(h.subscribers[workspaceID]))
	}
	return sub, nil
}

func (h *Hub) Unsubscribe(workspaceID string, sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if subs := h.subscribers[workspaceID]; subs != nil {
		if _, ok := subs[sub]; !ok {
			return
		}
		delete(subs, sub)
		if len(subs) == 0 {
			delete(h.subscribers, workspaceID)
		}
		if h.logger != nil {
			h.logger.Printf("closed websocket subscriber for workspace %s: %d active", workspaceID, len(subs))
		}
		close(sub.C)
	}
}

func (h *Hub) Publish(workspaceID string, msg protocol.WSMessage) {
	h.mu.RLock()
	var slow []*Subscriber
	for sub := range h.subscribers[workspaceID] {
		select {
		case sub.C <- msg:
		default:
			slow = append(slow, sub)
		}
	}
	h.mu.RUnlock()
	for _, sub := range slow {
		if h.logger != nil {
			h.logger.Printf("dropping slow websocket subscriber for workspace %s", workspaceID)
		}
		h.Unsubscribe(workspaceID, sub)
	}
}
