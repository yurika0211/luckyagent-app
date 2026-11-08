package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateRunDefaultsToNotStarted(t *testing.T) {
	repo := filepath.Clean(filepath.Join("..", ".."))
	bin := filepath.Join(t.TempDir(), "la-eval")
	build := exec.Command("go", "build", "-mod=readonly", "-o", bin, "./cmd/la-eval")
	build.Dir = repo
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build la-eval: %v\n%s", err, out)
	}

	runs := t.TempDir()
	cmd := exec.Command(bin, "create-run", "-dataset", filepath.Join(repo, "eval", "testdata", "cases.json"), "-runs-dir", runs)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("create-run: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	var created map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["started"] != false || created["status"] != "created" {
		t.Fatalf("create-run started by default: %s", stdout.String())
	}
	runID, _ := created["run_id"].(string)
	if _, err := os.Stat(filepath.Join(runs, "runs", runID, "attempts.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("default create-run wrote attempts: %v", err)
	}

	script := filepath.Join(repo, "eval", "testdata", "scripted-turns.json")
	start := exec.Command(bin, "create-run",
		"-dataset", filepath.Join(repo, "eval", "testdata", "cases.json"),
		"-runs-dir", runs,
		"-start",
		"-executor", "scripted",
		"-script", script,
	)
	out, err := start.CombinedOutput()
	if err != nil {
		t.Fatalf("explicit -start failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"started": true`) || !strings.Contains(string(out), `"status": "completed"`) {
		t.Fatalf("explicit start did not execute: %s", out)
	}
}

func TestScriptedPipelineWritesSplitVerdictAndHTML(t *testing.T) {
	repo := filepath.Clean(filepath.Join("..", ".."))
	bin := filepath.Join(t.TempDir(), "la-eval")
	build := exec.Command("go", "build", "-mod=readonly", "-o", bin, "./cmd/la-eval")
	build.Dir = repo
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	runs := t.TempDir()
	dataset := filepath.Join(repo, "eval", "testdata", "cases.json")
	script := filepath.Join(repo, "eval", "testdata", "scripted-turns.json")

	base := exec.Command(bin, "create-run", "-dataset", dataset, "-runs-dir", runs, "-start", "-executor", "scripted", "-script", script)
	baseOut, err := base.CombinedOutput()
	if err != nil {
		t.Fatalf("base run: %v\n%s", err, baseOut)
	}
	var baseCreated map[string]any
	if err := json.Unmarshal(lastJSON(baseOut), &baseCreated); err != nil {
		t.Fatalf("decode base: %v\n%s", err, baseOut)
	}
	baseID, _ := baseCreated["run_id"].(string)

	next := exec.Command(bin, "create-run", "-dataset", dataset, "-runs-dir", runs, "-start", "-executor", "scripted", "-script", script)
	nextOut, err := next.CombinedOutput()
	if err != nil {
		t.Fatalf("next run: %v\n%s", err, nextOut)
	}
	var nextCreated map[string]any
	if err := json.Unmarshal(lastJSON(nextOut), &nextCreated); err != nil {
		t.Fatal(err)
	}
	nextID, _ := nextCreated["run_id"].(string)
	htmlPath := filepath.Join(runs, "compare.html")
	report := exec.Command(bin, "report", "-run", nextID, "-compare", baseID, "-runs-dir", runs, "-out", htmlPath)
	reportOut, err := report.CombinedOutput()
	if err != nil {
		t.Fatalf("report: %v\n%s", err, reportOut)
	}
	html, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(html)
	for _, needle := range []string{"execution", "verdict", "sample-memory-correct-resume", "pass", "新增失败", "恢复"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("html missing %q", needle)
		}
	}
	status := exec.Command(bin, "status", "-run", nextID, "-runs-dir", runs)
	statusOut, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, statusOut)
	}
	if !strings.Contains(string(statusOut), `"pass": 1`) || !strings.Contains(string(statusOut), `"completed": 1`) {
		t.Fatalf("status did not split execution and verdict: %s", statusOut)
	}
}

func lastJSON(out []byte) []byte {
	start := bytes.LastIndexByte(out, '{')
	if start < 0 {
		return out
	}
	return out[start:]
}

func TestValidateSampleAndRejectBenchJSONL(t *testing.T) {
	repo := filepath.Clean(filepath.Join("..", ".."))
	bin := filepath.Join(t.TempDir(), "la-eval")
	build := exec.Command("go", "build", "-mod=readonly", "-o", bin, "./cmd/la-eval")
	build.Dir = repo
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "validate", "-dataset", filepath.Join(repo, "eval", "testdata", "cases.json"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("validate sample: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"valid": true`) {
		t.Fatalf("validate output: %s", out)
	}

	legacy := filepath.Join(t.TempDir(), "bench.json")
	if err := os.WriteFile(legacy, []byte(`{"schema_version":"agent-eval/v1","name":"bench","success":true,"cases":[{"id":"x","turns":[{"input":"hi"}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := exec.Command(bin, "validate", "-dataset", legacy)
	out, err = bad.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "legacy bench field \"success\"") {
		t.Fatalf("bench JSONL field was accepted: err=%v out=%s", err, out)
	}
}
