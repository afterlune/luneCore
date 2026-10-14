package core

import (
	"context"
	"encoding/json"
	"errors"
)

// SubscribeEvents replays durable events after afterCursor, then follows new
// events. Model stream deltas are live-only and may be dropped if a consumer is slow.
func (r *SessionRuntime) SubscribeEvents(ctx context.Context, afterCursor int64) (<-chan Event, error) {
	return r.subscribeEvents(ctx, afterCursor, true)
}

func (r *SessionRuntime) subscribeEvents(ctx context.Context, afterCursor int64, schedule bool) (<-chan Event, error) {
	if r == nil || r.core == nil || ctx == nil || afterCursor < 0 {
		return nil, errors.New("session runtime, context, and a non-negative event cursor are required")
	}
	if _, err := r.core.GetSession(ctx, r.sessionID); err != nil {
		return nil, err
	}
	hub := r.core.eventHub(r.sessionID)
	live, unsubscribe := hub.subscribeLive()
	if _, err := r.core.GetSession(ctx, r.sessionID); err != nil {
		unsubscribe()
		return nil, err
	}
	output := make(chan Event, 128)
	go func() {
		defer close(output)
		defer unsubscribe()
		cursor := afterCursor
		for {
			if hub.isClosed() {
				return
			}
			changed := hub.changeChannel()
			events, err := r.core.sessions.RuntimeEventsAfter(ctx, r.sessionID, cursor, 200)
			if err != nil {
				select {
				case output <- Event{Kind: EventRuntimeStreamFailed, SessionID: r.sessionID, Err: err.Error()}:
				case <-ctx.Done():
				}
				return
			}
			for _, stored := range events {
				var event Event
				if err := json.Unmarshal(stored.Payload, &event); err != nil {
					select {
					case output <- Event{Kind: EventRuntimeStreamFailed, SessionID: r.sessionID, Err: err.Error()}:
					case <-ctx.Done():
					}
					return
				}
				event.Cursor, event.OccurredAt = stored.Cursor, stored.CreatedAt
				select {
				case output <- event:
					cursor = stored.Cursor
				case <-ctx.Done():
					return
				}
			}
			if len(events) == 200 {
				continue
			}
			if hub.isClosed() {
				return
			}
			select {
			case event := <-live:
				select {
				case output <- event:
				case <-ctx.Done():
					return
				}
			case <-changed:
			case <-ctx.Done():
				return
			}
		}
	}()
	if schedule {
		r.core.scheduleQueueDrain(r.sessionID)
	}
	return output, nil
}
