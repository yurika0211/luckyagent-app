package websocket

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/session"
	"github.com/yurika0211/luckyagent-runtime/tool"
)

func TestHandlerQueuesMessagesWithoutCancellingCurrentRun(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var calls []string
	runtime := &stubAgentRuntime{
		sessions: mgr,
		tools:    tool.NewRegistry(),
		chatStreamInputFn: func(ctx context.Context, sessionID string, input agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			mu.Lock()
			calls = append(calls, input.RoutingText)
			callNumber := len(calls)
			mu.Unlock()
			if callNumber == 1 {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					t.Fatalf("first run was cancelled: %v", ctx.Err())
				}
			}
			out := make(chan agent.ChatEvent, 1)
			out <- agent.ChatEvent{Type: agent.ChatEventDone, Content: input.RoutingText + " done"}
			close(out)
			return out, nil
		},
	}
	h := NewAgentHandler(runtime)
	client := &Client{SessionID: "queue-session", Send: make(chan *Message, 32)}

	handle := func(id, text string) {
		data, _ := json.Marshal(ChatData{Message: text, Stream: true})
		h.HandleMessage(client, &Message{Type: TypeChat, SessionID: client.SessionID, ID: id, Data: data})
	}
	handle("request-1", "first")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not start")
	}
	handle("request-2", "second")

	mu.Lock()
	if len(calls) != 1 {
		t.Fatalf("second message started before first completed: %#v", calls)
	}
	mu.Unlock()
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		complete := len(calls) == 2
		mu.Unlock()
		if complete {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("queued message did not run after the first message")
}

func TestHandlerEventsReplayFromCursor(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewAgentHandler(&stubAgentRuntime{
		sessions: mgr,
		tools:    tool.NewRegistry(),
		chatStreamInputFn: func(context.Context, string, agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			out := make(chan agent.ChatEvent, 1)
			out <- agent.ChatEvent{Type: agent.ChatEventDone, Content: "done"}
			close(out)
			return out, nil
		},
	})
	client := &Client{SessionID: "replay-session", Send: make(chan *Message, 32)}
	data, _ := json.Marshal(ChatData{Message: "hello", Stream: true})
	h.HandleMessage(client, &Message{Type: TypeChat, SessionID: client.SessionID, ID: "request-replay", Data: data})
	h.WaitSession(client.SessionID, 2*time.Second)
	events := h.store.Replay(client.SessionID, "")
	if len(events) < 3 {
		t.Fatalf("expected persisted lifecycle events, got %d", len(events))
	}
	remaining := h.store.Replay(client.SessionID, events[0].ID)
	if len(remaining) != len(events)-1 {
		t.Fatalf("cursor replay returned %d events, want %d", len(remaining), len(events)-1)
	}
	if remaining[0].ID == events[0].ID {
		t.Fatal("cursor replay included the cursor event")
	}
}

func TestRunStoreReconnectReplaySkipsCompletedRunsWithoutCursor(t *testing.T) {
	store := newRunStore("")
	now := time.Now().UTC()
	completed := persistedRun{ID: "completed-run", SessionID: "reconnect-session", State: "completed", CreatedAt: now, UpdatedAt: now}
	active := persistedRun{ID: "active-run", SessionID: "reconnect-session", State: "running", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertRun(completed); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRun(active); err != nil {
		t.Fatal(err)
	}
	completedEvent, _ := NewMessage(TypeStreamEnd, completed.SessionID, StreamEndData{FullResponse: "old answer"})
	completedEvent.RunID = completed.ID
	activeEvent, _ := NewMessage(TypeStreamChunk, active.SessionID, StreamChunkData{Content: "current answer"})
	activeEvent.RunID = active.ID
	if err := store.AppendEvent(completed.SessionID, completed.ID, completedEvent); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(active.SessionID, active.ID, activeEvent); err != nil {
		t.Fatal(err)
	}

	events := store.ReplayForReconnect(active.SessionID, "")
	if len(events) != 1 || events[0].RunID != active.ID {
		t.Fatalf("empty cursor replayed %#v, want only active run", events)
	}
	events = store.ReplayForReconnect(active.SessionID, "missing-cursor")
	if len(events) != 1 || events[0].RunID != active.ID {
		t.Fatalf("stale cursor replayed %#v, want only active run", events)
	}
}

func TestHandlerStartRestoresPersistedRunOnlyOnce(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 2)
	runtime := &stubAgentRuntime{
		sessions: mgr,
		tools:    tool.NewRegistry(),
		chatStreamInputFn: func(ctx context.Context, sessionID string, input agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			started <- input.RoutingText
			out := make(chan agent.ChatEvent, 1)
			out <- agent.ChatEvent{Type: agent.ChatEventDone, Content: "recovered"}
			close(out)
			return out, nil
		},
	}
	h := NewAgentHandler(runtime)
	h.store = newRunStore(t.TempDir())
	now := time.Now().UTC()
	if err := h.store.UpsertRun(persistedRun{
		ID:        "restore-once",
		SessionID: "restore-session",
		ParentID:  "restore-once",
		Message:   "continue this task",
		Stream:    true,
		State:     "running",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	h.Start()
	h.Start()
	if !h.ResumeSession("restore-session", "restore-once") {
		t.Fatal("explicit resume did not release restored run")
	}
	h.WaitSession("restore-session", 2*time.Second)

	select {
	case got := <-started:
		if got != "continue this task" {
			t.Fatalf("restored message = %q, want original message", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("persisted run was not restored")
	}
	select {
	case duplicate := <-started:
		t.Fatalf("run restored twice with message %q", duplicate)
	case <-time.After(100 * time.Millisecond):
	}

	active := h.store.ActiveRuns("restore-session")
	if len(active) != 0 {
		t.Fatalf("restored run remained active after completion: %#v", active)
	}
}
