package eval

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type CaseReport struct {
	CaseID          string          `json:"case_id"`
	Title           string          `json:"title,omitempty"`
	Coverage        []string        `json:"coverage,omitempty"`
	Mode            string          `json:"mode"`
	Profile         string          `json:"profile"`
	Execution       ExecutionStatus `json:"execution"`
	Verdict         Verdict         `json:"verdict"`
	Attempt         int             `json:"attempt"`
	Reason          string          `json:"reason,omitempty"`
	AbortedAt       int             `json:"aborted_at_turn,omitempty"`
	Addendum        int             `json:"addendum_count,omitempty"`
	Turns           int             `json:"executed_turns"`
	DurationMS      float64         `json:"duration_ms"`
	OriginalVerdict Verdict         `json:"original_verdict,omitempty"`
	ReviewNote      string          `json:"review_note,omitempty"`
	Reviewer        string          `json:"reviewer,omitempty"`
	Reviewed        bool            `json:"reviewed,omitempty"`
}

type CompareDelta struct {
	CaseID       string          `json:"case_id"`
	Title        string          `json:"title,omitempty"`
	BaseVerdict  Verdict         `json:"base_verdict"`
	NewVerdict   Verdict         `json:"new_verdict"`
	BaseReason   string          `json:"base_reason,omitempty"`
	NewReason    string          `json:"new_reason,omitempty"`
	NewExecution ExecutionStatus `json:"new_execution"`
	Mode         string          `json:"mode,omitempty"`
}

type CompareReport struct {
	BaseRunID      string         `json:"base_run_id"`
	BaseDatasetSHA string         `json:"base_dataset_sha256"`
	NewDatasetSHA  string         `json:"new_dataset_sha256"`
	SameDataset    bool           `json:"same_dataset"`
	NewFailures    []CompareDelta `json:"new_failures"`
	Recoveries     []CompareDelta `json:"recoveries"`
	AddedCases     []CaseReport   `json:"added_cases,omitempty"`
	RemovedCaseIDs []string       `json:"removed_case_ids,omitempty"`
}

