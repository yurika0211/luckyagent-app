package websocket

import (
	"context"
	"testing"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/session"
	"github.com/yurika0211/luckyagent-runtime/tool"
)

type planningRuntime struct {
	stubAgentRuntime
	plan agent.TaskRelationPlan
	err  error
}

func (p *planningRuntime) PlanTaskRelation(context.Context, []string, string) (agent.TaskRelationPlan, error) {
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
	if !h.taskCanRunParallel("sess", "unrelated formatting") {
		t.Fatal("independent task was forced to wait")
	}
	h.mu.Lock()
	h.runners["sess"].cancelledRuns = map[string]bool{"task-a": true}
	h.mu.Unlock()
	runtime.plan = agent.TaskRelationPlan{Parallel: false, DependsOn: []string{"task-a"}}
	if h.taskCanRunParallel("sess", "finish B from A") {
		t.Fatal("cancelled dependency was treated as success")
	}
}
