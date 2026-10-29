package websocket

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/session"
	"github.com/yurika0211/luckyagent-runtime/tool"
)

func TestSessionActivityListsOnlyUnfinishedRuns(t *testing.T) {
	h := NewAgentHandler(&stubAgentRuntime{})
	h.store = newRunStore(t.TempDir())
	now := time.Now().UTC()
	for _, run := range []persistedRun{
		{ID: "queued-run", SessionID: "chat", State: "queued", CreatedAt: now, UpdatedAt: now},
		{ID: "running-run", SessionID: "chat", State: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "done-run", SessionID: "chat", State: "completed", CreatedAt: now, UpdatedAt: now},
	} {
		if err := h.store.UpsertRun(run); err != nil {
			t.Fatal(err)
		}
	}

	activity, err := h.SessionActivity("chat")
	if err != nil {
		t.Fatal(err)
	}
	if len(activity) != 2 {
		t.Fatalf("activity = %#v, want queued and running only", activity)
	}
	seen := map[string]string{}
	for _, run := range activity {
		seen[run.RunID] = run.State
		if run.UpdatedAt.IsZero() {
			t.Fatalf("run %s has no updated_at", run.RunID)
		}
	}
	if seen["queued-run"] != "queued" || seen["running-run"] != "running" || seen["done-run"] != "" {
		t.Fatalf("states = %#v", seen)
	}
	empty, err := h.SessionActivity("other")
	if err != nil || len(empty) != 0 {
		t.Fatalf("other session activity = %#v err=%v", empty, err)
	}
}

func TestReconnectReplayDoesNotCreateRunningActivity(t *testing.T) {
	h := NewAgentHandler(&stubAgentRuntime{})
	h.store = newRunStore(t.TempDir())
	now := time.Now().UTC()
	run := persistedRun{ID: "done-run", SessionID: "replay", ParentID: "done-run", State: "completed", CreatedAt: now, UpdatedAt: now}
	if err := h.store.UpsertRun(run); err != nil {
		t.Fatal(err)
	}
	cursor, _ := NewMessage(TypeStatus, run.SessionID, StatusData{State: "queued", Message: "message queued"})
	cursor.RunID = run.ID
	status, _ := NewMessage(TypeStatus, run.SessionID, StatusData{State: "running", Message: "agent is running"})
	status.RunID = run.ID
	if err := h.store.AppendEvent(run.SessionID, run.ID, cursor); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AppendEvent(run.SessionID, run.ID, status); err != nil {
		t.Fatal(err)
	}
	client := &Client{SessionID: run.SessionID, Send: make(chan *Message, 8)}
	data, _ := json.Marshal(ReconnectData{LastMessageID: cursor.ID})
	h.HandleReconnect(client, &Message{Type: TypeReconnect, Data: data})

	var replayedMsg *Message
	select {
	case replayedMsg = <-client.Send:
		if replayedMsg.Type != TypeStatus {
			t.Fatalf("first replay frame = %s", replayedMsg.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("replay did not send the historical status")
	}
	if !replayMarked(replayedMsg) {
		t.Fatalf("historical status was not marked as replay: %s", replayedMsg.Data)
	}
	stored := h.store.Replay(run.SessionID, "")
	if replayMarked(stored[0]) {
		t.Fatal("replay flag was written back into the durable event")
	}
	activity, err := h.SessionActivity(run.SessionID)
	if err != nil || len(activity) != 0 {
		t.Fatalf("replay created activity %#v err=%v", activity, err)
	}
}

func TestFinishedRunEmitsTerminalStatusAndLeavesActivity(t *testing.T) {
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
	h.store = newRunStore(t.TempDir())
	client := &Client{SessionID: "finish", Send: make(chan *Message, 16)}
	body, _ := json.Marshal(ChatData{Message: "hello", Stream: true})
	h.HandleMessage(client, &Message{Type: TypeChat, ID: "request", Data: body})
	if !h.WaitSession(client.SessionID, 2*time.Second) {
		t.Fatal("run did not finish")
	}

	var terminal StatusData
	for len(client.Send) > 0 {
		msg := <-client.Send
		if msg.Type != TypeStatus {
			continue
		}
		var data StatusData
		if err := msg.ParseData(&data); err != nil {
			t.Fatal(err)
		}
		if data.State == "thinking" || data.State == "executing" || data.State == "idle" {
			t.Fatalf("run used a parallel activity word: %s", data.State)
		}
		terminal = data
	}
	if terminal.State != "completed" {
		t.Fatalf("terminal status = %#v", terminal)
	}
	activity, err := h.SessionActivity(client.SessionID)
	if err != nil || len(activity) != 0 {
		t.Fatalf("finished run still listed: %#v err=%v", activity, err)
	}
}

func TestCancelPersistsTerminalStateBeforeActivityQuery(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	h := NewAgentHandler(&stubAgentRuntime{
		sessions: mgr,
		tools:    tool.NewRegistry(),
		chatStreamInputFn: func(ctx context.Context, _ string, _ agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			close(started)
			out := make(chan agent.ChatEvent)
			go func() {
				<-ctx.Done()
				close(out)
			}()
			return out, nil
		},
	})
	h.store = newRunStore(t.TempDir())
	client := &Client{SessionID: "cancel", Send: make(chan *Message, 16)}
	body, _ := json.Marshal(ChatData{Message: "stop me", Stream: true})
	h.HandleMessage(client, &Message{Type: TypeChat, ID: "request", Data: body})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("run did not start")
	}
	h.HandleMessage(client, &Message{Type: TypeCancel, ID: "stop"})
	if !h.WaitSession(client.SessionID, 2*time.Second) {
		t.Fatal("cancelled run stayed active")
	}
	activity, err := h.SessionActivity(client.SessionID)
	if err != nil || len(activity) != 0 {
		t.Fatalf("cancelled run still listed: %#v err=%v", activity, err)
	}
	var cancelled bool
	for len(client.Send) > 0 {
		msg := <-client.Send
		if msg.Type != TypeStatus {
			continue
		}
		var data StatusData
		_ = msg.ParseData(&data)
		if data.State == "cancelled" {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatal("client did not receive cancelled")
	}
}
