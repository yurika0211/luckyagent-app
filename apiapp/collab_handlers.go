package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/yurika0211/luckyagent-orchestrator/collab"
)

type delegateSubmission struct {
	taskID  string
	payload string
}

func (s *Server) delegateSubmissionOf(id string) (delegateSubmission, bool) {
	if s == nil || strings.TrimSpace(id) == "" {
		return delegateSubmission{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	got, ok := s.delegateSubmissions[id]
	return got, ok
}

func (s *Server) rememberDelegateSubmission(id string, item delegateSubmission) {
	if s == nil || strings.TrimSpace(id) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delegateSubmissions == nil {
		s.delegateSubmissions = map[string]delegateSubmission{}
	}
	s.delegateSubmissions[id] = item
}

func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	s.resourceController().HandleAgentsList(w, r)
}
func (s *Server) handleAgentsGet(w http.ResponseWriter, r *http.Request) {
	s.resourceController().HandleAgentsGet(w, r)
}
func (s *Server) handleAgentsRegister(w http.ResponseWriter, r *http.Request) {
	s.resourceController().HandleAgentsRegister(w, r)
}
func (s *Server) handleAgentsDeregister(w http.ResponseWriter, r *http.Request) {
	s.resourceController().HandleAgentsDeregister(w, r)
}
func (s *Server) handleAgentsDelegate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.resourceController().HandleAgentsDelegate(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.sendError(w, "invalid request body", http.StatusBadRequest, err.Error())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req struct {
		SubmissionID    string   `json:"submission_id"`
		ParentSessionID string   `json:"parent_session_id"`
		Mode            string   `json:"mode"`
		Description     string   `json:"description"`
		Input           string   `json:"input"`
		AgentIDs        []string `json:"agent_ids"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.resourceController().HandleAgentsDelegate(w, r)
		return
	}
	submissionID := strings.TrimSpace(req.SubmissionID)
	if submissionID == "" {
		s.resourceController().HandleAgentsDelegate(w, r)
		return
	}
	sessionID := strings.TrimSpace(req.ParentSessionID)
	if sessionID == "" {
		s.sendError(w, "parent_session_id is required", http.StatusBadRequest, submissionID)
		return
	}
	if s.agent == nil || s.agent.Sessions() == nil {
		s.sendError(w, "session not found", http.StatusNotFound, sessionID)
		return
	}
	if _, ok := s.agent.Sessions().Get(sessionID); !ok {
		s.sendError(w, "session not found", http.StatusNotFound, sessionID)
		return
	}
	payload := delegatePayloadKey(req.Mode, req.Description, req.Input, req.AgentIDs, sessionID)
	if existing, ok := s.delegateSubmissionOf(submissionID); ok {
		if existing.payload != payload {
			s.sendError(w, "submission id was already used with a different payload", http.StatusConflict, submissionID)
			return
		}
		task, found := s.delegateManager.GetTask(existing.taskID)
		if !found {
			s.sendError(w, "task result is not ready", http.StatusConflict, existing.taskID)
			return
		}
		if task.State != collab.TaskCompleted && task.State != collab.TaskFailed && task.State != collab.TaskCancelled {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(task)
			return
		}
		s.sendJSON(w, http.StatusOK, task)
		return
	}
	recorder := &delegateCapture{ResponseWriter: w}
	s.resourceController().HandleAgentsDelegate(recorder, r)
	if recorder.code == http.StatusAccepted {
		var task collab.CollabTask
		if json.Unmarshal(recorder.body.Bytes(), &task) == nil && task.ID != "" {
			s.rememberDelegateSubmission(submissionID, delegateSubmission{taskID: task.ID, payload: payload})
		}
	}
}

func delegatePayloadKey(mode, description, input string, agents []string, sessionID string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.TrimSpace(mode), strings.TrimSpace(description), strings.TrimSpace(input),
		strings.Join(agents, ","), strings.TrimSpace(sessionID),
	}, "\n")))
	return hex.EncodeToString(sum[:])
}

type delegateCapture struct {
	http.ResponseWriter
	code int
	body bytes.Buffer
}

func (c *delegateCapture) WriteHeader(status int) {
	c.code = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *delegateCapture) Write(p []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	c.body.Write(p)
	return c.ResponseWriter.Write(p)
}
func (s *Server) handleAgentsTask(w http.ResponseWriter, r *http.Request) {
	s.resourceController().HandleAgentsTask(w, r)
}
func (s *Server) handleAgentsTasks(w http.ResponseWriter, r *http.Request) {
	s.resourceController().HandleAgentsTasks(w, r)
}
func (s *Server) handleAgentsCancel(w http.ResponseWriter, r *http.Request) {
	s.resourceController().HandleAgentsCancel(w, r)
}
func (s *Server) handleAgentsBlackboard(w http.ResponseWriter, r *http.Request) {
	s.resourceController().HandleAgentsBlackboard(w, r)
}
func (s *Server) handleAgentsMessages(w http.ResponseWriter, r *http.Request) {
	s.resourceController().HandleAgentsMessages(w, r)
}
