package eval

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strings"
)

//go:embed ui/index.html
var uiFS embed.FS

// UI is a read-only view over a run store. Creating and starting runs stays
// on the CLI so a page load cannot execute cases.
type UI struct {
	Store *Store
}

func (u *UI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", u.handleIndex)
	mux.HandleFunc("GET /api/schema", u.handleSchema)
	mux.HandleFunc("POST /api/datasets/validate", u.handleValidate)
	mux.HandleFunc("POST /api/runs", u.handleCreateRun)
	mux.HandleFunc("GET /api/runs", u.handleRuns)
	mux.HandleFunc("GET /api/runs/{id}", u.handleRun)
	mux.HandleFunc("POST /api/runs/{id}/start", u.handleStart)
	mux.HandleFunc("POST /api/runs/{id}/reviews", u.handleReview)
	mux.HandleFunc("GET /api/runs/{id}/report", u.handleReport)
	mux.HandleFunc("GET /api/compare", u.handleCompare)
	return mux
}

func (u *UI) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(uiFS, "ui/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (u *UI) handleSchema(w http.ResponseWriter, r *http.Request) {
	writeUIJSON(w, map[string]any{
		"schema_version":               SchemaVersion,
		"turn_contract":                []string{"input", "expected", "evaluation", "wait"},
		"modes":                        []string{ModeReplayAbort, ModeReplayResume, ModeInjectAbort},
		"forbidden_mode":               modeInjectResume,
		"validators":                   []string{"keyword", "regex", "trace", "memory_write"},
		"execution":                    []string{string(ExecutionCompleted), string(ExecutionFailed), string(ExecutionUnknown)},
		"verdict":                      []string{string(VerdictPass), string(VerdictFail), string(VerdictSkip), string(VerdictUnknown)},
		"profiles":                     []string{"chat", "long_task"},
		"coverage":                     []string{"chat", "tool_route", "memory_write", "long_task"},
		"create_run_starts_by_default": false,
		"note":                         "未配置的验证器是 skip，证据不足是 unknown，两者都不算通过。inject_resume 直接拒绝。人工复核改最终结论，裁判原判保留在 reviews.json，不改 attempts.jsonl。",
	})
}

func (u *UI) handleValidate(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 2<<20)
	if err != nil {
		writeUIMessage(w, http.StatusBadRequest, err.Error())
		return
	}
	ds, err := DecodeDataset(body)
	if err != nil {
		writeUIJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{
			"valid": false,
			"error": err.Error(),
		})
		return
	}
	hash, err := DatasetHash(ds)
	if err != nil {
		writeUIError(w, err)
		return
	}
	cases := make([]map[string]any, 0, len(ds.Cases))
	for i := range ds.Cases {
		c := &ds.Cases[i]
		turns := make([]map[string]any, 0, len(c.Turns))
		for ti := range c.Turns {
			ev := c.Turns[ti].Evaluation
			names := []string{}
			if ev != nil {
				if ev.Keyword != nil {
					names = append(names, "keyword")
				}
				if ev.Regex != nil {
					names = append(names, "regex")
				}
				if ev.Trace != nil {
					names = append(names, "trace")
				}
				if ev.MemoryWrite != nil {
					names = append(names, "memory_write")
				}
			}
			status := "configured"
			if len(names) == 0 {
				status = "skip"
			}
			turns = append(turns, map[string]any{
				"index":      ti + 1,
				"input":      c.Turns[ti].Input,
				"expected":   c.Turns[ti].Expected,
				"wait":       c.Turns[ti].Wait,
				"validators": names,
				"status":     status,
			})
		}
		cases = append(cases, map[string]any{
			"id":         c.ID,
			"title":      c.Title,
			"coverage":   c.Coverage,
			"mode":       c.EffectiveMode(ds),
			"profile":    c.EffectiveProfile(ds),
			"workspace":  c.EffectiveWorkspace(ds),
			"turns":      len(c.Turns),
			"evaluated":  c.TurnIndexes(ds),
			"skip":       c.Skip,
			"turn_views": turns,
		})
	}
	writeUIJSON(w, map[string]any{
		"valid":          true,
		"schema_version": ds.SchemaVersion,
		"name":           ds.Name,
		"description":    ds.Description,
		"dataset_sha256": hash,
		"cases":          cases,
	})
}

