package websocket

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/session"
	"github.com/yurika0211/luckyagent-runtime/tool"
)

func TestEnqueueBackgroundTaskResultIsIdempotentAndRunsInOwnerSession(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := make(chan string, 2)
	runtime := &stubAgentRuntime{
		sessions: mgr,
		tools:    tool.NewRegistry(),
		chatStreamInputFn: func(_ context.Context, sessionID string, input agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			calls <- sessionID + "\n" + input.RoutingText
			ch := make(chan agent.ChatEvent)
			close(ch)
			return ch, nil
		},
	}
	h := NewAgentHandler(runtime)
	mgr.Ensure("owner")
	if !h.EnqueueBackgroundTaskResult("owner", "task-1", "inspect the release", "completed with evidence") {
		t.Fatal("expected handoff to be queued")
	}
	if h.EnqueueBackgroundTaskResult("owner", "task-1", "inspect the release", "completed with evidence") {
		t.Fatal("duplicate handoff must not create another run")
	}
	select {
	case got := <-calls:
		if !strings.HasPrefix(got, "owner\n") || !strings.Contains(got, "completed with evidence") {
			t.Fatalf("unexpected handoff: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background handoff did not run")
	}
	if !h.WaitSession("owner", 2*time.Second) {
		t.Fatal("handoff run did not finish")
	}
}

func TestSessionBDoesNotSeeSessionATaskResult(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.Ensure("owner-a")
	mgr.Ensure("owner-b")
	calls := make(chan string, 2)
	h := NewAgentHandler(&stubAgentRuntime{
		sessions: mgr, tools: tool.NewRegistry(),
		chatStreamInputFn: func(_ context.Context, sessionID string, input agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			calls <- sessionID + "\n" + input.RoutingText
			ch := make(chan agent.ChatEvent)
			close(ch)
			return ch, nil
		},
	})
	if !h.EnqueueBackgroundTaskResult("owner-a", "task-a", "inspect A", "result only for A") {
		t.Fatal("session A handoff was not queued")
	}
	select {
	case got := <-calls:
		if !strings.HasPrefix(got, "owner-a\n") || !strings.Contains(got, "result only for A") {
			t.Fatalf("session A did not receive its own result: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session A handoff did not run")
	}
	if !h.EnqueueBackgroundTaskResult("owner-b", "task-b", "inspect B", "result only for B") {
		t.Fatal("session B handoff was not queued")
	}
	select {
	case got := <-calls:
		if strings.Contains(got, "result only for A") || !strings.HasPrefix(got, "owner-b\n") {
			t.Fatalf("session B saw session A's result: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session B handoff did not run")
	}
}

func TestHandoffWaitsUntilCurrentReplyFinishes(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.Ensure("owner")
	started := make(chan string, 1)
	release := make(chan struct{})
	h := NewAgentHandler(&stubAgentRuntime{
		sessions: mgr,
		tools:    tool.NewRegistry(),
		chatStreamInputFn: func(ctx context.Context, sessionID string, input agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			ch := make(chan agent.ChatEvent, 1)
			if strings.Contains(input.RoutingText, "background delegated") || strings.Contains(input.RoutingText, "delegated task") {
				started <- input.RoutingText
				ch <- agent.ChatEvent{Type: agent.ChatEventDone, Content: "child arrived"}
				close(ch)
				return ch, nil
			}
			go func() {
				defer close(ch)
				<-release
				ch <- agent.ChatEvent{Type: agent.ChatEventContent, Content: "main still talking"}
			}()
			return ch, nil
		},
	})
	mainDone := make(chan struct{})
	h.mu.Lock()
	h.runners["owner"] = &sessionRunner{active: &queuedRun{id: "main", data: ChatData{Message: "main reply"}}, cancel: func() { close(mainDone) }}
	h.mu.Unlock()
	if !h.EnqueueBackgroundTaskResult("owner", "child-1", "look", "completed with evidence") {
		t.Fatal("child completion was dropped instead of waiting")
	}
	select {
	case <-mainDone:
		t.Fatal("child completion interrupted the current reply")
	default:
	}
	h.mu.Lock()
	queued := len(h.runners["owner"].queue)
	h.runners["owner"].active = nil
	h.runners["owner"].cancel = nil
	h.mu.Unlock()
	if queued != 1 {
		t.Fatalf("handoff queue=%d", queued)
	}
	h.startNext("owner")
	close(release)
	select {
	case got := <-started:
		if !strings.Contains(got, "delegated") {
			t.Fatalf("started text=%q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handoff did not run after the main reply")
	}
}

func TestChildErrorIsNotReportedAsSuccess(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.Ensure("owner")
	calls := make(chan string, 1)
	h := NewAgentHandler(&stubAgentRuntime{
		sessions: mgr, tools: tool.NewRegistry(),
		chatStreamInputFn: func(_ context.Context, _ string, input agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			calls <- input.RoutingText
			ch := make(chan agent.ChatEvent)
			close(ch)
			return ch, nil
		},
	})
	if !h.EnqueueBackgroundTaskResult("owner", "child-err", "look", "status=failed error=compile failed") {
		t.Fatal("error handoff was not queued")
	}
	select {
	case got := <-calls:
		lower := strings.ToLower(got)
		if !strings.Contains(lower, "failed") || strings.Contains(lower, "status=completed") || strings.Contains(lower, "task succeeded") {
			t.Fatalf("error was presented as success: %s", got)
		}
		if !strings.Contains(got, "compile failed") {
			t.Fatalf("error text missing: %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("error handoff did not run")
	}
}

func TestStopMainDoesNotStopChild(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.Ensure("owner")
	mainCtx, mainCancel := context.WithCancel(context.Background())
	childCtx, childCancel := context.WithCancel(context.Background())
	h := NewAgentHandler(&stubAgentRuntime{sessions: mgr, tools: tool.NewRegistry()})
	h.mu.Lock()
	h.runners["owner"] = &sessionRunner{
		active: &queuedRun{id: "main", data: ChatData{Message: "main"}},
		cancel: mainCancel,
		activeRuns: map[string]*queuedRun{
			"main":    {id: "main", data: ChatData{Message: "main"}},
			"child-1": {id: "child-1", child: true, data: ChatData{Message: "child still working"}},
		},
		cancels: map[string]context.CancelFunc{
			"main":    mainCancel,
			"child-1": childCancel,
		},
	}
	h.mu.Unlock()
	stopped := h.cancelSession("owner")
	var stoppedMain, stoppedChild bool
	for _, run := range stopped {
		if run.id == "main" {
			stoppedMain = true
		}
		if run.id == "child-1" {
			stoppedChild = true
		}
	}
	if !stoppedMain {
		t.Fatal("stopping the session did not stop the main task")
	}
	if stoppedChild {
		t.Fatal("stopping the main task stopped the child")
	}
	select {
	case <-mainCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("main task is still running")
	}
	select {
	case <-childCtx.Done():
		t.Fatal("child task was cancelled with the main task")
	default:
	}
	h.mu.Lock()
	child := h.runners["owner"].activeRuns["child-1"]
	h.mu.Unlock()
	if child == nil || !child.child {
		t.Fatal("child task is no longer running")
	}
}

func TestLateHandoffDoesNotRecreateDeletedSession(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner := mgr.Ensure("owner")
	if err := mgr.Delete(owner.ID); err != nil {
		t.Fatal(err)
	}
	h := NewAgentHandler(&stubAgentRuntime{sessions: mgr, tools: tool.NewRegistry()})
	if h.EnqueueBackgroundTaskResult("owner", "task-late", "inspect", "done") {
		t.Fatal("late completion recreated a deleted session")
	}
	if _, ok := mgr.Get("owner"); ok {
		t.Fatal("deleted session exists after late completion")
	}
}
