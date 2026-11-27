package websocket

import (
	"context"
	"testing"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/session"
	"github.com/yurika0211/luckyagent-runtime/tool"
)

type reviewingRuntime struct {
	stubAgentRuntime
	raw     string
	err     error
	reviews int
}

func (r *reviewingRuntime) ReviewDelegatedTask(context.Context, string, map[string]any) (string, error) {
	r.reviews++
	return r.raw, r.err
}

func TestManualStopStaysHeldAcrossRestart(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 2)
	runtime := &stubAgentRuntime{
		sessions: mgr, tools: tool.NewRegistry(),
		chatStreamInputFn: func(context.Context, string, agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			started <- "ran"
			ch := make(chan agent.ChatEvent, 1)
			ch <- agent.ChatEvent{Type: agent.ChatEventDone}
			close(ch)
			return ch, nil
		},
	}
	h := NewAgentHandler(runtime)
	h.store = newRunStore(t.TempDir())
	now := time.Now().UTC()
	if err := h.store.UpsertRun(persistedRun{
		ID: "stopped-run", SessionID: "sess", ParentID: "stopped-run",
		Message: "\x00held continue this", Stream: true, State: "running", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	h.Start()
	select {
	case got := <-started:
		t.Fatalf("manual stop released the task: %s", got)
	case <-time.After(80 * time.Millisecond):
	}
	restarted := NewAgentHandler(runtime)
	restarted.store = h.store
	restarted.Start()
	select {
	case got := <-started:
		t.Fatalf("restart released a manually stopped task: %s", got)
	case <-time.After(80 * time.Millisecond):
	}
	if !restarted.ResumeSession("sess", "stopped-run") {
		t.Fatal("explicit continue did not release the held run")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("explicit continue did not run")
	}
}

func TestContinueStopsOnWorktreeConflict(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	runtime := &reviewingRuntime{
		stubAgentRuntime: stubAgentRuntime{
			sessions: mgr, tools: tool.NewRegistry(),
			chatStreamInputFn: func(context.Context, string, agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
				started <- struct{}{}
				ch := make(chan agent.ChatEvent)
				close(ch)
				return ch, nil
			},
		},
		raw: `{"merged":false,"conflict":true,"kept_worktree":"/tmp/kept"}`,
	}
	h := NewAgentHandler(runtime)
	h.store = newRunStore(t.TempDir())
	now := time.Now().UTC()
	if err := h.store.UpsertRun(persistedRun{
		ID: "child-task", SessionID: "sess", ParentID: "child-task",
		Message: "\x00held continue", Stream: true, State: "running", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	h.Start()
	if h.ResumeSession("sess", "child-task") {
		t.Fatal("conflict continued the task")
	}
	if runtime.reviews != 1 {
		t.Fatalf("reviews=%d", runtime.reviews)
	}
	select {
	case <-started:
		t.Fatal("conflict still ran the task")
	case <-time.After(80 * time.Millisecond):
	}
}

func TestSecondContinueDoesNotStartAnotherRun(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	starts := 0
	runtime := &stubAgentRuntime{
		sessions: mgr, tools: tool.NewRegistry(),
		chatStreamInputFn: func(context.Context, string, agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			starts++
			entered <- struct{}{}
			ch := make(chan agent.ChatEvent, 1)
			go func() {
				<-release
				ch <- agent.ChatEvent{Type: agent.ChatEventDone}
				close(ch)
			}()
			return ch, nil
		},
	}
	h := NewAgentHandler(runtime)
	h.store = newRunStore(t.TempDir())
	now := time.Now().UTC()
	if err := h.store.UpsertRun(persistedRun{
		ID: "held-run", SessionID: "sess", ParentID: "held-run",
		Message: "\x00held continue once", Stream: true, State: "running", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	h.Start()
	if !h.ResumeSession("sess", "held-run") {
		t.Fatal("first continue did not release the held run")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first continue did not start")
	}
	if h.ResumeSession("sess", "held-run") {
		t.Fatal("second continue succeeded while the first was still running")
	}
	if starts != 1 {
		t.Fatalf("starts=%d", starts)
	}
	close(release)
	if !h.WaitSession("sess", 2*time.Second) {
		t.Fatal("first continue did not finish")
	}
}
