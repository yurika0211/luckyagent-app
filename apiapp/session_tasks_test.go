package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	taskstore "github.com/yurika0211/luckyagent-tools/task"
)

func TestSessionTaskCardKeepsUnreadUntilNewerResultIsSeen(t *testing.T) {
	agent := createTestAgent(t)
	record, err := agent.TaskStore().Create(taskstore.Record{
		ID: "task-card", ParentSessionID: "sess-1", Source: taskstore.SourceTool,
		Status: taskstore.StatusRunning, Description: "review the patch",
		CreatedAt: time.Now().Add(-time.Minute), StartedAt: time.Now().Add(-time.Second),
		Metadata: map[string]string{"current_step": "running tests"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{agent: agent}
	cards, summary, err := s.sessionTasks("sess-1")
	if err != nil || len(cards) != 1 {
		t.Fatalf("cards=%d err=%v", len(cards), err)
	}
	if cards[0].CurrentStep != "running tests" || cards[0].LastActivity.IsZero() || !cards[0].Unread {
		t.Fatalf("card=%+v", cards[0])
	}
	if summary.Unread != 1 {
		t.Fatalf("unread=%d", summary.Unread)
	}
	body := bytes.NewBufferString(`{"task_ids":["task-card"]}`)
	req := httptest.NewRequest(http.MethodPost, "/read", body)
	rec := httptest.NewRecorder()
	s.markSessionTasksRead(rec, req, "sess-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("mark read: %d %s", rec.Code, rec.Body.String())
	}
	record.Status = taskstore.StatusCompleted
	record.CompletedAt = time.Now()
	record.Metadata["result_at"] = record.CompletedAt.UTC().Format(time.RFC3339Nano)
	record.Metadata["current_step"] = "completed"
	if err := agent.TaskStore().Update(record); err != nil {
		t.Fatal(err)
	}
	cards, _, err = s.sessionTasks("sess-1")
	if err != nil || !cards[0].Unread {
		t.Fatalf("older read cleared a newer result: %+v err=%v", cards, err)
	}
	view := httptest.NewRecorder()
	s.handleSessionTasks(view, httptest.NewRequest(http.MethodGet, "/tasks", nil), "sess-1")
	var payload struct {
		Tasks []sessionTaskCard `json:"tasks"`
	}
	if err := json.Unmarshal(view.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Tasks) != 1 || !payload.Tasks[0].Unread || payload.Tasks[0].CurrentStep == "" {
		t.Fatalf("view cleared unread: %+v", payload.Tasks)
	}
}
