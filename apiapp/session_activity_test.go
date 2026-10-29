package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionActivityHTTPEmptyWithoutRuns(t *testing.T) {
	agent := createTestAgent(t)
	server := New(agent, DefaultServerConfig())
	agent.Sessions().Ensure("quiet")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/quiet/activity", nil)
	writer := httptest.NewRecorder()
	server.handleSessionByID(writer, req)
	if writer.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", writer.Code, writer.Body.String())
	}
	var payload struct {
		SessionID string            `json:"session_id"`
		Runs      []json.RawMessage `json:"runs"`
	}
	if err := json.Unmarshal(writer.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SessionID != "quiet" || len(payload.Runs) != 0 {
		t.Fatalf("payload = %#v", payload)
	}

	post := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/quiet/activity", nil)
	postWriter := httptest.NewRecorder()
	server.handleSessionByID(postWriter, post)
	if postWriter.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post status = %d", postWriter.Code)
	}
}
