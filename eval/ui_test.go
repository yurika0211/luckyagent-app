package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUIReadsRunWithoutStarting(t *testing.T) {
	ds, hash, err := LoadDataset("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	st := &Store{Root: t.TempDir(), Now: func() time.Time { return time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC) }}
	manifest, err := st.CreateRun(ds, hash, CreateOptions{Profile: "long_task", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartRun(context.Background(), manifest.RunID, passingSampleExecutor()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&UI{Store: st}).Handler())
	defer srv.Close()

	index := get(t, srv.URL+"/")
	if !strings.Contains(index, "Agent Eval") || !strings.Contains(index, "/api/runs") {
		t.Fatalf("index missing app shell: %s", index[:min(200, len(index))])
	}
	listBody := get(t, srv.URL+"/api/runs")
	if !strings.Contains(listBody, manifest.RunID) {
		t.Fatalf("run list: %s", listBody)
	}
	detail := get(t, srv.URL+"/api/runs/"+manifest.RunID)
	var payload map[string]any
	if err := json.Unmarshal([]byte(detail), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["status"] != RunStatusCompleted {
		t.Fatalf("status = %v", payload["status"])
	}
	cases, _ := payload["cases"].([]any)
	if len(cases) != 1 {
		t.Fatalf("cases = %#v", payload["cases"])
	}
	report := get(t, srv.URL+"/api/runs/"+manifest.RunID+"/report")
	if !strings.Contains(report, "sample-memory-correct-resume") || !strings.Contains(report, "execution") {
		t.Fatalf("html report missing case: %s", report[:min(240, len(report))])
	}
	if !strings.Contains(index, "测试资产") || !strings.Contains(index, "/api/datasets/validate") {
		t.Fatal("index is still the read-only shell")
	}

	schema := get(t, srv.URL+"/api/schema")
	if !strings.Contains(schema, "inject_resume") || !strings.Contains(schema, "memory_write") {
		t.Fatalf("schema: %s", schema)
	}

	raw, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	created := postJSON(t, srv.URL+"/api/runs", map[string]any{
		"dataset":    json.RawMessage(raw),
		"profile":    "chat",
		"created_by": "ui-test",
		"start":      false,
	}, http.StatusCreated)
	if created["started"] != false || created["status"] != RunStatusCreated {
		t.Fatalf("create started the run: %#v", created)
	}
	newID, _ := created["run_id"].(string)
	refused := postRaw(t, srv.URL+"/api/runs", string(mustJSON(t, map[string]any{
		"dataset": json.RawMessage(raw),
		"start":   true,
	})))
	if refused.StatusCode != http.StatusBadRequest || !strings.Contains(refused.Body, "start") {
		t.Fatalf("create with start: %d %s", refused.StatusCode, refused.Body)
	}

	script := writeScript(t)
	started := postJSON(t, srv.URL+"/api/runs/"+newID+"/start", map[string]any{
		"executor": "scripted",
		"script":   script,
		"confirm":  true,
	}, http.StatusOK)
	if started["status"] != RunStatusCompleted {
		t.Fatalf("start: %#v", started)
	}
	agentRefused := postRaw(t, srv.URL+"/api/runs/"+newID+"/start", `{"executor":"agent","confirm":true}`)
	if agentRefused.StatusCode != http.StatusBadRequest {
		t.Fatalf("agent executor status = %d", agentRefused.StatusCode)
	}

	reviewed := postJSON(t, srv.URL+"/api/runs/"+manifest.RunID+"/reviews", map[string]any{
		"case_id":  "sample-memory-correct-resume",
		"verdict":  "fail",
		"note":     "回复听起来对，但工具证据不对",
		"reviewer": "ui-test",
	}, http.StatusOK)
	if reviewed["original_verdict"] != string(VerdictPass) || reviewed["verdict"] != string(VerdictFail) {
		t.Fatalf("review: %#v", reviewed)
	}
	reviewedHTML := get(t, srv.URL+"/api/runs/"+manifest.RunID+"/report")
	if !strings.Contains(reviewedHTML, "原判") || !strings.Contains(reviewedHTML, "pass") {
		t.Fatal("report dropped the original verdict")
	}
	attemptsRaw, err := os.ReadFile(filepath.Join(st.Root, "runs", manifest.RunID, "attempts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(attemptsRaw), "工具证据不对") {
		t.Fatal("review rewrote attempts.jsonl")
	}

	legacy := postRaw(t, srv.URL+"/api/datasets/validate", `{"schema_version":"agent-eval/v1","name":"old","success":true,"cases":[{"id":"x","turns":[{"input":"hi"}]}]}`)
	if legacy.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("legacy field status = %d", legacy.StatusCode)
	}
	if !strings.Contains(legacy.Body, "success") {
		t.Fatalf("legacy body: %s", legacy.Body)
	}

	missing, err := http.Get(srv.URL + "/api/runs/missing")
	if err != nil {
		t.Fatal(err)
	}
	missingBody, _ := io.ReadAll(missing.Body)
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing run status = %d", missing.StatusCode)
	}
	if !strings.Contains(string(missingBody), `"error"`) {
		t.Fatalf("missing run body is not json: %s", missingBody)
	}

	noConfirm := postRaw(t, srv.URL+"/api/runs/"+newID+"/start", `{"executor":"scripted","script":"`+script+`"}`)
	if noConfirm.StatusCode != http.StatusBadRequest || !strings.Contains(noConfirm.Body, "confirm") {
		t.Fatalf("start without confirm: %d %s", noConfirm.StatusCode, noConfirm.Body)
	}
	badScript := postRaw(t, srv.URL+"/api/runs/"+newID+"/start", `{"executor":"scripted","script":"/no/such/scripted.json","confirm":true}`)
	if badScript.StatusCode != http.StatusBadRequest || !strings.Contains(badScript.Body, "error") {
		t.Fatalf("bad script: %d %s", badScript.StatusCode, badScript.Body)
	}
	emptyNote := postRaw(t, srv.URL+"/api/runs/"+manifest.RunID+"/reviews", `{"case_id":"sample-memory-correct-resume","verdict":"fail","note":" "}`)
	if emptyNote.StatusCode != http.StatusBadRequest || !strings.Contains(emptyNote.Body, "note") {
		t.Fatalf("empty review note: %d %s", emptyNote.StatusCode, emptyNote.Body)
	}

	if !strings.Contains(index, "人工复核") || !strings.Contains(index, "未勾选确认") || !strings.Contains(index, "消失用例") {
		t.Fatal("index missing operator surface copy")
	}

	after := get(t, srv.URL+"/api/runs/"+manifest.RunID)
	var detailPayload map[string]any
	if err := json.Unmarshal([]byte(after), &detailPayload); err != nil {
		t.Fatal(err)
	}
	casesAfter, _ := detailPayload["cases"].([]any)
	first, _ := casesAfter[0].(map[string]any)
	if first["auto_verdict"] != string(VerdictPass) || first["final_verdict"] != string(VerdictFail) {
		t.Fatalf("auto/final mix: %#v", first)
	}
	if first["execution"] == nil {
		t.Fatalf("execution missing: %#v", first)
	}
	attemptsView, _ := first["attempts"].([]any)
	latestAttempt, _ := attemptsView[0].(map[string]any)
	turns, _ := latestAttempt["turns"].([]any)
	turn0, _ := turns[0].(map[string]any)
	if turn0["input"] == nil || turn0["expected"] == nil {
		t.Fatalf("turn missing input/expected: %#v", turn0)
	}
	checks, _ := turn0["checks"].([]any)
	if len(checks) < 4 {
		t.Fatalf("validators not expanded: %#v", checks)
	}

	cmp := get(t, srv.URL+"/api/compare?run="+newID+"&base="+manifest.RunID)
	if !strings.Contains(cmp, "same_dataset") || !strings.Contains(cmp, "new_dataset_sha256") {
		t.Fatalf("compare missing sha fields: %s", cmp[:min(400, len(cmp))])
	}
	if !strings.Contains(index, "prefers-color-scheme") || !strings.Contains(index, "overflow-wrap") {
		t.Fatal("index missing layout/contrast rules")
	}
}

func mustJSON(t *testing.T, body any) []byte {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func postJSON(t *testing.T, url string, body any, want int) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res := postRaw(t, url, string(raw))
	if res.StatusCode != want {
		t.Fatalf("%s -> %d %s", url, res.StatusCode, res.Body)
	}
	var out map[string]any
	if strings.TrimSpace(res.Body) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
		if want >= 400 {
			return map[string]any{"error": res.Body}
		}
		t.Fatal(err)
	}
	return out
}

type rawResponse struct {
	StatusCode int
	Body       string
}

func postRaw(t *testing.T, url, body string) rawResponse {
	t.Helper()
	res, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return rawResponse{StatusCode: res.StatusCode, Body: string(data)}
}

func writeScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scripted.json")
	raw := `{
	  "turns": {
	    "sample-memory-correct-resume#0": {"response": "我在听，接下来按你的要求记笔记。"},
	    "sample-memory-correct-resume#1": {"response": "已记住周二 09:30，三楼会议室。", "tools": [{"name": "remember"}], "memory_writes": [{"tool": "remember", "content": "晨会周二 09:30 三楼会议室", "ok": true}]},
	    "sample-memory-correct-resume#2": {"response": "已改成周三 10:00，三楼会议室。", "tools": [{"name": "remember"}], "memory_writes": [{"tool": "remember", "content": "晨会周三 10:00 三楼会议室", "ok": true}]},
	    "sample-memory-correct-resume#3": {"response": "依据记忆：周三 10:00 三楼会议室。第一步准备议程。"}
	  }
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func get(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s -> %d %s", url, res.StatusCode, body)
	}
	return string(body)
}
