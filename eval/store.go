package eval

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	RunStatusCreated   = "created"
	RunStatusRunning   = "running"
	RunStatusCompleted = "completed"
	RunStatusFailed    = "failed"
)

// Manifest is the frozen run input. StartRun always reads this copy, never the
// live dataset file, so editing cases cannot change an existing run.
type Manifest struct {
	RunID          string    `json:"run_id"`
	SchemaVersion  string    `json:"schema_version"`
	DatasetName    string    `json:"dataset_name"`
	DatasetSHA256  string    `json:"dataset_sha256"`
	Dataset        Dataset   `json:"dataset"`
	Profile        string    `json:"profile"`
	Workspace      string    `json:"workspace"`
	CreatedBy      string    `json:"created_by,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	FrozenAt       time.Time `json:"frozen_at"`
	StartRequested bool      `json:"start_requested"`
}

type RunState struct {
	RunID      string    `json:"run_id"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Cases      int       `json:"cases"`
	Attempts   int       `json:"attempts"`
}

type CreateOptions struct {
	Profile        string
	Workspace      string
	CreatedBy      string
	StartRequested bool
}

// Store owns run directories under one root. Root defaults to
// ~/.luckyagent-eval, separate from the tested ~/.luckyagent surface.
type Store struct {
	Root string
	Now  func() time.Time
}

func DefaultRoot() (string, error) {
	if v := strings.TrimSpace(os.Getenv("LUCKYAGENT_EVAL_HOME")); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve eval home: %w", err)
	}
	return filepath.Join(home, ".luckyagent-eval"), nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) runDir(runID string) string {
	return filepath.Join(s.Root, "runs", runID)
}

// CreateRun freezes the dataset into a new run directory. It does not execute
// anything; start is a separate explicit step.
func (s *Store) CreateRun(ds *Dataset, hash string, opts CreateOptions) (*Manifest, error) {
	if ds == nil {
		return nil, fmt.Errorf("dataset is required")
	}
	if strings.TrimSpace(s.Root) == "" {
		return nil, fmt.Errorf("store root is required")
	}
	if err := ds.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(hash) == "" {
		var err error
		hash, err = DatasetHash(ds)
		if err != nil {
			return nil, err
		}
	}
	now := s.now()
	id, err := newRunID(now)
	if err != nil {
		return nil, err
	}
	profile := strings.TrimSpace(opts.Profile)
	if profile == "" {
		profile = "chat"
	}
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" {
		workspace = "."
	}
	m := &Manifest{
		RunID:          id,
		SchemaVersion:  SchemaVersion,
		DatasetName:    ds.Name,
		DatasetSHA256:  hash,
		Dataset:        *ds,
		Profile:        profile,
		Workspace:      workspace,
		CreatedBy:      strings.TrimSpace(opts.CreatedBy),
		CreatedAt:      now,
		FrozenAt:       now,
		StartRequested: opts.StartRequested,
	}
	dir := s.runDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	if err := writeJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
		return nil, err
	}
	state := RunState{RunID: id, Status: RunStatusCreated, CreatedAt: now, Cases: len(ds.Cases)}
	if err := writeJSON(filepath.Join(dir, "state.json"), &state); err != nil {
		return nil, err
	}
	return m, nil
}

func newRunID(now time.Time) (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return fmt.Sprintf("run-%s-%s", now.Format("20060102-150405"), hex.EncodeToString(buf)), nil
}

func (s *Store) LoadManifest(runID string) (*Manifest, error) {
	var m Manifest
	if err := readJSON(filepath.Join(s.runDir(runID), "manifest.json"), &m); err != nil {
		return nil, err
	}
	if m.RunID != runID {
		return nil, fmt.Errorf("manifest run id %q does not match %q", m.RunID, runID)
	}
	return &m, nil
}

