package evalplatform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportRejectsPromptOnlyCase(t *testing.T) {
	_, err := ParseDataset([]byte(`{"name":"old","cases":[{"id":"a","prompt":"hello"}]}`))
	if err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("err=%v", err)
	}
}

func TestCreateRunDoesNotStart(t *testing.T) {
	dir := t.TempDir()
	run, err := CreateRun(samplePath(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	if run.Started {
		t.Fatal("create-run started the run")
	}
	if len(run.Cases) != 1 || run.Cases[0].Verdict != "" || run.Cases[0].Execution != "" {
		t.Fatalf("verdicts were filled: %+v", run.Cases)
	}
	loaded, err := ReadRun(dir)
	if err != nil || loaded.Started {
		t.Fatalf("disk started=%v err=%v", loaded.Started, err)
	}
}

func TestStartSetsExecutionAndDoesNotPassUnconfigured(t *testing.T) {
	dir := t.TempDir()
	if _, err := CreateRun(samplePath(t), dir); err != nil {
		t.Fatal(err)
	}
	bare := dir + "-bare"
	raw := []byte(`{"name":"bare","cases":[{"id":"no-eval","input":{"user":"hi"},"expected":{},"evaluation":{},"wait":{}}]}`)
	path := filepath.Join(t.TempDir(), "bare.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateRun(path, bare); err != nil {
		t.Fatal(err)
	}
	run, err := Start(bare, scriptExecutor{execution: ExecutionCompleted, evidence: []Evidence{{
		Assistant: "hi", ToolsSet: true, MemorySet: true,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !run.Started || run.Cases[0].Execution != ExecutionCompleted {
		t.Fatalf("run=%+v", run.Cases[0])
	}
	if run.Cases[0].Verdict != VerdictSkip {
		t.Fatalf("unconfigured verdict=%s", run.Cases[0].Verdict)
	}
}

func TestForbiddenToolFailsEvenWhenKeywordsMatch(t *testing.T) {
	eval := Eval{
		Keyword: &KeywordEval{Contains: []string{"dark mode"}},
		Trace:   &TraceEval{Required: []string{"remember"}, Forbidden: []string{"terminal"}},
	}
	got := Judge(eval, Evidence{
		Assistant: "saved dark mode",
		Tools:     []string{"remember", "terminal"},
		ToolsSet:  true,
	}, ExecutionCompleted)
	if got != VerdictFail {
		t.Fatalf("verdict=%s", got)
	}
}

func TestReportKeepsFrozenInputAfterDatasetRewrite(t *testing.T) {
	root := t.TempDir()
	dataset := filepath.Join(root, "cases.json")
	src, err := os.ReadFile(samplePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataset, src, 0o644); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(root, "run")
	if _, err := CreateRun(dataset, runDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(runDir, SampleExecutor{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataset, []byte(`{"name":"rewritten","cases":[{"id":"other","input":{"user":"LIVE"},"expected":{},"evaluation":{},"wait":{}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	htmlPath := filepath.Join(root, "report.html")
	if err := WriteReport(runDir, htmlPath); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "Read the note and remember that I prefer dark mode.") {
		t.Fatalf("frozen input missing:\n%s", text)
	}
	if strings.Contains(text, "LIVE") {
		t.Fatalf("report followed the live dataset:\n%s", text)
	}
	if !strings.Contains(text, ">pass<") && !strings.Contains(text, "pass") {
		t.Fatalf("sample verdict missing:\n%s", text)
	}
}

func TestSampleStartPasses(t *testing.T) {
	dir := t.TempDir()
	if _, err := CreateRun(samplePath(t), dir); err != nil {
		t.Fatal(err)
	}
	run, err := Start(dir, SampleExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Cases[0].Execution != ExecutionCompleted || run.Cases[0].Verdict != VerdictPass {
		t.Fatalf("verdicts=%s %s assistant=%q tools=%v mem=%v", run.Cases[0].Turns[0].Verdict, run.Cases[0].Turns[1].Verdict, run.Cases[0].Turns[0].Evidence.Assistant, run.Cases[0].Turns[0].Evidence.Tools, run.Cases[0].Turns[0].Evidence.MemoryWrites)
	}
	if len(run.Cases[0].Turns) != 2 {
		t.Fatalf("turns=%d", len(run.Cases[0].Turns))
	}
}

type scriptExecutor struct {
	execution string
	evidence  []Evidence
}

func (s scriptExecutor) Execute(Case) (string, []Evidence, error) {
	return s.execution, s.evidence, nil
}

func samplePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "sample-chat-tool-memory.json")
}
