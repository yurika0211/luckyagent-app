package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testPerfDiagnosticsUpload(payload []byte) perfDiagnosticsUpload {
	digest := sha256.Sum256(payload)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return perfDiagnosticsUpload{
		CaptureID:     "capture-test",
		Sequence:      0,
		EndpointHash:  "endpoint-hash",
		SessionHash:   "session-hash",
		AppVersion:    "0.1.67-debug",
		DeviceModel:   "test-device",
		ScreenClass:   "narrow",
		StartedAt:     now,
		EndedAt:       now,
		SampleCount:   2,
		PayloadBase64: base64.StdEncoding.EncodeToString(payload),
		PayloadSHA256: hex.EncodeToString(digest[:]),
	}
}

func TestPerfDiagnosticsStoreIsIdempotent(t *testing.T) {
	store, err := openPerfDiagnosticsStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.close() })

	req := testPerfDiagnosticsUpload([]byte("gzip-jsonl-payload"))
	status, err := store.putBatch(req, time.Now())
	if err != nil || status != "accepted" {
		t.Fatalf("first upload status=%q err=%v", status, err)
	}
	status, err = store.putBatch(req, time.Now())
	if err != nil || status != "duplicate" {
		t.Fatalf("duplicate upload status=%q err=%v", status, err)
	}

	conflict := req
	conflict.PayloadBase64 = base64.StdEncoding.EncodeToString([]byte("different"))
	digest := sha256.Sum256([]byte("different"))
	conflict.PayloadSHA256 = hex.EncodeToString(digest[:])
	if _, err := store.putBatch(conflict, time.Now()); err == nil {
		t.Fatal("expected conflicting duplicate to fail")
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(store.path), perfDiagnosticsDBName)); err != nil {
		t.Fatalf("diagnostics database missing: %v", err)
	}
}

func TestHandlePerfDiagnosticsStoresCompressedPayload(t *testing.T) {
	a := createTestAgent(t)
	s := New(a, DefaultServerConfig())
	t.Cleanup(func() { _ = s.Stop() })

	req := testPerfDiagnosticsUpload([]byte("compressed-jsonl"))
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/diagnostics/perf", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handlePerfDiagnostics(w, httpReq)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	duplicateReq := httptest.NewRequest(http.MethodPost, "/api/v1/diagnostics/perf", bytes.NewReader(body))
	duplicate := httptest.NewRecorder()
	s.handlePerfDiagnostics(duplicate, duplicateReq)
	if duplicate.Code != http.StatusOK || !bytes.Contains(duplicate.Body.Bytes(), []byte(`"duplicate"`)) {
		t.Fatalf("expected duplicate response, got %d: %s", duplicate.Code, duplicate.Body.String())
	}
}

func TestHandlePerfDiagnosticsRejectsWrongMethod(t *testing.T) {
	a := createTestAgent(t)
	s := New(a, DefaultServerConfig())
	t.Cleanup(func() { _ = s.Stop() })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/perf", nil)
	w := httptest.NewRecorder()
	s.handlePerfDiagnostics(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}
