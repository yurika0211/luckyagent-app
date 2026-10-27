package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionRecapListDetailAndPatch(t *testing.T) {
	a := createTestAgent(t)
	s := New(a, DefaultServerConfig())
	sess := a.Sessions().Ensure("recap-test")
	sess.AddMessage("user", "question")
	sess.AddMessage("assistant", "answer")
	if err := sess.Save(); err != nil {
		t.Fatal(err)
	}
	_, _, version, err := sess.RecapSnapshot(10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.UpdateRecap("你问了 question。", version); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/api/v1/sessions", "/api/v1/sessions?q=question", "/api/v1/sessions/recap-test?limit=1"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, route, nil)
		if strings.Contains(route, "recap-test") {
			s.handleSessionByID(w, r)
		} else {
			s.handleSessions(w, r)
		}
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if rows, ok := payload["sessions"].([]any); ok {
			payload = rows[0].(map[string]any)
		}
		if payload["recap"] != "你问了 question。" || payload["recap_updated_at"] == nil {
			t.Fatal(payload)
		}
	}
	w := httptest.NewRecorder()
	s.handleSessionByID(w, httptest.NewRequest(http.MethodPatch, "/api/v1/sessions/recap-test", strings.NewReader(`{"title":"Manual title"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"recap":"你问了 question。"`) || sess.Title != "Manual title" {
		t.Fatal(w.Code, w.Body.String())
	}
}
