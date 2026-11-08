// Package eval implements the real-Agent-loop evaluation platform: frozen
// case manifests, execution/verdict separation, evidence validators,
// versioned attempts and run comparison reports.
//
// Component benchmarks (cmd/la-*-bench) stay independent. This package only
// covers cases executed through the real Agent loop.
package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

const SchemaVersion = "agent-eval/v1"

// Multi-turn execution modes. inject_resume is intentionally forbidden: it
// would mix produced state with injected expectations and cannot keep a
// trustworthy evidence chain.
const (
	ModeReplayAbort  = "replay_abort"
	ModeReplayResume = "replay_resume"
	ModeInjectAbort  = "inject_abort"
	modeInjectResume = "inject_resume"
)

// Dataset is the versioned case list. The file bytes are not the identity;
// DatasetSHA256 hashes the parsed canonical value so formatting never changes
// a run, but editing any case does.
type Dataset struct {
	SchemaVersion string    `json:"schema_version"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	Defaults      *Defaults `json:"defaults,omitempty"`
	Cases         []Case    `json:"cases"`
}

type Defaults struct {
	Mode      string `json:"mode,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

type Case struct {
	ID         string   `json:"id"`
	Title      string   `json:"title,omitempty"`
	Coverage   []string `json:"coverage,omitempty"` // chat, tool_route, memory_write, long_task
	Mode       string   `json:"mode,omitempty"`
	Profile    string   `json:"profile,omitempty"`
	Workspace  string   `json:"workspace,omitempty"`
	Skip       bool     `json:"skip,omitempty"`
	SkipReason string   `json:"skip_reason,omitempty"`
	Turns      []Turn   `json:"turns"`
}

// Turn is the fixed multi-turn contract: input / expected / evaluation / wait.
type Turn struct {
	Input      string      `json:"input"`
	Expected   string      `json:"expected,omitempty"`
	Evaluation *Evaluation `json:"evaluation,omitempty"`
	Wait       string      `json:"wait,omitempty"`
}

type Evaluation struct {
	Keyword     *KeywordCheck     `json:"keyword,omitempty"`
	Regex       *RegexCheck       `json:"regex,omitempty"`
	Trace       *TraceCheck       `json:"trace,omitempty"`
	MemoryWrite *MemoryWriteCheck `json:"memory_write,omitempty"`
}

type KeywordCheck struct {
	All []string `json:"all,omitempty"`
	Any []string `json:"any,omitempty"`
}

type RegexCheck struct {
	Patterns []string `json:"patterns"`
}

type TraceCheck struct {
	Required  []string `json:"required,omitempty"`
	Forbidden []string `json:"forbidden,omitempty"`
}

// MemoryWriteCheck verifies that a write happened and that the written value is
// the corrected one. Forbidden values must not survive in the write evidence or
// the isolated memory vault snapshot.
type MemoryWriteCheck struct {
	MustWrite bool     `json:"must_write"`
	Expected  []string `json:"expected,omitempty"`
	Forbidden []string `json:"forbidden,omitempty"`
}

// legacyFields are columns produced by the existing la-*-bench JSONL files that
// have no place in agent-eval/v1. They are rejected explicitly so an old file
// cannot be silently interpreted as a real-loop case list.
var legacyFields = map[string]string{
	"type":                 "record/summary stream tag; a real-loop dataset has no mixed record types",
	"variant":              "strategy variant label; profiles belong to the run manifest",
	"scenario":             "component benchmark scenario name",
	"task_id":              "replaced by case.id",
	"task_type":            "replaced by case.coverage",
	"round":                "benchmark repetition index; runs repeat cases instead",
	"prompt":               "replaced by turns[].input",
	"need_tool_gold":       "component-only gold label",
	"need_tool_prob":       "component strategy probability",
	"required_tools":       "moved under turns[].evaluation.trace.required",
	"required_operations":  "component operation-level routing is out of scope",
	"forbidden_tools":      "moved under turns[].evaluation.trace.forbidden",
	"forbidden_operations": "component operation-level routing is out of scope",
	"visible_tools":        "tool surface belongs to the profile manifest",
	"called_tools":         "evidence, not a case field",
	"called_operations":    "component operation-level routing is out of scope",
	"tool_calls":           "evidence, not a case field",
	"success":              "replaced by attempt.execution plus attempt.verdict",
	"clean":                "component threshold aggregate",
	"query_id":             "component retrieval sample id",
	"query":                "replaced by turns[].input",
	"recall_at_k":          "component retrieval metric",
	"precision_at_k":       "component retrieval metric",
	"mrr":                  "component retrieval metric",
	"ndcg_at_k":            "component retrieval metric",
	"risk_flags":           "component risk flags",
	"quality_pass":         "replaced by validators",
	"size":                 "component dataset size",
	"seed":                 "component synthetic seed",
	"started_at":           "execution metadata, not a case field",
	"duration_ms":          "execution metadata, not a case field",
	"duration_ns":          "execution metadata, not a case field",
}

// LoadDataset decodes strictly, rejects legacy columns and validates the case
// contract. The returned hash identifies the frozen case list.
func LoadDataset(path string) (*Dataset, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read dataset: %w", err)
	}
	ds, err := DecodeDataset(raw)
	if err != nil {
		return nil, "", err
	}
	hash, err := DatasetHash(ds)
	if err != nil {
		return nil, "", err
	}
	return ds, hash, nil
}

