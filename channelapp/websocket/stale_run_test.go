package websocket

import (
	"context"
	"testing"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/session"
	"github.com/yurika0211/luckyagent-runtime/tool"
)

func TestStartExpiresStaleRunningRun(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	h := NewAgentHandler(&stubAgentRuntime{
		sessions: mgr,
		tools:    tool.NewRegistry(),
		chatStreamInputFn: func(context.Context, string, agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			started <- struct{}{}
			out := make(chan agent.ChatEvent)
			close(out)
			return out, nil
		},
	})
	h.store = newRunStore(t.TempDir())
	old := time.Now().UTC().Add(-staleRunTTL - time.Minute)
	if err := h.store.UpsertRun(persistedRun{
		ID:        "stale-run",
		SessionID: "stale-session",
		ParentID:  "stale-run",
		Message:   "keep compiling forever",
		Stream:    true,
		State:     "running",
		CreatedAt: old,
		UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}

	h.Start()

	select {
	case <-started:
		t.Fatal("stale running run started a stream")
	case <-time.After(150 * time.Millisecond):
	}
	if got := h.store.Restoreable(); len(got) != 0 {
		t.Fatalf("stale run still restoreable: %#v", got)
	}
	active := h.store.ActiveRuns("stale-session")
	if len(active) != 0 {
		t.Fatalf("stale run still active: %#v", active)
	}
}

func TestStartExpiresStaleQueuedRun(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewAgentHandler(&stubAgentRuntime{sessions: mgr, tools: tool.NewRegistry()})
	h.store = newRunStore(t.TempDir())
	created := time.Now().UTC().Add(-2 * time.Hour)
	if err := h.store.UpsertRun(persistedRun{
		ID:        "stale-queued",
		SessionID: "queued-session",
		Message:   "old queue",
		State:     "queued",
		CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	h.Start()
	if got := h.store.Restoreable(); len(got) != 0 {
		t.Fatalf("stale queued run still restoreable: %#v", got)
	}
}

func TestStartRestoresFreshRunningRun(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 1)
	runtime := &stubAgentRuntime{
		sessions: mgr,
		tools:    tool.NewRegistry(),
		chatStreamInputFn: func(context.Context, string, agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			started <- "fresh"
			out := make(chan agent.ChatEvent, 1)
			out <- agent.ChatEvent{Type: agent.ChatEventDone, Content: "ok"}
			close(out)
			return out, nil
		},
	}
	h := NewAgentHandler(runtime)
	h.store = newRunStore(t.TempDir())
	now := time.Now().UTC()
	if err := h.store.UpsertRun(persistedRun{
		ID:        "fresh-run",
		SessionID: "fresh-session",
		ParentID:  "fresh-run",
		Message:   "still working",
		Stream:    true,
		State:     "running",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	h.Start()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("fresh running run was not resumed")
	}
	// startNext keeps the session manager's files open until the run goroutine
	// exits. TempDir cleanup fails if the test returns first.
	if !h.WaitSession("fresh-session", 2*time.Second) {
		t.Fatal("fresh running run did not finish")
	}
}
