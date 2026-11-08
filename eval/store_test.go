package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateRunDoesNotExecuteUntilStart(t *testing.T) {
	ds, hash, err := LoadDataset("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	st := &Store{Root: root, Now: func() time.Time { return time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC) }}
	manifest, err := st.CreateRun(ds, hash, CreateOptions{Profile: "long_task", Workspace: ".", CreatedBy: "test", StartRequested: false})
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.LoadState(manifest.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != RunStatusCreated || state.Attempts != 0 {
		t.Fatalf("create-run executed or changed status: %+v", state)
	}
	if _, err := os.Stat(filepath.Join(root, "runs", manifest.RunID, "attempts.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("attempts file exists before start: %v", err)
	}
	if manifest.StartRequested {
		t.Fatal("default create recorded start_requested=true")
	}
}

func TestEditedDatasetDoesNotChangeFrozenRun(t *testing.T) {
	ds, hash, err := LoadDataset("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	st := &Store{Root: root}
	manifest, err := st.CreateRun(ds, hash, CreateOptions{CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	frozenHash := manifest.DatasetSHA256
	frozenInput := manifest.Dataset.Cases[0].Turns[0].Input

	ds.Cases[0].Turns[0].Input = "改写后的新清单"
	ds.Cases[0].Turns[0].Evaluation = nil
	newHash, err := DatasetHash(ds)
	if err != nil {
		t.Fatal(err)
	}
	if newHash == frozenHash {
		t.Fatal("edited dataset hash did not change")
	}
	second, err := st.CreateRun(ds, newHash, CreateOptions{CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}

	reloaded, err := st.LoadManifest(manifest.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.DatasetSHA256 != frozenHash || reloaded.Dataset.Cases[0].Turns[0].Input != frozenInput {
		t.Fatalf("old run changed after dataset edit: hash=%s input=%s", reloaded.DatasetSHA256, reloaded.Dataset.Cases[0].Turns[0].Input)
	}
	if second.DatasetSHA256 != newHash || second.Dataset.Cases[0].Turns[0].Input != "改写后的新清单" {
		t.Fatal("new run did not freeze the edited dataset")
	}

	exec := passingSampleExecutor()
	if _, err := st.StartRun(context.Background(), manifest.RunID, exec); err != nil {
		t.Fatal(err)
	}
	attempts, err := st.Attempts(manifest.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].DatasetSHA256 != frozenHash {
		t.Fatalf("started old run with new manifest: %+v", attempts)
	}
	if !strings.Contains(attempts[0].Turns[0].Input, "先不用工具") {
		t.Fatalf("old run executed the edited input: %s", attempts[0].Turns[0].Input)
	}
}

func TestCompareReportNewFailureAndRecovery(t *testing.T) {
	baseDS := &Dataset{
		SchemaVersion: SchemaVersion,
		Name:          "compare",
		Cases: []Case{
			{ID: "stable", Title: "保持通过", Turns: []Turn{{Input: "a", Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"通过"}}}}}},
			{ID: "regressed", Title: "新增失败", Turns: []Turn{{Input: "b", Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"通过"}}}}}},
			{ID: "recovered", Title: "恢复", Turns: []Turn{{Input: "c", Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"通过"}}}}}},
		},
	}
	hash, err := DatasetHash(baseDS)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	st := &Store{Root: root}
	base, err := st.CreateRun(baseDS, hash, CreateOptions{CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartRun(context.Background(), base.RunID, &ScriptedExecutor{Turns: map[string]ScriptedTurn{
		"stable#0":    {Result: TextResult("通过")},
		"regressed#0": {Result: TextResult("通过")},
		"recovered#0": {Result: TextResult("没有关键字")},
	}}); err != nil {
		t.Fatal(err)
	}

	nextDS := &Dataset{
		SchemaVersion: SchemaVersion,
		Name:          "compare",
		Cases: []Case{
			{ID: "stable", Title: "保持通过", Turns: []Turn{{Input: "a", Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"通过"}}}}}},
			{ID: "regressed", Title: "新增失败", Turns: []Turn{{Input: "b changed", Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"通过"}}}}}},
			{ID: "recovered", Title: "恢复", Turns: []Turn{{Input: "c", Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"通过"}}}}}},
			{ID: "added", Title: "新题", Turns: []Turn{{Input: "d"}}},
		},
	}
	nextHash, err := DatasetHash(nextDS)
	if err != nil {
		t.Fatal(err)
	}
	next, err := st.CreateRun(nextDS, nextHash, CreateOptions{CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartRun(context.Background(), next.RunID, &ScriptedExecutor{Turns: map[string]ScriptedTurn{
		"stable#0":    {Result: TextResult("通过")},
		"regressed#0": {Result: TextResult("跑完了但是错的")},
		"recovered#0": {Result: TextResult("通过")},
		"added#0":     {Result: TextResult("新题没有验证器")},
	}}); err != nil {
		t.Fatal(err)
	}

	// A second attempt stays in JSONL; the report must keep only the latest.
	extra := Attempt{
		RunID: next.RunID, CaseID: "regressed", Attempt: 2,
		Execution: ExecutionCompleted, Verdict: VerdictFail, Reason: "latest failure",
		DatasetSHA256: nextHash,
	}
	if err := appendAttempt(filepath.Join(root, "runs", next.RunID, "attempts.jsonl"), extra); err != nil {
		t.Fatal(err)
	}

	rep, err := st.WriteReport(next.RunID, base.RunID, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Attempts != 5 {
		t.Fatalf("JSONL attempts = %d, want 5 (all attempts kept)", rep.Attempts)
	}
	var regressed CaseReport
	for _, item := range rep.Cases {
		if item.CaseID == "regressed" {
			regressed = item
		}
	}
	if regressed.Attempt != 2 || regressed.Reason != "latest failure" {
		t.Fatalf("report did not keep the latest attempt: %+v", regressed)
	}
	if len(rep.Compare.NewFailures) != 1 || rep.Compare.NewFailures[0].CaseID != "regressed" {
		t.Fatalf("new failures = %+v", rep.Compare.NewFailures)
	}
	if rep.Compare.NewFailures[0].BaseVerdict != VerdictPass || rep.Compare.NewFailures[0].NewVerdict != VerdictFail {
		t.Fatalf("new failure delta = %+v", rep.Compare.NewFailures[0])
	}
	if len(rep.Compare.Recoveries) != 1 || rep.Compare.Recoveries[0].CaseID != "recovered" {
		t.Fatalf("recoveries = %+v", rep.Compare.Recoveries)
	}
	if len(rep.Compare.AddedCases) != 1 || rep.Compare.AddedCases[0].CaseID != "added" {
		t.Fatalf("added = %+v", rep.Compare.AddedCases)
	}
	if rep.Compare.SameDataset {
		t.Fatal("edited dataset still reported as the same sha")
	}
	if rep.Compare.BaseDatasetSHA != hash || rep.Compare.NewDatasetSHA != nextHash {
		t.Fatalf("compare sha base=%s new=%s", rep.Compare.BaseDatasetSHA, rep.Compare.NewDatasetSHA)
	}
	html, err := os.ReadFile(filepath.Join(root, "runs", next.RunID, "report.html"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(html)
	for _, needle := range []string{"新增失败", "恢复", "regressed", "recovered", "latest failure", "execution", "verdict", "dataset sha"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("html report missing %q", needle)
		}
	}

	baseReport, err := st.BuildReport(base.RunID, "")
	if err != nil {
		t.Fatal(err)
	}
	if baseReport.DatasetSHA256 != hash {
		t.Fatal("base report no longer matches the frozen hash")
	}
	if baseReport.Compare != nil {
		t.Fatal("base report gained a comparison after the new run")
	}
}

func passingSampleExecutor() *ScriptedExecutor {
	return &ScriptedExecutor{Turns: map[string]ScriptedTurn{
		"sample-memory-correct-resume#0": {Result: TextResult("我在听，接下来按你的要求记笔记。")},
		"sample-memory-correct-resume#1": {Result: WithMemory(TextResult("已记住周二 09:30，三楼会议室。", ToolEvidence{Name: "remember"}),
			MemoryWriteEvidence{Tool: "remember", Content: "晨会周二 09:30 三楼会议室", OK: true})},
		"sample-memory-correct-resume#2": {Result: WithMemory(TextResult("已改成周三 10:00，三楼会议室。", ToolEvidence{Name: "remember"}),
			MemoryWriteEvidence{Tool: "remember", Content: "晨会周三 10:00 三楼会议室", OK: true})},
		"sample-memory-correct-resume#3": {Result: TextResult("依据记忆：周三 10:00 三楼会议室。第一步准备议程。")},
	}}
}
