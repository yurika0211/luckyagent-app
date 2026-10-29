package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yurika0211/luckyagent-runtime/provider"
)

func TestArtifactHandlerServesWorkspaceFilesAndRejectsTraversal(t *testing.T) {
	a := createTestAgent(t)
	s := New(a, DefaultServerConfig())
	path := filepath.Join(a.Config().HomeDir(), "workspace", "generated-images", "preview.png")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir artifact directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("png-data"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, s.artifactURLForPath(path), nil)
	recorder := httptest.NewRecorder()
	s.handleArtifact(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("artifact status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != "png-data" {
		t.Fatalf("artifact body = %q, want png-data", recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("content type = %q, want image/png", got)
	}

	traversal := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts?path=workspace%2F..%2Fconfig.json", nil)
	traversalRecorder := httptest.NewRecorder()
	s.handleArtifact(traversalRecorder, traversal)
	if traversalRecorder.Code != http.StatusForbidden {
		t.Fatalf("traversal status = %d, want %d", traversalRecorder.Code, http.StatusForbidden)
	}
}

func TestArtifactHandlerRejectsSymlinkEscape(t *testing.T) {
	a := createTestAgent(t)
	s := New(a, DefaultServerConfig())
	outside := filepath.Join(a.Config().HomeDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(a.Config().HomeDir(), "workspace", "escape.txt")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, s.artifactURLForPath(link), nil)
	rec := httptest.NewRecorder()
	s.handleArtifact(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("symlink status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHistoryMessagesIncludeGeneratedArtifacts(t *testing.T) {
	a := createTestAgent(t)
	s := New(a, DefaultServerConfig())
	path := filepath.Join(a.Config().HomeDir(), "workspace", "generated-images", "history.png")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir artifact directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("png-data"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	messages := s.historyMessages([]provider.Message{{
		Role:    "tool",
		Name:    "image_generate",
		Content: `{"paths":["` + path + `"]}`,
	}})
	if len(messages) != 1 || len(messages[0].Attachments) != 1 {
		t.Fatalf("history attachments = %+v, want one attachment", messages)
	}
	attachment := messages[0].Attachments[0]
	if attachment.FileURL == "" || attachment.Type != "image" || attachment.FileName != "history.png" {
		t.Fatalf("history attachment = %+v", attachment)
	}
}

func TestHistoryMessagesIncludeMediaDirective(t *testing.T) {
	a := createTestAgent(t)
	s := New(a, DefaultServerConfig())
	path := filepath.Join(a.Config().HomeDir(), "workspace", "generated-images", "media-directive.png")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir artifact directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("png-data"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	messages := s.historyMessages([]provider.Message{{
		Role:    "assistant",
		Content: "图片已生成\nMEDIA:" + path,
	}})
	if len(messages) != 1 || len(messages[0].Attachments) != 1 {
		t.Fatalf("history attachments = %+v, want one attachment", messages)
	}
	if messages[0].Content != "图片已生成" {
		t.Fatalf("history content = %q, want media directive removed", messages[0].Content)
	}
}

func TestArtifactHandlerServesServiceWorkingDirectoryFiles(t *testing.T) {
	a := createTestAgent(t)
	s := New(a, DefaultServerConfig())
	workDir, err := os.MkdirTemp(".", ".artifact-cwd-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })
	path := filepath.Join(workDir, "luckyagent-debug.apk")
	if err := os.WriteFile(path, []byte("apk-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}

	// The server emits a stable cwd namespace for attachments generated from
	// its working tree.
	requestURL := s.artifactURLForPath(path)
	if requestURL == "" || !strings.Contains(requestURL, "path=cwd%2F") {
		t.Fatalf("working-directory artifact URL = %q", requestURL)
	}
	recorder := httptest.NewRecorder()
	s.handleArtifact(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "apk-data" {
		t.Fatalf("working-directory artifact response = %d %q", recorder.Code, recorder.Body.String())
	}

	// A MEDIA directive contains an absolute path. Accept it only after the
	// same root check, so clients can derive the URL without knowing cwd.
	absolute := httptest.NewRecorder()
	s.handleArtifact(absolute, httptest.NewRequest(http.MethodGet,
		"/api/v1/artifacts?path="+url.QueryEscape(absolutePath), nil))
	if absolute.Code != http.StatusOK || absolute.Body.String() != "apk-data" {
		t.Fatalf("absolute artifact response = %d %q", absolute.Code, absolute.Body.String())
	}
}
