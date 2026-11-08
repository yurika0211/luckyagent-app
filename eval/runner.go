package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ExecutionStatus describes what happened, independent of quality.
type ExecutionStatus string

const (
	ExecutionCompleted ExecutionStatus = "completed"
	ExecutionFailed    ExecutionStatus = "failed"
	ExecutionUnknown   ExecutionStatus = "unknown"
)

// Verdict is the quality decision. A skipped or unknown verdict never counts
// as passed.
type Verdict string

const (
	VerdictPass    Verdict = "pass"
	VerdictFail    Verdict = "fail"
	VerdictSkip    Verdict = "skip"
	VerdictUnknown Verdict = "unknown"
)

type ToolEvidence struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	Failed    bool   `json:"failed,omitempty"`
}

type MemoryWriteEvidence struct {
	Tool    string `json:"tool"`
	Content string `json:"content,omitempty"`
	Result  string `json:"result,omitempty"`
	OK      bool   `json:"ok"`
}

// Evidence is the observable Agent output consumed by validators. Tool and
// memory evidence come from the Agent loop; session evidence is transcript
// metadata only. Missing evidence yields unknown, never pass.
type Evidence struct {
	Response              string                `json:"response,omitempty"`
	ResponseAvailable     bool                  `json:"response_available"`
	Tools                 []ToolEvidence        `json:"tools,omitempty"`
	TraceAvailable        bool                  `json:"trace_available"`
	MemoryWrites          []MemoryWriteEvidence `json:"memory_writes,omitempty"`
	MemoryDump            string                `json:"memory_dump,omitempty"`
	MemoryDumpAvailable   bool                  `json:"memory_dump_available"`
	SessionMessages       int                   `json:"session_messages,omitempty"`
	SessionTranscriptPath string                `json:"session_transcript_path,omitempty"`
}

type CheckResult struct {
	Validator string `json:"validator"`
	Status    string `json:"status"` // pass, fail, unknown, skip
	Reason    string `json:"reason,omitempty"`
}

type TurnAttempt struct {
	Index      int             `json:"index"` // 1-based
	Input      string          `json:"input"`
	Addendum   string          `json:"addendum,omitempty"`
	Execution  ExecutionStatus `json:"execution"`
	Verdict    Verdict         `json:"verdict"`
	Reason     string          `json:"reason,omitempty"`
	Checks     []CheckResult   `json:"checks,omitempty"`
	Evidence   Evidence        `json:"evidence"`
	StartedAt  time.Time       `json:"started_at"`
	DurationMS float64         `json:"duration_ms"`
}

// Addendum is the evidence left by replay_resume when a turn fails and the next
// turn continues the same session.
type Addendum struct {
	AfterTurn int    `json:"after_turn"`
	Note      string `json:"note"`
	Injected  string `json:"injected"`
}

