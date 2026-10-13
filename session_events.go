package core

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

type sessionEventHub struct {
	mu      sync.Mutex
	changed chan struct{}
	live    map[uint64]eventSubscriber
	nextID  uint64
	closed  bool
}

type eventSubscriber struct {
	channel chan Event
}

func newSessionEventHub() *sessionEventHub {
	return &sessionEventHub{changed: make(chan struct{}), live: make(map[uint64]eventSubscriber)}
}

func (h *sessionEventHub) subscribeLive() (<-chan Event, func()) {
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	channel := make(chan Event, 64)
	h.live[id] = eventSubscriber{channel: channel}
	h.mu.Unlock()
	return channel, func() { h.mu.Lock(); delete(h.live, id); h.mu.Unlock() }
}

func (h *sessionEventHub) changeChannel() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

func (h *sessionEventHub) notifyDurable() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	close(h.changed)
	h.changed = make(chan struct{})
	h.mu.Unlock()
}

func (h *sessionEventHub) isClosed() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.closed }

func (h *sessionEventHub) close() {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		close(h.changed)
	}
	h.mu.Unlock()
}

func (h *sessionEventHub) publishLive(event Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for _, subscriber := range h.live {
		select {
		case subscriber.channel <- event:
		default: // Model deltas are best effort and must not stall execution.
		}
	}
}

func (c *Core) eventHub(sessionID string) *sessionEventHub {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.eventHubs == nil {
		c.eventHubs = make(map[string]*sessionEventHub)
	}
	hub := c.eventHubs[sessionID]
	if hub == nil {
		hub = newSessionEventHub()
		c.eventHubs[sessionID] = hub
	}
	return hub
}

func (c *Core) emit(event Event) {
	if event.SessionID == "" {
		return
	}
	if event.Kind == EventModelStream {
		c.eventHub(event.SessionID).publishLive(event)
		return
	}
	encoded, err := json.Marshal(safeEventProjection(event))
	if err != nil {
		c.logger.Warn("encode runtime event", "error", err)
		return
	}
	if _, err := c.sessions.AppendRuntimeEvent(context.Background(), event.SessionID, encoded); err != nil {
		c.logger.Warn("persist runtime event", "error", err, "kind", event.Kind)
		return
	}
	c.eventHub(event.SessionID).notifyDurable()
}

func safeEventProjection(event Event) Event {
	event.Cursor, event.OccurredAt, event.Err = 0, time.Time{}, ""
	event.Tool.Arguments = nil
	if event.Approval != nil {
		copy := cloneApproval(*event.Approval)
		copy.Arguments = nil
		event.Approval = &copy
	}
	if event.Interaction != nil {
		copy := cloneInteraction(*event.Interaction)
		copy.Request = nil
		event.Interaction = &copy
	}
	if event.UserQuestion != nil {
		copy := cloneUserQuestion(*event.UserQuestion)
		copy.Question = ""
		copy.Options = nil
		event.UserQuestion = &copy
	}
	if event.Compaction != nil {
		copy := *event.Compaction
		copy.Err = ""
		event.Compaction = &copy
	}
	if event.ToolRecovery != nil {
		copy := *event.ToolRecovery
		copy.Arguments = nil
		event.ToolRecovery = &copy
	}
	event.Stream = nil
	return event
}

func (c *Core) persistQueuedEvent(ctx context.Context, event Event, operation func(json.RawMessage) (int64, error)) error {
	encoded, err := json.Marshal(safeEventProjection(event))
	if err != nil {
		return err
	}
	if _, err := operation(encoded); err != nil {
		return err
	}
	c.eventHub(event.SessionID).notifyDurable()
	return nil
}

func mustMarshalEvent(event Event) json.RawMessage {
	encoded, _ := json.Marshal(safeEventProjection(event))
	return encoded
}

func cloneApproval(request ApprovalRequest) ApprovalRequest {
	request.Arguments = append(json.RawMessage(nil), request.Arguments...)
	return request
}

func cloneInteraction(request UserInteractionRequest) UserInteractionRequest {
	request.Request = append(json.RawMessage(nil), request.Request...)
	return request
}

func cloneUserQuestion(request UserQuestionRequest) UserQuestionRequest {
	request.Options = append([]string(nil), request.Options...)
	return request
}
