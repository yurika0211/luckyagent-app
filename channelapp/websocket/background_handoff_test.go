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