type Report struct {
	RunID         string         `json:"run_id"`
	DatasetName   string         `json:"dataset_name"`
	DatasetSHA256 string         `json:"dataset_sha256"`
	Status        string         `json:"status"`
	Profile       string         `json:"profile"`
	Workspace     string         `json:"workspace"`
	CreatedAt     time.Time      `json:"created_at"`
	StartedAt     time.Time      `json:"started_at,omitempty"`
	FinishedAt    time.Time      `json:"finished_at,omitempty"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Attempts      int            `json:"attempts"`
	Counts        map[string]int `json:"verdict_counts"`
	Execution     map[string]int `json:"execution_counts"`
	Cases         []CaseReport   `json:"cases"`
	Compare       *CompareReport `json:"compare,omitempty"`
}

// BuildReport derives a report from the frozen manifest and all attempts,
// keeping only the latest attempt per case.
func (s *Store) BuildReport(runID, compareRunID string) (*Report, error) {
	manifest, err := s.LoadManifest(runID)
	if err != nil {
		return nil, err
	}
	state, err := s.LoadState(runID)
	if err != nil {
		return nil, err
	}
	attempts, err := s.Attempts(runID)
	if err != nil {
		return nil, err
	}
	reviews, err := s.LoadReviews(runID)
	if err != nil {
		return nil, err
	}
	latest := LatestAttempts(attempts)
	rep := &Report{
		RunID:         runID,
		DatasetName:   manifest.DatasetName,
		DatasetSHA256: manifest.DatasetSHA256,
		Status:        state.Status,
		Profile:       manifest.Profile,
		Workspace:     manifest.Workspace,
		CreatedAt:     manifest.CreatedAt,
		StartedAt:     state.StartedAt,
		FinishedAt:    state.FinishedAt,
		GeneratedAt:   s.now(),
		Attempts:      len(attempts),
		Counts:        map[string]int{string(VerdictPass): 0, string(VerdictFail): 0, string(VerdictSkip): 0, string(VerdictUnknown): 0},
		Execution:     map[string]int{string(ExecutionCompleted): 0, string(ExecutionFailed): 0, string(ExecutionUnknown): 0},
	}
	for i := range manifest.Dataset.Cases {
		c := manifest.Dataset.Cases[i]
		item := CaseReport{
			CaseID:   c.ID,
			Title:    c.Title,
			Coverage: c.Coverage,
			Mode:     c.EffectiveMode(&manifest.Dataset),
			Profile:  c.EffectiveProfile(&manifest.Dataset),
		}
		if a, ok := latest[c.ID]; ok {
			item.Execution = a.Execution
			item.Verdict = a.Verdict
			item.Attempt = a.Attempt
			item.Reason = a.Reason
			item.AbortedAt = a.AbortedAtTurn
			item.Addendum = len(a.Addendum)
			item.Turns = len(a.Turns)
			item.DurationMS = a.DurationMS
			if review, ok := reviews[c.ID]; ok && review.Attempt == a.Attempt {
				item.OriginalVerdict = a.Verdict
				item.Verdict = review.Verdict
				item.ReviewNote = review.Note
				item.Reviewer = review.Reviewer
				item.Reviewed = true
			}
		} else {
			item.Execution = ExecutionUnknown
			item.Verdict = VerdictUnknown
			item.Reason = "case has no attempt in this run"
		}
		rep.Counts[string(item.Verdict)]++
		rep.Execution[string(item.Execution)]++
		rep.Cases = append(rep.Cases, item)
	}

	if strings.TrimSpace(compareRunID) != "" {
		cmp, err := s.buildCompare(compareRunID, rep)
		if err != nil {
			return nil, err
		}
		rep.Compare = cmp
	}
	return rep, nil
}

func (s *Store) buildCompare(baseRunID string, newRep *Report) (*CompareReport, error) {
	baseManifest, err := s.LoadManifest(baseRunID)
	if err != nil {
		return nil, err
	}
	baseAttempts, err := s.Attempts(baseRunID)
	if err != nil {
		return nil, err
	}
	baseLatest := LatestAttempts(baseAttempts)
	cmp := &CompareReport{
		BaseRunID:      baseRunID,
		BaseDatasetSHA: baseManifest.DatasetSHA256,
		NewDatasetSHA:  newRep.DatasetSHA256,
		SameDataset:    baseManifest.DatasetSHA256 == newRep.DatasetSHA256 && baseManifest.DatasetSHA256 != "",
	}
	newIDs := map[string]bool{}
	for _, item := range newRep.Cases {
		newIDs[item.CaseID] = true
		base, ok := baseLatest[item.CaseID]
		if !ok {
			cmp.AddedCases = append(cmp.AddedCases, item)
			continue
		}
		delta := CompareDelta{
			CaseID:       item.CaseID,
			Title:        item.Title,
			BaseVerdict:  base.Verdict,
			NewVerdict:   item.Verdict,
			BaseReason:   base.Reason,
			NewReason:    item.Reason,
			NewExecution: item.Execution,
			Mode:         item.Mode,
		}
		switch {
		case base.Verdict != VerdictFail && item.Verdict == VerdictFail:
			cmp.NewFailures = append(cmp.NewFailures, delta)
		case base.Verdict == VerdictFail && item.Verdict == VerdictPass:
			cmp.Recoveries = append(cmp.Recoveries, delta)
		}
	}
	seenRemoved := map[string]bool{}
	for id := range baseLatest {
		if !newIDs[id] && !seenRemoved[id] {
			seenRemoved[id] = true
			cmp.RemovedCaseIDs = append(cmp.RemovedCaseIDs, id)
		}
	}
	sort.Strings(cmp.RemovedCaseIDs)
	return cmp, nil
}

// WriteReport writes a versioned HTML snapshot plus report.json into the run
// directory. An explicit outPath writes the HTML elsewhere while report.json
// remains with the run.
func (s *Store) WriteReport(runID, compareRunID, outPath string) (*Report, error) {
	rep, err := s.BuildReport(runID, compareRunID)
	if err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(s.runDir(runID), "report.json"), rep); err != nil {
		return nil, err
	}
	if strings.TrimSpace(outPath) == "" {
		outPath = filepath.Join(s.runDir(runID), "report.html")
	}
	var buf bytes.Buffer
	if err := reportTemplate.Execute(&buf, rep); err != nil {
		return nil, fmt.Errorf("render report: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(outPath, buf.Bytes(), 0o600); err != nil {
		return nil, fmt.Errorf("write report: %w", err)
	}
	return rep, nil
}

var reportTemplate = template.Must(template.New("report").Funcs(template.FuncMap{
	"cls": func(v any) string {
		switch fmt.Sprint(v) {
		case "pass", "completed":
			return "ok"
		case "fail", "failed":
			return "bad"
		case "skip":
			return "skip"
		default:
			return "unknown"
		}
	},
	"join": func(v []string) string { return strings.Join(v, ", ") },
}).Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>Agent Eval {{.RunID}}</title>
<style>
:root { color-scheme: light dark; }
body { font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 24px; max-width: 1200px; }
h1, h2 { font-weight: 600; }
.meta { display: grid; grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); gap: 8px 24px; margin: 16px 0 24px; }
.meta div { border-bottom: 1px dashed #8884; padding: 4px 0; }
code { font-family: ui-monospace, SFMono-Regular, monospace; }
table { border-collapse: collapse; width: 100%; margin: 12px 0 28px; }
th, td { text-align: left; padding: 6px 8px; border-bottom: 1px solid #8884; vertical-align: top; }
th { font-weight: 600; }
.summary { display: flex; flex-wrap: wrap; gap: 12px; margin: 16px 0; }
.card { border: 1px solid #8884; border-radius: 8px; padding: 10px 16px; min-width: 120px; }
.card b { display: block; font-size: 24px; }
.badge { display: inline-block; padding: 1px 8px; border-radius: 999px; font-size: 12px; border: 1px solid currentColor; }
.badge.ok { color: #0b7a32; background: #d4f5de; }
.badge.bad { color: #8c1d18; background: #fadad7; }
.badge.skip { color: #8a5a00; background: #ffe9c2; }
.badge.unknown { color: #334; background: #e4e6ea; }
@media (prefers-color-scheme: dark) {
  .badge.ok { color: #6ee7a0; background: #0f3d24; }
  .badge.bad { color: #ff8a80; background: #4a1c18; }
  .badge.skip { color: #ffcc66; background: #3d2e12; }
  .badge.unknown { color: #c5c9d1; background: #2a2e36; }
}
td, pre { overflow-wrap: anywhere; word-break: break-word; }
.empty { opacity: .7; }
</style>
</head>
<body>
<h1>Agent Eval Report</h1>
<div class="meta">
  <div>Run: <code>{{.RunID}}</code></div>
  <div>Dataset: {{.DatasetName}}</div>
  <div>Dataset SHA-256: <code>{{.DatasetSHA256}}</code></div>
  <div>Status: {{.Status}}</div>
  <div>Profile: {{.Profile}}</div>
  <div>Workspace: <code>{{.Workspace}}</code></div>
  <div>Created: {{.CreatedAt}}</div>
  <div>Started: {{.StartedAt}}</div>
  <div>Finished: {{.FinishedAt}}</div>
  <div>Generated: {{.GeneratedAt}}</div>
  <div>Attempts in JSONL: {{.Attempts}} (report keeps the latest per case)</div>
</div>

<div class="summary">
  <div class="card"><b>{{index .Counts "pass"}}</b>verdict pass</div>
  <div class="card"><b>{{index .Counts "fail"}}</b>verdict fail</div>
  <div class="card"><b>{{index .Counts "skip"}}</b>verdict skip</div>
  <div class="card"><b>{{index .Counts "unknown"}}</b>verdict unknown</div>
  <div class="card"><b>{{index .Execution "completed"}}</b>execution completed</div>
  <div class="card"><b>{{index .Execution "failed"}}</b>execution failed</div>
  <div class="card"><b>{{index .Execution "unknown"}}</b>execution unknown</div>
</div>

{{if .Compare}}
<h2>对照 {{.Compare.BaseRunID}}</h2>
<p>{{if .Compare.SameDataset}}两次 run 的 dataset sha 相同。{{else}}两次 run 的 dataset sha 不同：清单已经改过，新增/消失用例可能来自改题而不是执行差异。{{end}} 本轮 <code>{{.Compare.NewDatasetSHA}}</code> · 基线 <code>{{.Compare.BaseDatasetSHA}}</code></p>
<h3>新增失败 ({{len .Compare.NewFailures}})</h3>
{{if .Compare.NewFailures}}
<table>
<tr><th>Case</th><th>Mode</th><th>基线 verdict</th><th>本轮 execution</th><th>本轮 verdict</th><th>基线原因</th><th>本轮原因</th></tr>
{{range .Compare.NewFailures}}
<tr><td><code>{{.CaseID}}</code><br>{{.Title}}</td><td>{{.Mode}}</td><td><span class="badge {{cls .BaseVerdict}}">{{.BaseVerdict}}</span></td><td><span class="badge {{cls .NewExecution}}">{{.NewExecution}}</span></td><td><span class="badge bad">{{.NewVerdict}}</span></td><td>{{.BaseReason}}</td><td>{{.NewReason}}</td></tr>
{{end}}
</table>
{{else}}<p class="empty">没有新增失败。</p>{{end}}

<h3>恢复 ({{len .Compare.Recoveries}})</h3>
{{if .Compare.Recoveries}}
<table>
<tr><th>Case</th><th>Mode</th><th>基线 verdict</th><th>本轮 execution</th><th>本轮 verdict</th><th>基线原因</th><th>本轮原因</th></tr>
{{range .Compare.Recoveries}}
<tr><td><code>{{.CaseID}}</code><br>{{.Title}}</td><td>{{.Mode}}</td><td><span class="badge bad">{{.BaseVerdict}}</span></td><td><span class="badge {{cls .NewExecution}}">{{.NewExecution}}</span></td><td><span class="badge ok">{{.NewVerdict}}</span></td><td>{{.BaseReason}}</td><td>{{.NewReason}}</td></tr>
{{end}}
</table>
{{else}}<p class="empty">没有恢复项。</p>{{end}}

{{if .Compare.AddedCases}}<h3>新增用例 ({{len .Compare.AddedCases}})</h3>
<p>{{range $i, $c := .Compare.AddedCases}}{{if $i}}, {{end}}<code>{{$c.CaseID}}</code>{{end}}</p>{{end}}
{{if .Compare.RemovedCaseIDs}}<h3>基线有、本轮没有的用例</h3><p>{{join .Compare.RemovedCaseIDs}}</p>{{end}}
{{end}}

<h2>用例（最新一条尝试）</h2>
<table>
<tr><th>Case</th><th>Coverage</th><th>Mode</th><th>Execution</th><th>Verdict</th><th>Attempt</th><th>Turns</th><th>Addendum</th><th>Aborted</th><th>Reason</th></tr>
{{range .Cases}}
<tr>
<td><code>{{.CaseID}}</code><br>{{.Title}}</td>
<td>{{join .Coverage}}</td>
<td>{{.Mode}}</td>
<td><span class="badge {{cls .Execution}}">{{.Execution}}</span></td>
<td><span class="badge {{cls .Verdict}}">{{.Verdict}}</span>{{if .Reviewed}}<br><span class="empty">原判 {{.OriginalVerdict}} · {{.Reviewer}}: {{.ReviewNote}}</span>{{end}}</td>
<td>{{.Attempt}}</td>
<td>{{.Turns}}</td>
<td>{{.Addendum}}</td>
<td>{{.AbortedAt}}</td>
<td>{{.Reason}}</td>
</tr>
{{end}}
</table>
</body>
</html>
`))