func (u *UI) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dataset   json.RawMessage `json:"dataset"`
		Profile   string          `json:"profile"`
		Workspace string          `json:"workspace"`
		CreatedBy string          `json:"created_by"`
		Start     bool            `json:"start"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeUIMessage(w, http.StatusBadRequest, "decode request: "+err.Error())
		return
	}
	if req.Start {
		writeUIMessage(w, http.StatusBadRequest, "webui create-run refuses start=true; freeze the manifest first, then POST /api/runs/{id}/start")
		return
	}
	ds, err := DecodeDataset(req.Dataset)
	if err != nil {
		writeUIJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	hash, err := DatasetHash(ds)
	if err != nil {
		writeUIError(w, err)
		return
	}
	createdBy := strings.TrimSpace(req.CreatedBy)
	if createdBy == "" {
		createdBy = "webui"
	}
	m, err := u.Store.CreateRun(ds, hash, CreateOptions{
		Profile:        req.Profile,
		Workspace:      req.Workspace,
		CreatedBy:      createdBy,
		StartRequested: false,
	})
	if err != nil {
		writeUIError(w, err)
		return
	}
	w.Header().Set("Location", "/api/runs/"+m.RunID)
	writeUIJSONStatus(w, http.StatusCreated, map[string]any{
		"run_id":         m.RunID,
		"status":         RunStatusCreated,
		"dataset_name":   m.DatasetName,
		"dataset_sha256": m.DatasetSHA256,
		"cases":          len(m.Dataset.Cases),
		"profile":        m.Profile,
		"workspace":      m.Workspace,
		"started":        false,
	})
}

func (u *UI) handleStart(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	var req struct {
		Executor string `json:"executor"`
		Script   string `json:"script"`
		Confirm  bool   `json:"confirm"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeUIMessage(w, http.StatusBadRequest, "decode request: "+err.Error())
			return
		}
	}
	if !req.Confirm {
		writeUIMessage(w, http.StatusBadRequest, "start requires confirm=true; a page view never starts a run")
		return
	}
	execName := strings.ToLower(strings.TrimSpace(req.Executor))
	if execName == "" {
		execName = "scripted"
	}
	if execName == "agent" {
		writeUIMessage(w, http.StatusBadRequest, "webui refuses the real agent executor; use la-eval start -executor agent from the operator shell")
		return
	}
	if execName != "scripted" {
		writeUIMessage(w, http.StatusBadRequest, "unknown executor "+execName)
		return
	}
	if strings.TrimSpace(req.Script) == "" {
		writeUIMessage(w, http.StatusBadRequest, "script is required for the scripted executor")
		return
	}
	exec, err := LoadScripted(req.Script)
	if err != nil {
		writeUIMessage(w, http.StatusBadRequest, err.Error())
		return
	}
	state, err := u.Store.StartRun(r.Context(), runID, exec)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "already ") {
			status = http.StatusConflict
		}
		if strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "open manifest.json") {
			status = http.StatusNotFound
		}
		writeUIMessage(w, status, err.Error())
		return
	}
	writeUIJSON(w, state)
}

