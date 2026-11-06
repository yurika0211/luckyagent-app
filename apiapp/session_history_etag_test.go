package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yurika0211/luckyagent-runtime/provider"
)

func TestSessionHistoryNotModifiedSkipsBodies(t *testing.T) {
	const id = "history-etag"
	server := seedHistorySession(t, id, 8)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+id+"?limit=40", nil)
	writer := httptest.NewRecorder()
	server.handleSessionByID(writer, req)
	if writer.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", writer.Code, writer.Body.String())
	}
	etag := writer.Header().Get("ETag")
	if etag == "" || !json.Valid(writer.Body.Bytes()) {
		t.Fatalf("etag=%q body=%s", etag, writer.Body.String())
	}
	var page historyPage
	if err := json.Unmarshal(writer.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) == 0 {
		t.Fatal("first GET omitted messages")
	}

	again := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+id+"?limit=40", nil)
	again.Header.Set("If-None-Match", "W/"+etag+", \"other\"")
	second := httptest.NewRecorder()
	server.handleSessionByID(second, again)
	if second.Code != http.StatusNotModified {
		t.Fatalf("status=%d body=%s", second.Code, second.Body.String())
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 body=%s", second.Body.String())
	}
	if second.Header().Get("ETag") != etag {
		t.Fatalf("304 etag=%q want %q", second.Header().Get("ETag"), etag)
	}
}

func TestSessionHistoryETagChangesAfterNewMessage(t *testing.T) {
	const id = "history-etag-new"
	server := seedHistorySession(t, id, 3)
	etag := historyGET(t, server, id, "?limit=40").etag

	sess, ok := server.agent.Sessions().Get(id)
	if !ok {
		t.Fatal("session missing")
	}
	sess.AddProviderMessage(provider.Message{Role: "assistant", Content: "new answer"})

	got := historyGET(t, server, id, "?limit=40", etag)
	if got.status != http.StatusOK {
		t.Fatalf("status=%d body=%s", got.status, got.body)
	}
	if got.etag == etag || got.etag == "" {
		t.Fatalf("etag stayed %q", got.etag)
	}
	var page historyPage
	if err := json.Unmarshal([]byte(got.body), &page); err != nil || len(page.Messages) == 0 {
		t.Fatalf("messages missing: %v %s", err, got.body)
	}
}

func TestSessionHistoryFullIncludeHasDistinctETag(t *testing.T) {
	const id = "history-etag-full"
	server := seedHistorySession(t, id, 2)
	preview := historyGET(t, server, id, "?limit=40")
	full := historyGET(t, server, id, "?limit=40&include=full", preview.etag)
	if full.status != http.StatusOK {
		t.Fatalf("status=%d", full.status)
	}
	if full.etag == "" || full.etag == preview.etag {
		t.Fatalf("full etag=%q preview=%q", full.etag, preview.etag)
	}
}

func TestSessionHistoryCountsOmitsBodies(t *testing.T) {
	const id = "history-etag-counts"
	server := seedHistorySession(t, id, 5)
	got := historyGET(t, server, id, "?limit=40&counts=1")
	if got.status != http.StatusOK {
		t.Fatalf("status=%d body=%s", got.status, got.body)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(got.body), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["message_count"] == nil || raw["updated_at"] == nil {
		t.Fatalf("counts missing: %s", got.body)
	}
	messages, _ := raw["messages"].([]any)
	if len(messages) != 0 {
		t.Fatalf("messages=%v", raw["messages"])
	}
	if raw["has_more"] != false {
		t.Fatalf("has_more=%v", raw["has_more"])
	}
	if got.etag == "" {
		t.Fatal("counts response missing etag")
	}
}

func TestSessionHistoryPageETagDoesNotMatchNextOffset(t *testing.T) {
	const id = "history-etag-page"
	server := seedHistorySession(t, id, 80)
	first := historyGET(t, server, id, "?limit=40")
	next := historyGET(t, server, id, "?limit=40&offset=40", first.etag)
	if next.status != http.StatusOK {
		t.Fatalf("status=%d body=%s", next.status, next.body)
	}
	if next.etag == first.etag {
		t.Fatal("page 1 reused page 0 etag")
	}
}

type historyHTTP struct {
	status int
	etag   string
	body   string
}

func historyGET(t *testing.T, server *Server, id, query string, noneMatch ...string) historyHTTP {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+id+query, nil)
	if len(noneMatch) > 0 && noneMatch[0] != "" {
		req.Header.Set("If-None-Match", noneMatch[0])
	}
	writer := httptest.NewRecorder()
	server.handleSessionByID(writer, req)
	return historyHTTP{status: writer.Code, etag: writer.Header().Get("ETag"), body: writer.Body.String()}
}
