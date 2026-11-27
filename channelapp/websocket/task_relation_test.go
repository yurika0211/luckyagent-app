package websocket

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/session"
	"github.com/yurika0211/luckyagent-runtime/tool"
)

type planningRuntime struct {
	stubAgentRuntime
	plan   agent.TaskRelationPlan
	err    error
	planFn func(context.Context, []string, string) (agent.TaskRelationPlan, error)
}

func (p *planningRuntime) PlanTaskRelation(ctx context.Context, active []string, next string) (agent.TaskRelationPlan, error) {
	if p.planFn != nil {
		return p.planFn(ctx, active, next)
	}
	return p.plan, p.err
}

func TestDependencyPlanWaitsAndCancelDoesNotSucceedDependent(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &planningRuntime{
		stubAgentRuntime: stubAgentRuntime{sessions: mgr, tools: tool.NewRegistry()},
		plan:             agent.TaskRelationPlan{Parallel: false, DependsOn: []string{"task-a"}, Reason: "needs A"},
	}
	h := NewAgentHandler(runtime)
	h.mu.Lock()
	h.runners["sess"] = &sessionRunner{active: &queuedRun{id: "task-a", data: ChatData{Message: "task A"}}}
	h.mu.Unlock()
	if h.taskCanRunParallel("sess", "task B uses the result of A") {
		t.Fatal("dependent task was allowed to run in parallel")
	}
	runtime.plan = agent.TaskRelationPlan{Parallel: true, Reason: "independent"}
	if h.taskCanRunParallel("sess", "unrelated formatting") {
		t.Fatal("active turn asked the planner and released the next task")
	}
	h.mu.Lock()
	h.runners["sess"].active = nil
	h.runners["sess"].queue = []*queuedRun{{id: "queued-a", data: ChatData{Message: "task A"}}}
	h.mu.Unlock()
	if !h.taskCanRunParallel("sess", "unrelated formatting") {
		t.Fatal("independent queued task was forced to wait")
	}
	h.mu.Lock()
	h.runners["sess"].cancelledRuns = map[string]bool{"task-a": true}
	h.runners["sess"].active = nil
	h.runners["sess"].queue = []*queuedRun{{id: "task-a", data: ChatData{Message: "task A"}}}
	h.runners["sess"].activeRuns = nil
	h.mu.Unlock()
	runtime.plan = agent.TaskRelationPlan{Parallel: false, DependsOn: []string{"task-a"}, Reason: "needs A"}
	plans := 0
	runtime.planFn = func(context.Context, []string, string) (agent.TaskRelationPlan, error) {
		plans++
		return runtime.plan, runtime.err
	}
	started := make(chan struct{}, 1)
	runtime.chatStreamInputFn = func(context.Context, string, agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
		started <- struct{}{}
		ch := make(chan agent.ChatEvent, 1)
		ch <- agent.ChatEvent{Type: agent.ChatEventDone, Content: "B succeeded"}
		close(ch)
		return ch, nil
	}
	mgr.Ensure("sess")
	client := &Client{SessionID: "sess", Send: make(chan *Message, 8)}
	data, _ := json.Marshal(ChatData{Message: "finish B from A", Stream: true})
	h.HandleMessage(client, &Message{Type: TypeChat, ID: "task-b", SessionID: "sess", Data: data})
	if plans == 0 {
		t.Fatal("idle planner was not asked after A was cancelled")
	}
	if h.taskCanRunParallel("sess", "finish B from A") {
		t.Fatal("cancelled dependency was treated as success")
	}
	select {
	case <-started:
		t.Fatal("dependent task B started after A was cancelled")
	case <-time.After(150 * time.Millisecond):
	}
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case msg := <-client.Send:
			if msg != nil && (msg.Type == TypeStatus || msg.Type == TypeStreamEnd) {
				raw, _ := json.Marshal(msg)
				if strings.Contains(string(raw), "completed") || strings.Contains(string(raw), "B succeeded") {
					t.Fatalf("cancelled dependency marked B successful: %s", raw)
				}
			}
		case <-deadline:
			return
		}
	}
}

func TestNoPlannerWaitsInsteadOfKeywordRelease(t *testing.T) {
	h := NewAgentHandler(&stubAgentRuntime{})
	h.mu.Lock()
	h.runners["sess"] = &sessionRunner{active: &queuedRun{id: "task-a", data: ChatData{Message: "search notes about cats"}}}
	h.mu.Unlock()
	if h.taskCanRunParallel("sess", "search notes about cats and dogs") {
		t.Fatal("missing planner released the next task")
	}
	if h.taskCanRunParallel("sess", "completely different request") {
		t.Fatal("missing planner used a keyword fallback")
	}
}