func (u *UI) handleReview(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	var req struct {
		CaseID   string  `json:"case_id"`
		Verdict  Verdict `json:"verdict"`
		Note     string  `json:"note"`
		Reviewer string  `json:"reviewer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeUIMessage(w, http.StatusBadRequest, "decode request: "+err.Error())
		return
	}
	saved, err := u.Store.SaveReview(runID, Review{
		CaseID:   req.CaseID,
		Verdict:  req.Verdict,
		Note:     req.Note,
		Reviewer: req.Reviewer,
	})
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "open manifest.json") {
			status = http.StatusNotFound
		}
		writeUIMessage(w, status, err.Error())
		return
	}
	writeUIJSON(w, saved)
}

func (u *UI) handleRuns(w http.ResponseWriter, r *http.Request) {
	items, err := u.Store.List()
	if err != nil {
		writeUIError(w, err)
		return
	}
	if items == nil {
		items = []RunSummary{}
	}
	writeUIJSON(w, map[string]any{"runs": items, "root": u.Store.Root})
}

type uiTool struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	Failed    bool   `json:"failed,omitempty"`
}

type uiTurn struct {
	Index             int                   `json:"index"`
	Input             string                `json:"input"`
	Expected          string                `json:"expected,omitempty"`
	Execution         ExecutionStatus       `json:"execution"`
	Verdict           Verdict               `json:"verdict"`
	Reason            string                `json:"reason,omitempty"`
	Response          string                `json:"response,omitempty"`
	Tools             []uiTool              `json:"tools,omitempty"`
	Checks            []CheckResult         `json:"checks,omitempty"`
	Addendum          string                `json:"addendum,omitempty"`
	AddendumDelivered bool                  `json:"addendum_delivered"`
	DurationMS        float64               `json:"duration_ms"`
	MemoryWrites      []MemoryWriteEvidence `json:"memory_writes,omitempty"`
	MemoryDump        string                `json:"memory_dump,omitempty"`
	TraceAvailable    bool                  `json:"trace_available"`
	ResponseAvailable bool                  `json:"response_available"`
}

type uiAttempt struct {
	Attempt           int             `json:"attempt"`
	Execution         ExecutionStatus `json:"execution"`
	Verdict           Verdict         `json:"verdict"`
	OriginalVerdict   Verdict         `json:"original_verdict,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	Mode              string          `json:"mode"`
	Addendum          int             `json:"addendum_count"`
	AddendumDelivered bool            `json:"addendum_delivered"`
	Latest            bool            `json:"latest"`
	AdoptedByReport   bool            `json:"adopted_by_report"`
	Turns             []uiTurn        `json:"turns"`
}

func (u *UI) handleRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	manifest, err := u.Store.LoadManifest(runID)
	if err != nil {
		writeUIError(w, err)
		return
	}
	state, err := u.Store.LoadState(runID)
	if err != nil {
		writeUIError(w, err)
		return
	}
	attempts, err := u.Store.Attempts(runID)
	if err != nil {
		writeUIError(w, err)
		return
	}
	reviews, err := u.Store.LoadReviews(runID)
	if err != nil {
		writeUIError(w, err)
		return
	}
	latest := LatestAttempts(attempts)
	byCase := map[string][]uiAttempt{}
	for _, attempt := range attempts {
		isLatest := latest[attempt.CaseID].Attempt == attempt.Attempt
		item := uiAttempt{
			Attempt:           attempt.Attempt,
			Execution:         attempt.Execution,
			Verdict:           attempt.Verdict,
			OriginalVerdict:   attempt.Verdict,
			Reason:            attempt.Reason,
			Mode:              attempt.Mode,
			Addendum:          len(attempt.Addendum),
			AddendumDelivered: attempt.AddendumDelivered(),
			Latest:            isLatest,
			AdoptedByReport:   isLatest,
		}
		for _, turn := range attempt.Turns {
			view := uiTurn{
				Index:             turn.Index,
				Input:             turn.Input,
				Execution:         turn.Execution,
				Verdict:           turn.Verdict,
				Reason:            turn.Reason,
				Response:          turn.Evidence.Response,
				Checks:            turn.Checks,
				Addendum:          turn.Addendum,
				AddendumDelivered: strings.TrimSpace(turn.Addendum) != "",
				DurationMS:        turn.DurationMS,
				MemoryWrites:      turn.Evidence.MemoryWrites,
				MemoryDump:        turn.Evidence.MemoryDump,
				TraceAvailable:    turn.Evidence.TraceAvailable,
				ResponseAvailable: turn.Evidence.ResponseAvailable,
			}
			for _, tool := range turn.Evidence.Tools {
				if name := strings.TrimSpace(tool.Name); name == "" {
					continue
				}
				view.Tools = append(view.Tools, uiTool{
					Name: tool.Name, Arguments: tool.Arguments, Result: tool.Result, Failed: tool.Failed,
				})
			}
			item.Turns = append(item.Turns, view)
		}
		byCase[attempt.CaseID] = append(byCase[attempt.CaseID], item)
	}
	cases := make([]map[string]any, 0, len(manifest.Dataset.Cases))
	for i := range manifest.Dataset.Cases {
		c := manifest.Dataset.Cases[i]
		items := byCase[c.ID]
		sort.Slice(items, func(a, b int) bool { return items[a].Attempt > items[b].Attempt })
		frozen := make([]map[string]any, 0, len(c.Turns))
		for ti := range c.Turns {
			t := c.Turns[ti]
			validators := []string{}
			if t.Evaluation != nil {
				if t.Evaluation.Keyword != nil {
					validators = append(validators, "keyword")
				}
				if t.Evaluation.Regex != nil {
					validators = append(validators, "regex")
				}
				if t.Evaluation.Trace != nil {
					validators = append(validators, "trace")
				}
				if t.Evaluation.MemoryWrite != nil {
					validators = append(validators, "memory_write")
				}
			}
			frozen = append(frozen, map[string]any{
				"index":      ti + 1,
				"input":      t.Input,
				"expected":   t.Expected,
				"wait":       t.Wait,
				"validators": validators,
				"evaluation": t.Evaluation,
			})
		}
		expectedByIndex := map[int]string{}
		for ti := range c.Turns {
			expectedByIndex[ti+1] = c.Turns[ti].Expected
		}
		for i := range items {
			for j := range items[i].Turns {
				items[i].Turns[j].Expected = expectedByIndex[items[i].Turns[j].Index]
				items[i].Turns[j].Checks = completeChecks(c.Turns, items[i].Turns[j])
			}
		}
		autoVerdict := VerdictUnknown
		autoExecution := ExecutionUnknown
		if latestAttempt, ok := latest[c.ID]; ok {
			autoVerdict = latestAttempt.Verdict
			autoExecution = latestAttempt.Execution
		}
		finalVerdict := autoVerdict
		reviewed := false
		var review any
		if item, ok := reviews[c.ID]; ok {
			review = item
			reviewed = true
			if latestAttempt, ok := latest[c.ID]; ok && item.Attempt == latestAttempt.Attempt {
				finalVerdict = item.Verdict
				for i := range items {
					if items[i].Latest {
						items[i].Verdict = item.Verdict
						items[i].OriginalVerdict = item.Original
					}
				}
			}
		}
		caseView := map[string]any{
			"id":            c.ID,
			"title":         c.Title,
			"coverage":      c.Coverage,
			"mode":          c.EffectiveMode(&manifest.Dataset),
			"profile":       c.EffectiveProfile(&manifest.Dataset),
			"workspace":     c.EffectiveWorkspace(&manifest.Dataset),
			"skip":          c.Skip,
			"turns":         len(c.Turns),
			"frozen":        frozen,
			"attempts":      items,
			"execution":     autoExecution,
			"auto_verdict":  autoVerdict,
			"final_verdict": finalVerdict,
			"reviewed":      reviewed,
			"has_attempts":  len(items) > 0,
		}
		if review != nil {
			caseView["review"] = review
		}
		cases = append(cases, caseView)
	}
	writeUIJSON(w, map[string]any{
		"run_id":          manifest.RunID,
		"dataset_name":    manifest.DatasetName,
		"dataset_sha256":  manifest.DatasetSHA256,
		"profile":         manifest.Profile,
		"workspace":       manifest.Workspace,
		"status":          state.Status,
		"created_by":      manifest.CreatedBy,
		"created_at":      manifest.CreatedAt,
		"frozen_at":       manifest.FrozenAt,
		"started_at":      state.StartedAt,
		"finished_at":     state.FinishedAt,
		"start_requested": manifest.StartRequested,
		"attempts":        len(attempts),
		"cases":           cases,
	})
}