func DecodeDataset(raw []byte) (*Dataset, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var ds Dataset
	if err := dec.Decode(&ds); err != nil {
		return nil, legacyDecodeError(err)
	}
	if dec.More() {
		return nil, fmt.Errorf("dataset has trailing JSON values")
	}
	if err := ds.Validate(); err != nil {
		return nil, err
	}
	return &ds, nil
}

func legacyDecodeError(err error) error {
	msg := err.Error()
	const marker = "unknown field "
	if idx := strings.Index(msg, marker); idx >= 0 {
		rest := msg[idx+len(marker):]
		rest = strings.Trim(rest, `"`)
		if i := strings.IndexAny(rest, `" `); i >= 0 {
			rest = rest[:i]
		}
		if hint, ok := legacyFields[rest]; ok {
			return fmt.Errorf("legacy bench field %q is rejected by %s: %s", rest, SchemaVersion, hint)
		}
		return fmt.Errorf("unknown field %q is rejected by %s; the turn contract is input/expected/evaluation/wait", rest, SchemaVersion)
	}
	return fmt.Errorf("decode dataset: %w", err)
}

func DatasetHash(ds *Dataset) (string, error) {
	data, err := json.Marshal(ds)
	if err != nil {
		return "", fmt.Errorf("hash dataset: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (ds *Dataset) Validate() error {
	if ds == nil {
		return fmt.Errorf("dataset is nil")
	}
	if ds.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %q, got %q", SchemaVersion, ds.SchemaVersion)
	}
	if strings.TrimSpace(ds.Name) == "" {
		return fmt.Errorf("dataset name is required")
	}
	if len(ds.Cases) == 0 {
		return fmt.Errorf("dataset has no cases")
	}
	seen := map[string]bool{}
	for i := range ds.Cases {
		c := &ds.Cases[i]
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("cases[%d]: id is required", i)
		}
		if seen[c.ID] {
			return fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
		mode := c.EffectiveMode(ds)
		if err := validateMode(c.ID, mode); err != nil {
			return err
		}
		if len(c.Turns) == 0 {
			return fmt.Errorf("case %q: turns is required", c.ID)
		}
		for ti := range c.Turns {
			t := &c.Turns[ti]
			where := fmt.Sprintf("case %q turn %d", c.ID, ti+1)
			if strings.TrimSpace(t.Input) == "" {
				return fmt.Errorf("%s: input is required", where)
			}
			if strings.TrimSpace(t.Wait) != "" {
				if _, err := time.ParseDuration(t.Wait); err != nil {
					return fmt.Errorf("%s: wait must be a Go duration: %w", where, err)
				}
			}
			if err := t.Evaluation.Validate(); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		}
		if mode == ModeInjectAbort && len(c.Turns) < 2 {
			return fmt.Errorf("case %q: inject_abort needs at least two turns", c.ID)
		}
	}
	return nil
}

func (e *Evaluation) Validate() error {
	if e == nil {
		return nil
	}
	if e.Keyword != nil && len(e.Keyword.All) == 0 && len(e.Keyword.Any) == 0 {
		return fmt.Errorf("keyword check needs all or any terms")
	}
	if e.Regex != nil {
		if len(e.Regex.Patterns) == 0 {
			return fmt.Errorf("regex check needs patterns")
		}
		for _, p := range e.Regex.Patterns {
			if _, err := regexp.Compile(p); err != nil {
				return fmt.Errorf("regex %q: %w", p, err)
			}
		}
	}
	if e.Trace != nil && len(e.Trace.Required) == 0 && len(e.Trace.Forbidden) == 0 {
		return fmt.Errorf("trace check needs required or forbidden tools")
	}
	if e.MemoryWrite != nil && !e.MemoryWrite.MustWrite && len(e.MemoryWrite.Expected) == 0 && len(e.MemoryWrite.Forbidden) == 0 {
		return fmt.Errorf("memory_write check needs must_write, expected or forbidden")
	}
	return nil
}

func (e *Evaluation) Configured() bool {
	if e == nil {
		return false
	}
	return e.Keyword != nil || e.Regex != nil || e.Trace != nil || e.MemoryWrite != nil
}

func (c *Case) EffectiveMode(ds *Dataset) string {
	if v := strings.TrimSpace(c.Mode); v != "" {
		return v
	}
	if ds != nil && ds.Defaults != nil {
		if v := strings.TrimSpace(ds.Defaults.Mode); v != "" {
			return v
		}
	}
	return ModeReplayAbort
}

func (c *Case) EffectiveProfile(ds *Dataset) string {
	if v := strings.TrimSpace(c.Profile); v != "" {
		return v
	}
	if ds != nil && ds.Defaults != nil && strings.TrimSpace(ds.Defaults.Profile) != "" {
		return ds.Defaults.Profile
	}
	return "chat"
}

func (c *Case) EffectiveWorkspace(ds *Dataset) string {
	if v := strings.TrimSpace(c.Workspace); v != "" {
		return v
	}
	if ds != nil && ds.Defaults != nil && strings.TrimSpace(ds.Defaults.Workspace) != "" {
		return ds.Defaults.Workspace
	}
	return "."
}

func validateMode(caseID, mode string) error {
	switch mode {
	case ModeReplayAbort, ModeReplayResume, ModeInjectAbort:
		return nil
	case modeInjectResume:
		return fmt.Errorf("case %q: mode inject_resume is forbidden; use replay_resume (requires addendum evidence) or inject_abort", caseID)
	default:
		return fmt.Errorf("case %q: unknown mode %q (allowed: %s, %s, %s)", caseID, mode, ModeReplayAbort, ModeReplayResume, ModeInjectAbort)
	}
}

// TurnIndexes returns the turns actually executed for a mode. inject_abort
// injects prior turns as context and evaluates only the last turn. The dataset
// is required so a case that inherits inject_abort from defaults is not
// replayed in full.
func (c *Case) TurnIndexes(ds *Dataset) []int {
	if c == nil || len(c.Turns) == 0 {
		return nil
	}
	if c.EffectiveMode(ds) == ModeInjectAbort {
		return []int{len(c.Turns) - 1}
	}
	out := make([]int, len(c.Turns))
	for i := range c.Turns {
		out[i] = i
	}
	return out
}