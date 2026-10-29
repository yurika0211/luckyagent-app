package websocket

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/session"
	"github.com/yurika0211/luckyagent-runtime/tool"
)

func TestStartupDoesNotExecuteRecoveryMetadataWithDamagedHistory(t *testing.T) {
	root := t.TempDir()
	seed := newRunStore(root)
	now := time.Now().UTC()
	runs := []persistedRun{
		{ID: "first", SessionID: "damaged", ParentID: "first", Message: "must not execute", State: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "next", SessionID: "damaged", ParentID: "next", Message: "must not execute either", State: "queued", CreatedAt: now, UpdatedAt: now},
	}
	for _, run := range runs {
		if err := seed.UpsertRun(run); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := filepath.Glob(filepath.Join(root, "*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("journal paths=%v err=%v", paths, err)
	}
	metadata, err := json.Marshal(runs)
	if err != nil {
		t.Fatal(err)
	}
	// Recovery can read the active metadata, but executing must first validate
	// the complete history through the ordinary loader and persist running.
	snapshot := strings.TrimSuffix(paths[0], ".jsonl") + ".json"
	damaged := []byte(`{"version":1,"session_id":"damaged","last_sequence":0,"runs":` + string(metadata) + `,"events":[`)
	if err := os.WriteFile(snapshot, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths[0]); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	h := NewAgentHandler(&stubAgentRuntime{tools: tool.NewRegistry(), chatInputFn: func(context.Context, string, agent.UserTurnInput) (string, error) {
		calls.Add(1)
		return "unexpected result", nil
	}})
	h.store = newRunStore(root)
	if got := h.store.Restoreable(); len(got) != 2 {
		t.Fatalf("fixture must expose both active records to metadata recovery: %#v", got)
	}
	notifications := make(chan *Message, 8)
	h.SetEventSink(func(_ string, msg *Message) { notifications <- msg })
	h.Start()
	h.Start()
	if !h.WaitSession("damaged", 2*time.Second) {
		t.Fatal("failed recovery did not release the runner and drain its queue")
	}
	if calls.Load() != 0 || h.PendingCount() != 0 {
		t.Fatalf("executions=%d pending=%d", calls.Load(), h.PendingCount())
	}
	for _, run := range runs {
		select {
		case msg := <-notifications:
			assertPersistenceFailure(t, msg, run.ID)
		case <-time.After(time.Second):
			t.Fatalf("missing persistence failure notification for %s", run.ID)
		}
		select {
		case msg := <-notifications:
			assertTerminalStatus(t, msg, run.ID, "error")
		case <-time.After(time.Second):
			t.Fatalf("missing terminal status for %s", run.ID)
		}
	}
	if len(notifications) != 0 {
		t.Fatal("recovery emitted extra output after persistence failure")
	}
	if got, err := os.ReadFile(snapshot); err != nil || string(got) != string(damaged) {
		t.Fatalf("damaged history was overwritten: %v", err)
	}
}

func TestQueuedEventPersistenceFailureSkipsRunAndContinuesQueue(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := make(chan string, 2)
	h := NewAgentHandler(&stubAgentRuntime{sessions: mgr, tools: tool.NewRegistry(), chatInputFn: func(_ context.Context, _ string, input agent.UserTurnInput) (string, error) {
		calls <- input.RoutingText
		return "done", nil
	}})
	root := t.TempDir()
	h.store = newRunStore(root)
	client := &Client{SessionID: "queued", Send: make(chan *Message, 16)}
	now := time.Now().UTC()
	for _, id := range []string{"failed", "healthy"} {
		if err := h.store.UpsertRun(persistedRun{ID: id, SessionID: client.SessionID, State: "queued", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	failed := &queuedRun{id: "failed", parentID: "failed", client: client, data: ChatData{Message: "skip me"}}
	healthy := &queuedRun{id: "healthy", parentID: "healthy", client: client, data: ChatData{Message: "execute me"}}
	h.runners[client.SessionID] = &sessionRunner{queue: []*queuedRun{failed, healthy}}
	restore := interruptRunJournal(t, root)
	queued, _ := NewMessage(TypeStatus, client.SessionID, StatusData{State: "queued"})
	queued.ParentID = failed.parentID
	h.emit(client, client.SessionID, failed.id, queued)
	restore()
	assertPersistenceFailure(t, <-client.Send, failed.id)
	assertTerminalStatus(t, <-client.Send, failed.id, "error")
	h.startNext(client.SessionID)
	if !h.WaitSession(client.SessionID, 2*time.Second) {
		t.Fatal("queue stopped after its first persistence failure")
	}
	if len(calls) != 1 || <-calls != "execute me" {
		t.Fatal("failed queued run executed or healthy successor was not executed")
	}
	for len(client.Send) > 0 {
		if msg := <-client.Send; msg.RunID == failed.id {
			t.Fatalf("failed queued run emitted further output: %#v", msg)
		}
	}
	if run, ok, err := h.store.GetRun(client.SessionID, failed.id); err != nil || !ok || run.State != "error" {
		t.Fatalf("failed queued run=%#v ok=%t err=%v", run, ok, err)
	}
}

func TestEventPersistenceFailureStopsExecutionWithoutSuccessfulEnd(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[streaming], func(t *testing.T) {
			mgr, err := session.NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			var restore func()
			cancelled := make(chan struct{})
			runtime := &stubAgentRuntime{sessions: mgr, tools: tool.NewRegistry()}
			runtime.chatInputFn = func(context.Context, string, agent.UserTurnInput) (string, error) {
				restore = interruptRunJournal(t, root)
				return "must not be delivered", nil
			}
			runtime.chatStreamInputFn = func(ctx context.Context, _ string, _ agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
				restore = interruptRunJournal(t, root)
				out := make(chan agent.ChatEvent, 2)
				out <- agent.ChatEvent{Type: agent.ChatEventContent, Content: "cannot persist this"}
				go func() {
					<-ctx.Done()
					close(cancelled)
					// Even a producer returning a buffered success after cancellation
					// must not turn the persistence failure into a successful result.
					out <- agent.ChatEvent{Type: agent.ChatEventDone, Content: "late result"}
					close(out)
				}()
				return out, nil
			}
			h := NewAgentHandler(runtime)
			h.store = newRunStore(root)
			notifications := make(chan *Message, 16)
			h.SetEventSink(func(_ string, msg *Message) {
				if msg.Type == TypeError {
					var data ErrorData
					_ = msg.ParseData(&data)
					if data.Code == "PERSIST_FAILED" && restore != nil {
						restore()
					}
				}
				notifications <- msg
			})
			client := &Client{SessionID: "active", Send: make(chan *Message, 16)}
			h.HandleMessage(client, &Message{Type: TypeChat, ID: "request", Data: mustChatData(ChatData{Message: "test", Stream: streaming})})
			if !h.WaitSession(client.SessionID, 2*time.Second) {
				t.Fatal("persistence failure left the run active")
			}
			if streaming {
				select {
				case <-cancelled:
				case <-time.After(time.Second):
					t.Fatal("storage failure did not cancel the Agent execution")
				}
			}
			failures := 0
			for len(notifications) > 0 {
				msg := <-notifications
				if msg.Type == TypeStreamEnd || msg.Type == TypeStreamChunk {
					t.Fatalf("unpersisted or successful output escaped: %#v", msg)
				}
				if msg.Type == TypeError {
					assertPersistenceFailure(t, msg, "request")
					failures++
				}
				if msg.Type == TypeStatus && msg.RunID == "request" && msg.ID == "" {
					assertTerminalStatus(t, msg, "request", "error")
				}
				if msg.Type == TypeStatus {
					var data StatusData
					_ = msg.ParseData(&data)
					if data.State == "cancelled" || data.State == "idle" {
						t.Fatalf("storage failure was followed by a misleading state: %s", data.State)
					}
				}
			}
			if failures != 1 {
				t.Fatalf("persistence failure notifications=%d", failures)
			}
			if run, ok, err := h.store.GetRun(client.SessionID, "request"); err != nil || !ok || run.State != "error" {
				t.Fatalf("failed active run=%#v ok=%t err=%v", run, ok, err)
			}
		})
	}
}

// Replace the actual journal with a directory, so append fails even when tests
// run as root. Restoring it lets the test verify the persisted terminal state.
func interruptRunJournal(t *testing.T, root string) func() {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("journal paths=%v err=%v", paths, err)
	}
	path, backup := paths[0], paths[0]+".test-backup"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
			if err := os.Rename(backup, path); err != nil {
				t.Error(err)
			}
		})
	}
}

func assertPersistenceFailure(t *testing.T, msg *Message, runID string) {
	t.Helper()
	var data ErrorData
	if msg == nil || msg.Type != TypeError || msg.RunID != runID || msg.ParentID != runID {
		t.Fatalf("unexpected persistence failure message: %#v", msg)
	}
	if err := msg.ParseData(&data); err != nil || data.Code != "PERSIST_FAILED" {
		t.Fatalf("error data=%#v err=%v", data, err)
	}
	if msg.EventID != "" || msg.ID != "" {
		t.Fatal("unpersisted notification exposed a replay cursor")
	}
}

func assertTerminalStatus(t *testing.T, msg *Message, runID, state string) {
	t.Helper()
	var data StatusData
	if msg == nil || msg.Type != TypeStatus || msg.RunID != runID || msg.ParseData(&data) != nil || data.State != state {
		t.Fatalf("terminal status = %#v, want %s", msg, state)
	}
	if msg.EventID != "" || msg.ID != "" {
		t.Fatal("terminal status sent after a store failure exposed a replay cursor")
	}
}