func (u *UI) handleReport(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	rep, err := u.Store.BuildReport(runID, r.URL.Query().Get("compare"))
	if err != nil {
		writeUIError(w, err)
		return
	}
	var buf strings.Builder
	if err := reportTemplate.Execute(&buf, rep); err != nil {
		writeUIError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(buf.String()))
}

func (u *UI) handleCompare(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.URL.Query().Get("run"))
	baseID := strings.TrimSpace(r.URL.Query().Get("base"))
	if runID == "" || baseID == "" {
		writeUIMessage(w, http.StatusBadRequest, "run and base are required")
		return
	}
	rep, err := u.Store.BuildReport(runID, baseID)
	if err != nil {
		writeUIError(w, err)
		return
	}
	writeUIJSON(w, rep)
}

func writeUIJSON(w http.ResponseWriter, v any) {
	writeUIJSONStatus(w, http.StatusOK, v)
}

func writeUIJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("body exceeds %d bytes", limit)
	}
	return data, nil
}

func writeUIError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "open manifest.json") {
		status = http.StatusNotFound
	}
	writeUIMessage(w, status, err.Error())
}

func writeUIMessage(w http.ResponseWriter, status int, msg string) {
	writeUIJSONStatus(w, status, map[string]any{"error": msg})
}

func completeChecks(turns []Turn, view uiTurn) []CheckResult {
	idx := view.Index - 1
	var evaln *Evaluation
	if idx >= 0 && idx < len(turns) {
		evaln = turns[idx].Evaluation
	}
	have := map[string]CheckResult{}
	for _, c := range view.Checks {
		have[c.Validator] = c
	}
	names := []string{"keyword", "regex", "trace", "memory_write"}
	out := make([]CheckResult, 0, 4)
	for _, name := range names {
		if c, ok := have[name]; ok {
			out = append(out, c)
			continue
		}
		configured := false
		if evaln != nil {
			switch name {
			case "keyword":
				configured = evaln.Keyword != nil
			case "regex":
				configured = evaln.Regex != nil
			case "trace":
				configured = evaln.Trace != nil
			case "memory_write":
				configured = evaln.MemoryWrite != nil
			}
		}
		if configured {
			out = append(out, CheckResult{Validator: name, Status: "unknown", Reason: "validator produced no result"})
		} else {
			out = append(out, CheckResult{Validator: name, Status: "skip", Reason: "validator not configured"})
		}
	}
	return out
}

func ListenAndServeUI(addr string, store *Store) error {
	if store == nil || strings.TrimSpace(store.Root) == "" {
		return fmt.Errorf("ui store root is required")
	}
	if strings.TrimSpace(addr) == "" {
		addr = "127.0.0.1:3910"
	}
	return http.ListenAndServe(addr, (&UI{Store: store}).Handler())
}
