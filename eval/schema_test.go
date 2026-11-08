package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSampleDatasetLoads(t *testing.T) {
	ds, hash, err := LoadDataset(filepath.Join("testdata", "cases.json"))
	if err != nil {
		t.Fatalf("load sample: %v", err)
	}
	if ds.SchemaVersion != SchemaVersion || ds.Name == "" || hash == "" {
		t.Fatalf("sample identity missing: %+v hash=%s", ds.SchemaVersion, hash)
	}
	if len(ds.Cases) != 1 {
		t.Fatalf("first cut keeps one full sample, got %d", len(ds.Cases))
	}
	c := ds.Cases[0]
	if c.EffectiveMode(ds) != ModeReplayResume {
		t.Fatalf("sample mode = %s", c.EffectiveMode(ds))
	}
	if c.EffectiveProfile(ds) != "long_task" {
		t.Fatalf("sample profile = %s", c.EffectiveProfile(ds))
	}
	wantCoverage := []string{"chat", "tool_route", "memory_write", "long_task"}
	if strings.Join(c.Coverage, ",") != strings.Join(wantCoverage, ",") {
		t.Fatalf("coverage = %#v", c.Coverage)
	}
	if len(c.Turns) != 4 {
		t.Fatalf("sample turns = %d, want 4", len(c.Turns))
	}
	if c.Turns[0].Evaluation.Keyword == nil || c.Turns[0].Evaluation.Trace == nil {
		t.Fatal("turn 1 must show keyword + trace")
	}
	if c.Turns[1].Evaluation.MemoryWrite == nil || !c.Turns[1].Evaluation.MemoryWrite.MustWrite {
		t.Fatal("turn 2 must require a memory write")
	}
	if c.Turns[2].Evaluation.Regex == nil || c.Turns[2].Evaluation.MemoryWrite == nil {
		t.Fatal("turn 3 must show regex + corrected memory write")
	}
	if got := c.TurnIndexes(ds); len(got) != 4 {
		t.Fatalf("replay indexes = %#v", got)
	}
}

func TestLegacyBenchFieldsRejected(t *testing.T) {
	raw := []byte(`{
	  "schema_version": "agent-eval/v1",
	  "name": "imported",
	  "type": "record",
	  "cases": [{"id": "x", "turns": [{"input": "hi"}]}]
	}`)
	_, err := DecodeDataset(raw)
	if err == nil || !strings.Contains(err.Error(), "legacy bench field \"type\"") {
		t.Fatalf("expected legacy rejection, got %v", err)
	}
}

func TestInjectResumeForbidden(t *testing.T) {
	raw := []byte(`{
	  "schema_version": "agent-eval/v1",
	  "name": "bad",
	  "cases": [{
	    "id": "inject-resume",
	    "mode": "inject_resume",
	    "turns": [{"input": "a"}, {"input": "b"}]
	  }]
	}`)
	_, err := DecodeDataset(raw)
	if err == nil || !strings.Contains(err.Error(), "inject_resume is forbidden") {
		t.Fatalf("expected forbidden mode, got %v", err)
	}
}

func TestInjectAbortEvaluatesLastTurnOnly(t *testing.T) {
	ds := &Dataset{Defaults: &Defaults{Mode: ModeInjectAbort}}
	c := Case{ID: "late", Turns: []Turn{{Input: "earlier"}, {Input: "earlier-2"}, {Input: "ask"}}}
	got := c.TurnIndexes(ds)
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("inject_abort indexes = %#v, want [2]", got)
	}
}

func TestEditingCaseChangesHash(t *testing.T) {
	ds, hash, err := LoadDataset(filepath.Join("testdata", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := DatasetHash(ds)
	if err != nil || again != hash {
		t.Fatalf("hash unstable: %s vs %s (%v)", hash, again, err)
	}
	ds.Cases[0].Turns[0].Input += "。"
	changed, err := DatasetHash(ds)
	if err != nil {
		t.Fatal(err)
	}
	if changed == hash {
		t.Fatal("editing a turn must change the frozen dataset hash")
	}
}

func TestMismatchDocExists(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "bench-jsonl-mismatch.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"success", "quality_pass", "required_tools", "recall_at_k", "called_tools", "duration_ns"} {
		if !strings.Contains(string(data), "`"+col+"`") {
			t.Fatalf("mismatch doc missing column %s", col)
		}
	}
}
