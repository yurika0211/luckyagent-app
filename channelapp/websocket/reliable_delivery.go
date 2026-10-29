package websocket

import (
	"context"
	"time"
)

// Multiple reconnect requests for a client share one replay without retaining
// a permanent map entry for every client that has ever connected.
func (h *AgentHandler) lockClientReplay(client *Client) func() {
	h.mu.Lock()
	if h.replays == nil {
		h.replays = make(map[*Client]*clientReplayLock)
	}
	lock := h.replays[client]
	if lock == nil {
		lock = &clientReplayLock{}
		h.replays[client] = lock
	}
	lock.users++
	h.mu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		h.mu.Lock()
		lock.users--
		if lock.users == 0 {
			delete(h.replays, client)
		}
		h.mu.Unlock()
	}
}

func (h *AgentHandler) sendChatError(client *Client, parentID, code, message string) {
	msg, _ := NewMessage(TypeError, client.SessionID, ErrorData{Code: code, Message: message})
	msg.ParentID = parentID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.SendReliable(ctx, msg); err != nil {
		client.Close()
	}
}

// Retrying an accepted request reuses its durable outcome and never enqueues
// another execution, even when the original queued acknowledgment was lost.
func (h *AgentHandler) replayAcceptedRun(client *Client, run persistedRun) {
	unlock := h.lockClientReplay(client)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.BeginReplay(); err != nil {
		client.Close()
		return
	}
	delivered := make(map[string]bool)
	defer func() {
		if err := client.EndReplay(ctx, delivered); err != nil {
			client.Close()
		}
	}()
	events, err := h.store.ReplayWithError(client.SessionID, "")
	if err != nil {
		h.sendChatError(client, run.ParentID, "RECOVERY_FAILED", "could not load durable session events")
		return
	}
	terminal := false
	for _, event := range events {
		if event.RunID != run.ID {
			continue
		}
		if err := client.SendReliable(ctx, replayed(event)); err != nil {
			client.Close()
			return
		}
		if event.Type == TypeStreamEnd || event.Type == TypeError {
			terminal = true
		}
		if event.EventID != "" {
			delivered[event.EventID] = true
		} else if event.ID != "" {
			delivered[event.ID] = true
		}
	}
	if terminal {
		return
	}
	if current, ok, err := h.store.GetRun(client.SessionID, run.ID); err == nil && ok {
		run = current
	} else if err != nil {
		h.sendChatError(client, run.ParentID, "RECOVERY_FAILED", "could not load durable run state")
		return
	}
	status, _ := NewMessage(TypeStatus, client.SessionID, StatusData{State: run.State, Message: "request already accepted"})
	status = withReplay(status)
	status.ID, status.EventID, status.RunID, status.ParentID = "", "", run.ID, run.ParentID
	if err := client.SendReliable(ctx, status); err != nil {
		client.Close()
	}
}