type Attempt struct {
	RunID          string          `json:"run_id"`
	CaseID         string          `json:"case_id"`
	Attempt        int             `json:"attempt"`
	DatasetSHA256  string          `json:"dataset_sha256"`
	Mode           string          `json:"mode"`
	Profile        string          `json:"profile"`
	Execution      ExecutionStatus `json:"execution"`
	Verdict        Verdict         `json:"verdict"`
	Reason         string          `json:"reason,omitempty"`
	EffectiveTurns []int           `json:"effective_turns,omitempty"`
	AbortedAtTurn  int             `json:"aborted_at_turn,omitempty"`
	Addendum       []Addendum      `json:"addendum,omitempty"`
	Turns          []TurnAttempt   `json:"turns"`
	Error          string          `json:"error,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	DurationMS     float64         `json:"duration_ms"`
}

func (a Attempt) MarshalJSONLine() ([]byte, error) { return json.Marshal(a) }

// TurnRequest is one real-loop turn request. Addendum is non-empty only for a
// replay_resume continuation after a failed turn.
type TurnRequest struct {
	RunID     string
	Manifest  Manifest
	Case      Case
	TurnIndex int // 0-based
	Input     string
	Addendum  string
	Workspace string
	Profile   string
}

type TurnResult struct {
	Response              string
	ResponseAvailable     bool
	Tools                 []ToolEvidence
	TraceAvailable        bool
	MemoryWrites          []MemoryWriteEvidence
	MemoryDump            string
	MemoryDumpAvailable   bool
	SessionMessages       int
	SessionTranscriptPath string
}

type Executor interface {
	Execute(ctx context.Context, req TurnRequest) (TurnResult, error)
}

// CaseCloser lets an executor release per-case resources between cases.
type CaseCloser interface {
	CloseCase(caseID string) error
}

// Runner executes frozen cases against an Executor.
type Runner struct {
	Executor Executor
	Now      func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

// RunCase executes one frozen case and returns the attempt to append to the
// run's JSONL. It never returns an error: failures are recorded in the attempt
// so that JSONL keeps every attempt.
func (r *Runner) RunCase(ctx context.Context, m Manifest, c Case, attempt int) (out Attempt) {
	started := r.now()
	out = Attempt{
		RunID:         m.RunID,
		CaseID:        c.ID,
		Attempt:       attempt,
		DatasetSHA256: m.DatasetSHA256,
		Mode:          c.EffectiveMode(&m.Dataset),
		Profile:       c.EffectiveProfile(&m.Dataset),
		Execution:     ExecutionCompleted,
		Verdict:       VerdictUnknown,
		StartedAt:     started,
	}
	defer func() {
		if rec := recover(); rec != nil {
			out.Execution = ExecutionFailed
			out.Verdict = VerdictUnknown
			out.Error = fmt.Sprintf("panic: %v", rec)
			out.Reason = out.Error
		}
		out.DurationMS = float64(r.now().Sub(started)) / float64(time.Millisecond)
	}()

	if c.Skip {
		out.Verdict = VerdictSkip
		out.Reason = strings.TrimSpace(c.SkipReason)
		if out.Reason == "" {
			out.Reason = "case marked skip"
		}
		out.EffectiveTurns = []int{}
		return out
	}
	if r.Executor == nil {
		out.Execution = ExecutionUnknown
		out.Verdict = VerdictUnknown
		out.Reason = "no executor configured"
		return out
	}

	indexes := c.TurnIndexes(&m.Dataset)
	out.EffectiveTurns = make([]int, 0, len(indexes))
	mode := out.Mode
	workspace := c.EffectiveWorkspace(&m.Dataset)
	profile := out.Profile
	aborted := false

	for pos, idx := range indexes {
		if aborted {
			break
		}
		turn := c.Turns[idx]

		// Replay modes wait between executed turns; inject mode uses the last
		// turn only, so its wait is ignored.
		if pos > 0 && mode != ModeInjectAbort {
			if wait := strings.TrimSpace(c.Turns[indexes[pos-1]].Wait); wait != "" {
				if d, err := time.ParseDuration(wait); err == nil && d > 0 {
					select {
					case <-time.After(d):
					case <-ctx.Done():
						out.Execution = ExecutionUnknown
						out.Verdict = VerdictUnknown
						out.Reason = "canceled while waiting between turns"
						return out
					}
				}
			}
		}

		input := turn.Input
		addendum := ""
		if mode == ModeInjectAbort {
			input = injectContext(c, idx) + "\n\n" + turn.Input
		}
		if mode == ModeReplayResume && len(out.Addendum) > 0 && out.Addendum[len(out.Addendum)-1].AfterTurn == indexes[pos-1] {
			addendum = out.Addendum[len(out.Addendum)-1].Injected
			input = addendum + "\n\n" + turn.Input
		}

		turnStart := r.now()
		res, err := r.Executor.Execute(ctx, TurnRequest{
			RunID:     m.RunID,
			Manifest:  m,
			Case:      c,
			TurnIndex: idx,
			Input:     input,
			Addendum:  addendum,
			Workspace: workspace,
			Profile:   profile,
		})
		turnAttempt := r.evaluateTurn(turn, res, err, turnStart)
		turnAttempt.Index = idx + 1
		turnAttempt.Input = turn.Input
		turnAttempt.Addendum = addendum
		out.Turns = append(out.Turns, turnAttempt)

		if turnAttempt.Execution == ExecutionFailed {
			out.Execution = ExecutionFailed
		} else if turnAttempt.Execution == ExecutionUnknown {
			out.Execution = ExecutionUnknown
		}

		failed := turnAttempt.Execution == ExecutionFailed || turnAttempt.Verdict == VerdictFail
		if failed {
			if mode == ModeReplayResume && pos < len(indexes)-1 {
				note := fmt.Sprintf("第 %d 轮未通过：%s", idx+1, strings.TrimSpace(turnAttempt.Reason))
				inj := fmt.Sprintf("[评测补写] %s\n请先更正上一轮的问题，再继续完成本轮要求。", note)
				out.Addendum = append(out.Addendum, Addendum{
					AfterTurn: idx,
					Note:      note,
					Injected:  inj,
				})
			} else {
				aborted = true
				out.AbortedAtTurn = idx + 1
			}
		}

		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.Canceled) || errors.Is(ctxErr, context.DeadlineExceeded) {
				out.Execution = ExecutionUnknown
			}
		}
	}

	if mode == ModeReplayResume && len(out.Addendum) > 0 && !addendumDelivered(out) {
		out.Verdict = VerdictUnknown
		out.Reason = "replay_resume produced an addendum that was not delivered to a later turn"
	} else {
		out.Verdict = aggregateVerdict(out.Turns)
		out.Reason = aggregateReason(out)
	}
	if out.Execution == ExecutionCompleted && len(out.Turns) == 0 {
		out.Execution = ExecutionUnknown
		out.Verdict = VerdictUnknown
		out.Reason = "no effective turns executed"
	}
	return out
}

// AddendumDelivered is true when every replay_resume correction was actually
// prefixed onto a later executed turn. An addendum that only sits on the
// attempt is not evidence.
func (a Attempt) AddendumDelivered() bool {
	return addendumDelivered(a)
}

func addendumDelivered(a Attempt) bool {
	if len(a.Addendum) == 0 {
		return true
	}
	delivered := map[string]bool{}
	for _, turn := range a.Turns {
		if strings.TrimSpace(turn.Addendum) != "" {
			delivered[turn.Addendum] = true
		}
	}
	for _, item := range a.Addendum {
		if strings.TrimSpace(item.Injected) == "" || !delivered[item.Injected] {
			return false
		}
	}
	return true
}

func (r *Runner) evaluateTurn(turn Turn, res TurnResult, err error, started time.Time) TurnAttempt {
	out := TurnAttempt{
		Execution:  ExecutionCompleted,
		Verdict:    VerdictUnknown,
		StartedAt:  started,
		DurationMS: float64(r.now().Sub(started)) / float64(time.Millisecond),
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			out.Execution = ExecutionUnknown
			out.Reason = "turn outcome unknown: " + err.Error()
		} else {
			out.Execution = ExecutionFailed
			out.Reason = "turn failed: " + err.Error()
		}
		out.Verdict = VerdictUnknown
		return out
	}
	if !res.ResponseAvailable && !res.TraceAvailable && !res.MemoryDumpAvailable && res.MemoryWrites == nil {
		// Defensive: an executor that returns nothing gives no evidence.
		out.Execution = ExecutionUnknown
		out.Verdict = VerdictUnknown
		out.Reason = "executor returned no observable evidence"
		return out
	}
	out.Evidence = Evidence{
		Response:              res.Response,
		ResponseAvailable:     res.ResponseAvailable,
		Tools:                 res.Tools,
		TraceAvailable:        res.TraceAvailable,
		MemoryWrites:          res.MemoryWrites,
		MemoryDump:            truncate(res.MemoryDump, 200_000),
		MemoryDumpAvailable:   res.MemoryDumpAvailable,
		SessionMessages:       res.SessionMessages,
		SessionTranscriptPath: res.SessionTranscriptPath,
	}
	out.Checks, out.Verdict, out.Reason = EvaluateChecks(turn.Evaluation, out.Evidence)
	return out
}

func aggregateVerdict(turns []TurnAttempt) Verdict {
	if len(turns) == 0 {
		return VerdictUnknown
	}
	seenSkip := false
	for _, t := range turns {
		switch t.Verdict {
		case VerdictFail:
			return VerdictFail
		case VerdictUnknown:
			return VerdictUnknown
		case VerdictSkip:
			seenSkip = true
		}
	}
	if seenSkip {
		return VerdictSkip
	}
	return VerdictPass
}

func aggregateReason(a Attempt) string {
	if a.Error != "" {
		return a.Error
	}
	if a.Execution == ExecutionFailed {
		return "execution failed"
	}
	if a.Execution == ExecutionUnknown {
		return "execution outcome unknown"
	}
	for _, t := range a.Turns {
		if t.Reason != "" && (t.Verdict != VerdictPass || t.Execution != ExecutionCompleted) {
			return fmt.Sprintf("turn %d: %s", t.Index, t.Reason)
		}
	}
	if a.Verdict == VerdictPass {
		return "all effective turns passed"
	}
	if a.Verdict == VerdictSkip {
		return "no configured validators"
	}
	return ""
}

func injectContext(c Case, last int) string {
	var b strings.Builder
	b.WriteString("[评测注入] 以下是同一会话此前的轮次，请视为已经发生的上下文，不要重复执行历史工具调用。")
	for i := 0; i < last; i++ {
		t := c.Turns[i]
		fmt.Fprintf(&b, "\n\n[历史轮次 %d] 用户：%s", i+1, t.Input)
		if strings.TrimSpace(t.Expected) != "" {
			fmt.Fprintf(&b, "\n[历史轮次 %d] 期望：%s", i+1, t.Expected)
		}
	}
	return b.String()
}

func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "\n... (truncated)"
}
