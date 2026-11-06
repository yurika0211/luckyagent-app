package evalplatform

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const runFileVersion = 1

// RunFile is the frozen run. Started stays false until Start.
type RunFile struct {
	Version   int          `json:"version"`
	Name      string       `json:"name"`
	Started   bool         `json:"started"`
	CreatedAt time.Time    `json:"created_at"`
	Cases     []FrozenCase `json:"cases"`
}

// FrozenCase keeps the case text that existed at create-run.
type FrozenCase struct {
	Case      Case         `json:"case"`
	Execution string       `json:"execution,omitempty"`
	Verdict   string       `json:"verdict,omitempty"`
	Turns     []TurnResult `json:"turns,omitempty"`
}

// TurnResult is one step after start. Both execution and verdict are set.
type TurnResult struct {
	Execution string   `json:"execution"`
	Verdict   string   `json:"verdict"`
	Evidence  Evidence `json:"evidence"`
}

// Evidence is the fake or recorded transcript. Nil tool or memory slices mean
// the evidence was not collected.
type Evidence struct {
	Assistant    string        `json:"assistant,omitempty"`
	Tools        []string      `json:"tools,omitempty"`
	ToolsSet     bool          `json:"tools_set"`
	MemoryWrites []MemoryWrite `json:"memory_writes,omitempty"`
	MemorySet    bool          `json:"memory_set"`
}

// MemoryWrite is one recorded remember call.
type MemoryWrite struct {
	Content  string `json:"content"`
	Category string `json:"category"`
}

// Executor supplies a transcript. It must not call a live model.
type Executor interface {
	Execute(c Case) (execution string, turns []Evidence, err error)
}

// CreateRun freezes the dataset into dir/run.json and does not execute.
func CreateRun(datasetPath, dir string) (RunFile, error) {
	dataset, err := LoadDataset(datasetPath)
	if err != nil {
		return RunFile{}, err
	}
	run := RunFile{
		Version:   runFileVersion,
		Name:      dataset.Name,
		Started:   false,
		CreatedAt: time.Now().UTC(),
	}
	for _, item := range dataset.Cases {
		run.Cases = append(run.Cases, FrozenCase{Case: item})
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return RunFile{}, err
	}
	if err := writeRun(filepath.Join(dir, "run.json"), run); err != nil {
		return RunFile{}, err
	}
	return run, nil
}

// Start fills execution and verdict from the executor. It refuses a second start.
func Start(dir string, exec Executor) (RunFile, error) {
	if exec == nil {
		return RunFile{}, fmt.Errorf("executor is required")
	}
	run, err := ReadRun(dir)
	if err != nil {
		return RunFile{}, err
	}
	if run.Started {
		return run, fmt.Errorf("run already started")
	}
	for i := range run.Cases {
		item := &run.Cases[i]
		execution, evidence, err := exec.Execute(item.Case)
		if err != nil {
			return RunFile{}, fmt.Errorf("case %s: %w", item.Case.ID, err)
		}
		if execution == "" {
			execution = ExecutionUnknown
		}
		steps := item.Case.Steps()
		if len(evidence) != len(steps) {
			evidence = padEvidence(evidence, len(steps))
			if execution == ExecutionCompleted {
				execution = ExecutionUnknown
			}
		}
		item.Execution = execution
		item.Turns = make([]TurnResult, len(steps))
		verdicts := make([]string, len(steps))
		for n, step := range steps {
			turnExec := execution
			if n >= len(evidence) {
				turnExec = ExecutionUnknown
			}
			verdict := Judge(step.Eval, evidenceOrEmpty(evidence, n), turnExec)
			item.Turns[n] = TurnResult{Execution: turnExec, Verdict: verdict, Evidence: evidenceOrEmpty(evidence, n)}
			verdicts[n] = verdict
		}
		item.Verdict = combineVerdicts(verdicts)
		if item.Execution == "" {
			item.Execution = execution
		}
	}
	run.Started = true
	if err := writeRun(filepath.Join(dir, "run.json"), run); err != nil {
		return RunFile{}, err
	}
	return run, nil
}

// ReadRun loads the frozen run directory.
func ReadRun(dir string) (RunFile, error) {
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return RunFile{}, err
	}
	var run RunFile
	if err := json.Unmarshal(data, &run); err != nil {
		return RunFile{}, err
	}
	return run, nil
}

func writeRun(path string, run RunFile) error {
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func padEvidence(in []Evidence, n int) []Evidence {
	if len(in) >= n {
		return in[:n]
	}
	out := append([]Evidence{}, in...)
	for len(out) < n {
		out = append(out, Evidence{})
	}
	return out
}

func evidenceOrEmpty(in []Evidence, n int) Evidence {
	if n < 0 || n >= len(in) {
		return Evidence{}
	}
	return in[n]
}

func combineVerdicts(verdicts []string) string {
	if len(verdicts) == 0 {
		return VerdictUnknown
	}
	rank := map[string]int{VerdictFail: 4, VerdictUnknown: 3, VerdictSkip: 2, VerdictPass: 1}
	best := VerdictPass
	for _, verdict := range verdicts {
		if rank[verdict] > rank[best] {
			best = verdict
		}
	}
	return best
}
