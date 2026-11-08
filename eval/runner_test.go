package eval

import (
	"context"
	"strings"
	"testing"
	"time"
)

func sampleManifest(ds *Dataset, hash string) Manifest {
	return Manifest{
		RunID:         "run-test",
		SchemaVersion: SchemaVersion,
		DatasetName:   ds.Name,
		DatasetSHA256: hash,
		Dataset:       *ds,
		Profile:       "chat",
		Workspace:     ".",
		FrozenAt:      time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
	}
}

func TestReplayResumeLeavesDeliveredAddendum(t *testing.T) {
	ds, hash, err := LoadDataset("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	exec := &ScriptedExecutor{Turns: map[string]ScriptedTurn{
		"sample-memory-correct-resume#0": {Result: TextResult("我在听，接下来按你的要求记笔记。")},
		"sample-memory-correct-resume#1": {Result: WithMemory(TextResult("已记住周二 09:30，三楼会议室。",
			ToolEvidence{Name: "remember", Arguments: `{"content":"晨会周二 09:30 三楼会议室"}`}),
			MemoryWriteEvidence{Tool: "remember", Content: "晨会周二 09:30 三楼会议室", OK: true})},
		"sample-memory-correct-resume#2": {Result: WithMemory(TextResult("已改成周三 10:00，三楼会议室。",
			ToolEvidence{Name: "remember", Arguments: `{"content":"晨会周三 10:00 三楼会议室"}`}),
			MemoryWriteEvidence{Tool: "remember", Content: "晨会周三 10:00 三楼会议室", OK: true})},
		"sample-memory-correct-resume#3": {Result: TextResult("依据记忆：周三 10:00 三楼会议室。第一步准备议程。")},
	}}

	// Turn 2's keyword requires 09:30 in the response. The scripted response has it,
	// so this path is a clean pass and must not invent an addendum.
	runner := &Runner{Executor: exec, Now: func() time.Time { return time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC) }}
	attempt := runner.RunCase(context.Background(), sampleManifest(ds, hash), ds.Cases[0], 1)
	if attempt.Execution != ExecutionCompleted || attempt.Verdict != VerdictPass {
		t.Fatalf("clean sample execution=%s verdict=%s reason=%s", attempt.Execution, attempt.Verdict, attempt.Reason)
	}
	if len(attempt.Addendum) != 0 {
		t.Fatalf("clean pass left addendum: %+v", attempt.Addendum)
	}
	if len(attempt.Turns) != 4 {
		t.Fatalf("executed turns = %d", len(attempt.Turns))
	}

	// Force turn 2 to fail quality, then resume. The next request must carry the addendum.
	exec = &ScriptedExecutor{Turns: map[string]ScriptedTurn{
		"sample-memory-correct-resume#0": {Result: TextResult("我在听，接下来按你的要求记笔记。")},
		"sample-memory-correct-resume#1": {Result: WithMemory(TextResult("已调用工具，但回复里没有具体时间。",
			ToolEvidence{Name: "remember"}),
			MemoryWriteEvidence{Tool: "remember", Content: "晨会周二 09:30 三楼会议室", OK: true})},
		"sample-memory-correct-resume#2": {Result: WithMemory(TextResult("已改成周三 10:00，三楼会议室。",
			ToolEvidence{Name: "remember"}),
			MemoryWriteEvidence{Tool: "remember", Content: "晨会周三 10:00 三楼会议室", OK: true})},
		"sample-memory-correct-resume#3": {Result: TextResult("依据记忆：周三 10:00 三楼会议室。第一步准备议程。")},
	}}
	runner.Executor = exec
	attempt = runner.RunCase(context.Background(), sampleManifest(ds, hash), ds.Cases[0], 2)
	if len(attempt.Addendum) == 0 {
		t.Fatal("replay_resume after a failed turn left no addendum")
	}
	if attempt.Turns[2].Addendum == "" || !strings.Contains(exec.Requests[2].Input, "[评测补写]") {
		t.Fatalf("addendum was not delivered to the next turn: input=%q addendum=%q", exec.Requests[2].Input, attempt.Turns[2].Addendum)
	}
	if attempt.Verdict == VerdictPass {
		t.Fatal("a group with a failed turn must not pass")
	}
	if attempt.Verdict != VerdictFail {
		t.Fatalf("group verdict = %s, want fail (%s)", attempt.Verdict, attempt.Reason)
	}
}

