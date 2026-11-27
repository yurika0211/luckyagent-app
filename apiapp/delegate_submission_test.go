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
	handler := collab.TaskHandlerFunc(func(ctx context.Context, task *collab.SubTask) (string, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(300 * time.Millisecond):
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
		"input": "hello", "agent_ids": []string{"agent-1"}, "parent_session_id": sess.ID, "timeout": 5,
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
	if again.Code != http.StatusAccepted && again.Code != http.StatusOK {
		t.Fatalf("retry status %d body %s", again.Code, again.Body.String())
	}
	var retried collab.CollabTask
	if err := json.Unmarshal(again.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.ID != created.ID {
		t.Fatalf("retry created %s, original %s", retried.ID, created.ID)
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
	notReady := postDelegate(t, s, payload)
	if notReady.Code == http.StatusOK {
		var done collab.CollabTask
		_ = json.Unmarshal(notReady.Body.Bytes(), &done)
		if done.State != collab.TaskCompleted && done.State != collab.TaskFailed {
			t.Fatalf("unfinished retry looked ready: %s", notReady.Body.String())
		}
	}
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
