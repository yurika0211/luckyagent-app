package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yurika0211/luckyagent-orchestrator/collab"
)

func TestHTTPDelegateSubmissionDedup(t *testing.T) {
	agent := createTestAgent(t)
	sess := agent.Sessions().Ensure("owner")
	reg := collab.NewRegistry()
	if err := reg.Register(&collab.AgentProfile{ID: "agent-1", Name: "Agent 1"}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	handler := collab.TaskHandlerFunc(func(ctx context.Context, task *collab.SubTask) (string, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
			return "processed: " + task.Input, nil
		}
	})
	s := &Server{
		agent:           agent,
		collabRegistry:  reg,
		delegateManager: collab.NewDelegateManager(reg, handler),
	}
	payload := map[string]any{
		"submission_id": "sub-1", "mode": "parallel", "description": "notes",
		"input": "hello", "agent_ids": []string{"agent-1"}, "parent_session_id": sess.ID, "timeout": 30000000000,
	}
	first := postDelegate(t, s, payload)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status %d body %s", first.Code, first.Body.String())
	}
	var created collab.CollabTask
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("first body: %s err=%v", first.Body.String(), err)
	}
	again := postDelegate(t, s, payload)
	if again.Code != http.StatusOK {
		t.Fatalf("retry status %d body %s", again.Code, again.Body.String())
	}
	var retried struct {
		Task     collab.CollabTask `json:"task"`
		Original bool              `json:"original"`
		Ready    bool              `json:"ready"`
	}
	if err := json.Unmarshal(again.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if !retried.Original || retried.Task.ID != created.ID {
		t.Fatalf("retry created %s, original %s body %s", retried.Task.ID, created.ID, again.Body.String())
	}
	if retried.Ready || delegateResultReady(retried.Task.State) {
		t.Fatalf("unfinished retry looked ready: %s", again.Body.String())
	}
	if len(s.delegateManager.ListTasks()) != 1 {
		t.Fatalf("tasks=%d", len(s.delegateManager.ListTasks()))
	}
	changed := map[string]any{}
	for key, value := range payload {
		changed[key] = value
	}
	changed["input"] = "different"
	conflict := postDelegate(t, s, changed)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("changed payload status %d body %s", conflict.Code, conflict.Body.String())
	}
	missing := map[string]any{}
	for key, value := range payload {
		missing[key] = value
	}
	missing["parent_session_id"] = ""
	missing["submission_id"] = "sub-missing"
	if code := postDelegate(t, s, missing).Code; code != http.StatusBadRequest {
		t.Fatalf("missing session status %d", code)
	}
	deleted := map[string]any{}
	for key, value := range payload {
		deleted[key] = value
	}
	deleted["parent_session_id"] = "gone"
	deleted["submission_id"] = "sub-gone"
	if code := postDelegate(t, s, deleted).Code; code != http.StatusNotFound {
		t.Fatalf("deleted session status %d", code)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("delegate did not start")
	}
	notReady := getDelegateTask(t, s, created.ID)
	if notReady.Code != http.StatusAccepted {
		t.Fatalf("result status %d body %s", notReady.Code, notReady.Body.String())
	}
	var pending struct {
		Ready  bool              `json:"ready"`
		Status string            `json:"status"`
		Task   collab.CollabTask `json:"task"`
	}
	if err := json.Unmarshal(notReady.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Ready || pending.Status != "not_ready" || pending.Task.ID != created.ID || delegateResultReady(pending.Task.State) {
		t.Fatalf("unfinished result looked ready: %s", notReady.Body.String())
	}
	close(release)
}

func delegateResultReady(state collab.TaskState) bool {
	return state == collab.TaskCompleted || state == collab.TaskFailed || state == collab.TaskCancelled || state == collab.TaskTimeout
}

func getDelegateTask(t *testing.T, s *Server, taskID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/task?id="+taskID, nil)
	w := httptest.NewRecorder()
	s.handleAgentsTask(w, req)
	return w
}

func postDelegate(t *testing.T, s *Server, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/delegate", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleAgentsDelegate(w, req)
	return w
}