func TestReplayResumeLastTurnFailureNeedsNoAddendum(t *testing.T) {
	ds := &Dataset{
		SchemaVersion: SchemaVersion,
		Name:          "last",
		Cases: []Case{{
			ID:   "last-fails",
			Mode: ModeReplayResume,
			Turns: []Turn{
				{Input: "先通过", Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"通过"}}}},
				{Input: "最后一轮", Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"通过"}}}},
			},
		}},
	}
	exec := &ScriptedExecutor{Turns: map[string]ScriptedTurn{
		"last-fails#0": {Result: TextResult("通过")},
		"last-fails#1": {Result: TextResult("没有关键字")},
	}}
	attempt := (&Runner{Executor: exec}).RunCase(context.Background(), sampleManifest(ds, "hash"), ds.Cases[0], 1)
	if len(attempt.Addendum) != 0 {
		t.Fatalf("last-turn failure must not invent an undeliverable addendum: %+v", attempt.Addendum)
	}
	if attempt.Verdict != VerdictFail || attempt.AbortedAtTurn != 2 {
		t.Fatalf("execution=%s verdict=%s aborted=%d reason=%s", attempt.Execution, attempt.Verdict, attempt.AbortedAtTurn, attempt.Reason)
	}
}

func TestInjectAbortEvaluatesOnlyLastTurn(t *testing.T) {
	ds := &Dataset{
		SchemaVersion: SchemaVersion,
		Name:          "inject",
		Cases: []Case{{
			ID:   "late-only",
			Mode: ModeInjectAbort,
			Turns: []Turn{
				{Input: "先记住错误时间周二 09:30", Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"不会执行"}}}},
				{Input: "更正后的安排是周三 10:00", Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"周三"}}}},
			},
		}},
	}
	exec := &ScriptedExecutor{Turns: map[string]ScriptedTurn{
		"late-only#1": {Result: TextResult("更正后是周三 10:00")},
	}}
	runner := &Runner{Executor: exec}
	attempt := runner.RunCase(context.Background(), sampleManifest(ds, "hash"), ds.Cases[0], 1)
	if len(exec.Requests) != 1 || exec.Requests[0].TurnIndex != 1 {
		t.Fatalf("inject_abort executed %#v", exec.Requests)
	}
	if !strings.Contains(exec.Requests[0].Input, "[评测注入]") || !strings.Contains(exec.Requests[0].Input, "周二 09:30") {
		t.Fatalf("prior turn was not injected: %s", exec.Requests[0].Input)
	}
	if attempt.Verdict != VerdictPass || attempt.Execution != ExecutionCompleted {
		t.Fatalf("execution=%s verdict=%s reason=%s", attempt.Execution, attempt.Verdict, attempt.Reason)
	}
	if len(attempt.Turns) != 1 || attempt.Turns[0].Index != 2 {
		t.Fatalf("effective turns = %+v", attempt.Turns)
	}
}

func TestExecutionAndVerdictStaySeparate(t *testing.T) {
	ds := &Dataset{
		SchemaVersion: SchemaVersion,
		Name:          "split",
		Cases: []Case{{
			ID: "ran-but-wrong",
			Turns: []Turn{{
				Input:      "说在听",
				Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"在听"}}},
			}},
		}},
	}
	exec := &ScriptedExecutor{Turns: map[string]ScriptedTurn{
		"ran-but-wrong#0": {Result: TextResult("好的")},
	}}
	attempt := (&Runner{Executor: exec}).RunCase(context.Background(), sampleManifest(ds, "hash"), ds.Cases[0], 1)
	if attempt.Execution != ExecutionCompleted || attempt.Verdict != VerdictFail {
		t.Fatalf("execution=%s verdict=%s", attempt.Execution, attempt.Verdict)
	}

	exec = &ScriptedExecutor{Turns: map[string]ScriptedTurn{
		"ran-but-wrong#0": {Err: context.DeadlineExceeded},
	}}
	attempt = (&Runner{Executor: exec}).RunCase(context.Background(), sampleManifest(ds, "hash"), ds.Cases[0], 1)
	if attempt.Execution != ExecutionUnknown || attempt.Verdict != VerdictUnknown {
		t.Fatalf("timeout execution=%s verdict=%s", attempt.Execution, attempt.Verdict)
	}
	if attempt.Verdict == VerdictPass {
		t.Fatal("unknown counted as pass")
	}
}

func TestOneFailedTurnFailsTheGroup(t *testing.T) {
	ds := &Dataset{
		SchemaVersion: SchemaVersion,
		Name:          "group",
		Cases: []Case{{
			ID:   "all-must-pass",
			Mode: ModeReplayAbort,
			Turns: []Turn{
				{Input: "第一轮", Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"通过"}}}},
				{Input: "第二轮", Evaluation: &Evaluation{Keyword: &KeywordCheck{All: []string{"通过"}}}},
			},
		}},
	}
	exec := &ScriptedExecutor{Turns: map[string]ScriptedTurn{
		"all-must-pass#0": {Result: TextResult("通过")},
		"all-must-pass#1": {Result: TextResult("好听但没有关键字")},
	}}
	attempt := (&Runner{Executor: exec}).RunCase(context.Background(), sampleManifest(ds, "hash"), ds.Cases[0], 1)
	if attempt.Verdict != VerdictFail {
		t.Fatalf("group verdict = %s", attempt.Verdict)
	}
	if attempt.AbortedAtTurn != 2 {
		t.Fatalf("aborted at %d", attempt.AbortedAtTurn)
	}
}