func (s *Store) LoadState(runID string) (*RunState, error) {
	var state RunState
	if err := readJSON(filepath.Join(s.runDir(runID), "state.json"), &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// StartRun executes the frozen manifest. It refuses to restart a run so a
// versioned report cannot be silently replaced.
func (s *Store) StartRun(ctx context.Context, runID string, exec Executor) (*RunState, error) {
	manifest, err := s.LoadManifest(runID)
	if err != nil {
		return nil, err
	}
	state, err := s.LoadState(runID)
	if err != nil {
		return nil, err
	}
	if state.Status != RunStatusCreated {
		return nil, fmt.Errorf("run %s already %s; create a new run instead", runID, state.Status)
	}
	state.Status = RunStatusRunning
	state.StartedAt = s.now()
	if err := writeJSON(s.runDir(runID)+"/state.json", state); err != nil {
		return nil, err
	}

	runner := &Runner{Executor: exec, Now: s.Now}
	attemptsFile := filepath.Join(s.runDir(runID), "attempts.jsonl")
	for i := range manifest.Dataset.Cases {
		c := manifest.Dataset.Cases[i]
		attemptNo, err := attemptCount(attemptsFile, c.ID)
		if err != nil {
			return nil, err
		}
		attempt := runner.RunCase(ctx, *manifest, c, attemptNo+1)
		if err := appendAttempt(attemptsFile, attempt); err != nil {
			return nil, err
		}
		state.Attempts++
		if closer, ok := exec.(CaseCloser); ok {
			_ = closer.CloseCase(c.ID)
		}
	}
	state.Status = RunStatusCompleted
	state.FinishedAt = s.now()
	if err := writeJSON(s.runDir(runID)+"/state.json", state); err != nil {
		return nil, err
	}
	return state, nil
}

// Attempts reads every attempt in append order.
func (s *Store) Attempts(runID string) ([]Attempt, error) {
	path := filepath.Join(s.runDir(runID), "attempts.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Attempt
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var a Attempt
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			return nil, fmt.Errorf("decode attempt: %w", err)
		}
		out = append(out, a)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LatestAttempts keeps the newest attempt per case, as required by reports.
func LatestAttempts(attempts []Attempt) map[string]Attempt {
	out := make(map[string]Attempt, len(attempts))
	for _, a := range attempts {
		prev, ok := out[a.CaseID]
		if !ok || a.Attempt >= prev.Attempt {
			out[a.CaseID] = a
		}
	}
	return out
}

func attemptCount(path, caseID string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var a Attempt
		if err := json.Unmarshal(scanner.Bytes(), &a); err != nil {
			continue
		}
		if a.CaseID == caseID {
			count++
		}
	}
	return count, scanner.Err()
}

func appendAttempt(path string, a Attempt) error {
	line, err := a.MarshalJSONLine()
	if err != nil {
		return fmt.Errorf("encode attempt: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open attempts: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write attempt: %w", err)
	}
	return nil
}

type RunSummary struct {
	RunID         string    `json:"run_id"`
	DatasetName   string    `json:"dataset_name"`
	DatasetSHA256 string    `json:"dataset_sha256"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	FinishedAt    time.Time `json:"finished_at,omitempty"`
	Cases         int       `json:"cases"`
	Attempts      int       `json:"attempts"`
}

func (s *Store) List() ([]RunSummary, error) {
	dir := filepath.Join(s.Root, "runs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []RunSummary
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runID := entry.Name()
		m, err := s.LoadManifest(runID)
		if err != nil {
			continue
		}
		state, err := s.LoadState(runID)
		if err != nil {
			state = &RunState{RunID: runID, Status: "unknown", Cases: len(m.Dataset.Cases)}
		}
		out = append(out, RunSummary{
			RunID:         runID,
			DatasetName:   m.DatasetName,
			DatasetSHA256: m.DatasetSHA256,
			Status:        state.Status,
			CreatedAt:     m.CreatedAt,
			StartedAt:     state.StartedAt,
			FinishedAt:    state.FinishedAt,
			Cases:         len(m.Dataset.Cases),
			Attempts:      state.Attempts,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}