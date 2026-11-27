package server

import (
	"net/http"
	"strings"
	"time"

	taskstore "github.com/yurika0211/luckyagent-tools/task"
)

type sessionTaskCard struct {
	ID            string                          `json:"id"`
	ParentID      string                          `json:"parent_id,omitempty"`
	SessionID     string                          `json:"session_id"`
	ExecutionID   string                          `json:"execution_session_id,omitempty"`
	Description   string                          `json:"description"`
	Status        taskstore.Status                `json:"status"`
	CreatedAt     time.Time                       `json:"created_at"`
	StartedAt     time.Time                       `json:"started_at,omitempty"`
	CompletedAt   time.Time                       `json:"completed_at,omitempty"`
	Workspace     string                          `json:"workspace,omitempty"`
	ResultSummary string                          `json:"result_summary,omitempty"`
	Result        string                          `json:"result,omitempty"`
	Unread        bool                            `json:"unread"`
	NeedsAction   bool                            `json:"needs_action"`
	CurrentStep   string                          `json:"current_step,omitempty"`
	LastActivity  time.Time                       `json:"last_activity,omitempty"`
	Observation   *taskstore.MainAgentObservation `json:"observation,omitempty"`
}

type sessionTaskSummary struct {
	Active int
	Unread int
}

func (s *Server) sessionTasks(sessionID string) ([]sessionTaskCard, sessionTaskSummary, error) {
	if s == nil || s.agent == nil || s.agent.TaskStore() == nil {
		return nil, sessionTaskSummary{}, nil
	}
	records, err := s.agent.TaskStore().List(taskstore.ListFilter{Source: taskstore.SourceTool, Limit: 10000})
	if err != nil {
		return nil, sessionTaskSummary{}, err
	}
	var cards []sessionTaskCard
	var summary sessionTaskSummary
	for _, record := range records {
		if strings.TrimSpace(record.ParentSessionID) != sessionID {
			continue
		}
		terminal := record.Status == taskstore.StatusCompleted || record.Status == taskstore.StatusFailed || record.Status == taskstore.StatusBlocked || record.Status == taskstore.StatusCancelled
		handledAt := strings.TrimSpace(record.Metadata["task_handled_at"])
		resultAt := strings.TrimSpace(record.Metadata["result_at"])
		unread := handledAt == "" || (resultAt != "" && resultAt > handledAt)
		needsAction := record.Status == taskstore.StatusPending || record.Status == taskstore.StatusRunning || record.Status == taskstore.StatusBlocked || (terminal && unread)
		if record.Status == taskstore.StatusPending || record.Status == taskstore.StatusRunning {
			summary.Active++
		}
		if unread && needsAction {
			summary.Unread++
		}
		result, _, _ := s.agent.TaskStore().Result(record.ID)
		events, _ := s.agent.TaskStore().Events(record.ID)
		observation := taskstore.ReduceObservation(record, events)
		step := strings.TrimSpace(record.Metadata["current_step"])
		if step == "" {
			step = string(record.Status)
		}
		last := record.CompletedAt
		if last.IsZero() {
			last = record.StartedAt
		}
		if last.IsZero() {
			last = record.CreatedAt
		}
		cards = append(cards, sessionTaskCard{
			ID: record.ID, ParentID: record.ParentID, SessionID: record.ParentSessionID, ExecutionID: record.ExecutionSessionID,
			Description: record.Description, Status: record.Status, CreatedAt: record.CreatedAt, StartedAt: record.StartedAt,
			CompletedAt: record.CompletedAt, Workspace: record.Runtime.Workspace, ResultSummary: record.Metadata["result_summary"],
			Result: result, Unread: unread, NeedsAction: needsAction, CurrentStep: step, LastActivity: last, Observation: &observation,
		})
	}
	return cards, summary, nil
}

func (s *Server) handleSessionTasks(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		s.sendError(w, "method not allowed", http.StatusMethodNotAllowed, "")
		return
	}
	cards, summary, err := s.sessionTasks(sessionID)
	if err != nil {
		s.sendError(w, "list session tasks failed", http.StatusInternalServerError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"session_id": sessionID, "tasks": cards, "count": len(cards), "active": summary.Active, "unread": summary.Unread})
}

func (s *Server) markSessionTasksRead(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		s.sendError(w, "method not allowed", http.StatusMethodNotAllowed, "")
		return
	}
	var body struct {
		TaskIDs []string `json:"task_ids"`
	}
	if err := jsonAPI.NewDecoder(r.Body).Decode(&body); err != nil {
		s.sendError(w, "invalid request body", http.StatusBadRequest, err.Error())
		return
	}
	allowed := make(map[string]struct{}, len(body.TaskIDs))
	for _, id := range body.TaskIDs {
		if id = strings.TrimSpace(id); id != "" {
			allowed[id] = struct{}{}
		}
	}
	cards, _, err := s.sessionTasks(sessionID)
	if err != nil {
		s.sendError(w, "list session tasks failed", http.StatusInternalServerError, err.Error())
		return
	}
	marked := 0
	for _, card := range cards {
		if len(allowed) > 0 {
			if _, ok := allowed[card.ID]; !ok {
				continue
			}
		}
		record, ok, getErr := s.agent.TaskStore().Get(card.ID)
		if getErr != nil || !ok {
			continue
		}
		if record.Metadata == nil {
			record.Metadata = map[string]string{}
		}
		record.Metadata["task_handled_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		if err := s.agent.TaskStore().Update(record); err != nil {
			continue
		}
		marked++
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"session_id": sessionID, "marked": marked})
}
