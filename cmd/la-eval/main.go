// Command la-eval is the Agent-facing evaluation CLI.
//
// It sits next to the component benchmarks (la-tool-bench, la-memory-bench, ...)
// and only covers cases executed through the real Agent loop.
//
// create-run freezes a case manifest and defaults to start=false. Execution is
// always a separate, explicit step.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	eval "github.com/yurika0211/luckyagent-app/eval"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "la-eval: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("missing subcommand")
	}
	switch args[0] {
	case "validate":
		return cmdValidate(args[1:])
	case "create-run":
		return cmdCreateRun(args[1:])
	case "start":
		return cmdStart(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "list":
		return cmdList(args[1:])
	case "report":
		return cmdReport(args[1:])
	case "ui":
		return cmdUI(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `la-eval: real Agent loop evaluation

  validate   -dataset cases.json
  create-run -dataset cases.json [-profile chat] [-workspace .] [-runs-dir DIR] [-start=false] [-executor agent|scripted -script FILE]
  start      -run RUN_ID [-runs-dir DIR] [-executor agent|scripted -script FILE] [-home OPERATOR_HOME] [provider flags]
  status     -run RUN_ID [-runs-dir DIR]
  list       [-runs-dir DIR]
  report     -run RUN_ID [-compare BASE_RUN_ID] [-out report.html] [-runs-dir DIR]
  ui         [-addr 127.0.0.1:3910] [-runs-dir DIR]

create-run never executes cases unless -start is passed explicitly.
Runs live under $LUCKYAGENT_EVAL_HOME or ~/.luckyagent-eval, separate from the
tested ~/.luckyagent surface.
`)
}

func defaultRoot() string {
	root, err := eval.DefaultRoot()
	if err != nil {
		return ".luckyagent-eval"
	}
	return root
}

func store(root string) *eval.Store {
	return &eval.Store{Root: root}
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	dataset := fs.String("dataset", "", "case dataset JSON path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dataset) == "" {
		return fmt.Errorf("-dataset is required")
	}
	ds, hash, err := eval.LoadDataset(*dataset)
	if err != nil {
		return err
	}
	out := map[string]any{
		"valid":          true,
		"schema_version": ds.SchemaVersion,
		"name":           ds.Name,
		"cases":          len(ds.Cases),
		"dataset_sha256": hash,
	}
	return printJSON(out)
}

func cmdCreateRun(args []string) error {
	fs := flag.NewFlagSet("create-run", flag.ContinueOnError)
	dataset := fs.String("dataset", "", "case dataset JSON path")
	runsDir := fs.String("runs-dir", defaultRoot(), "run store root")
	profile := fs.String("profile", "chat", "run profile: chat or long_task")
	workspace := fs.String("workspace", ".", "workspace (absolute, or relative to the run dir)")
	createdBy := fs.String("created-by", "cli", "operator identity")
	start := fs.Bool("start", false, "explicitly execute after freezing (default false)")
	home := fs.String("home", "", "operator config home (read-only provider settings)")
	provider := fs.String("provider", "", "provider override for execution")
	model := fs.String("model", "", "model override for execution")
	apiKey := fs.String("api-key", "", "API key override for execution")
	apiBase := fs.String("api-base", "", "API base override for execution")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-turn timeout when -start is used")
	executorName := fs.String("executor", "agent", "agent or scripted; scripted does not call a model")
	script := fs.String("script", "", "scripted turn file, required when -executor=scripted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dataset) == "" {
		return fmt.Errorf("-dataset is required")
	}
	ds, hash, err := eval.LoadDataset(*dataset)
	if err != nil {
		return err
	}
	st := store(*runsDir)
	m, err := st.CreateRun(ds, hash, eval.CreateOptions{
		Profile:        *profile,
		Workspace:      *workspace,
		CreatedBy:      *createdBy,
		StartRequested: *start,
	})
	if err != nil {
		return err
	}
	result := map[string]any{
		"run_id":         m.RunID,
		"status":         eval.RunStatusCreated,
		"dataset_name":   m.DatasetName,
		"dataset_sha256": m.DatasetSHA256,
		"cases":          len(m.Dataset.Cases),
		"profile":        m.Profile,
		"workspace":      m.Workspace,
		"path":           filepath.Join(st.Root, "runs", m.RunID),
		"started":        false,
	}
	if *start {
		state, err := executeRun(st, m.RunID, executorConfig{
			Name: *executorName, Script: *script, Home: *home,
			Provider: *provider, Model: *model, APIKey: *apiKey, APIBase: *apiBase, Timeout: *timeout,
		})
		if err != nil {
			return err
		}
		result["status"] = state.Status
		result["started"] = true
		result["attempts"] = state.Attempts
	}
	return printJSON(result)
}

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	runID := fs.String("run", "", "run id from create-run")
	runsDir := fs.String("runs-dir", defaultRoot(), "run store root")
	home := fs.String("home", "", "operator config home (read-only provider settings)")
	provider := fs.String("provider", "", "provider override")
	model := fs.String("model", "", "model override")
	apiKey := fs.String("api-key", "", "API key override")
	apiBase := fs.String("api-base", "", "API base override")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-turn timeout")
	executorName := fs.String("executor", "agent", "agent or scripted; scripted does not call a model")
	script := fs.String("script", "", "scripted turn file, required when -executor=scripted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*runID) == "" {
		return fmt.Errorf("-run is required")
	}
	st := store(*runsDir)
	state, err := executeRun(st, *runID, executorConfig{
		Name: *executorName, Script: *script, Home: *home,
		Provider: *provider, Model: *model, APIKey: *apiKey, APIBase: *apiBase, Timeout: *timeout,
	})
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"run_id":    state.RunID,
		"status":    state.Status,
		"attempts":  state.Attempts,
		"started_at": state.StartedAt,
		"finished_at": state.FinishedAt,
	})
}

type executorConfig struct {
	Name     string
	Script   string
	Home     string
	Provider string
	Model    string
	APIKey   string
	APIBase  string
	Timeout  time.Duration
}

func executeRun(st *eval.Store, runID string, cfg executorConfig) (*eval.RunState, error) {
	manifest, err := st.LoadManifest(runID)
	if err != nil {
		return nil, err
	}
	exec, closer, err := newExecutor(st, runID, cfg)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	state, err := st.StartRun(ctx, runID, exec)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "run %s %s: %d cases, %d attempts (dataset %s)\n",
		runID, state.Status, len(manifest.Dataset.Cases), state.Attempts, manifest.DatasetSHA256[:12])
	return state, nil
}

func newExecutor(st *eval.Store, runID string, cfg executorConfig) (eval.Executor, func(), error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Name)) {
	case "", "agent":
		exec := &eval.AgentExecutor{
			RunDir:       filepath.Join(st.Root, "runs", runID),
			OperatorHome: cfg.Home,
			Provider:     cfg.Provider,
			Model:        cfg.Model,
			APIKey:       cfg.APIKey,
			APIBase:      cfg.APIBase,
			Timeout:      cfg.Timeout,
		}
		return exec, func() { _ = exec.Close() }, nil
	case "scripted":
		if strings.TrimSpace(cfg.Script) == "" {
			return nil, nil, fmt.Errorf("-script is required when -executor=scripted")
		}
		exec, err := eval.LoadScripted(cfg.Script)
		if err != nil {
			return nil, nil, err
		}
		return exec, nil, nil
	default:
		return nil, nil, fmt.Errorf("unknown executor %q", cfg.Name)
	}
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	runID := fs.String("run", "", "run id")
	runsDir := fs.String("runs-dir", defaultRoot(), "run store root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*runID) == "" {
		return fmt.Errorf("-run is required")
	}
	st := store(*runsDir)
	state, err := st.LoadState(*runID)
	if err != nil {
		return err
	}
	attempts, err := st.Attempts(*runID)
	if err != nil {
		return err
	}
	latest := eval.LatestAttempts(attempts)
	counts := map[string]int{"pass": 0, "fail": 0, "skip": 0, "unknown": 0}
	execution := map[string]int{"completed": 0, "failed": 0, "unknown": 0}
	for _, a := range latest {
		counts[string(a.Verdict)]++
		execution[string(a.Execution)]++
	}
	return printJSON(map[string]any{
		"run_id":           state.RunID,
		"status":           state.Status,
		"cases":            state.Cases,
		"attempts":         len(attempts),
		"latest_verdicts":  counts,
		"latest_execution": execution,
		"started_at":       state.StartedAt,
		"finished_at":      state.FinishedAt,
	})
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	runsDir := fs.String("runs-dir", defaultRoot(), "run store root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	items, err := store(*runsDir).List()
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"runs": items})
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	runID := fs.String("run", "", "run id")
	compare := fs.String("compare", "", "base run id to compare against")
	out := fs.String("out", "", "HTML output path (default <run>/report.html)")
	runsDir := fs.String("runs-dir", defaultRoot(), "run store root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*runID) == "" {
		return fmt.Errorf("-run is required")
	}
	st := store(*runsDir)
	rep, err := st.WriteReport(*runID, *compare, *out)
	if err != nil {
		return err
	}
	path := *out
	if strings.TrimSpace(path) == "" {
		path = filepath.Join(st.Root, "runs", *runID, "report.html")
	}
	return printJSON(map[string]any{
		"run_id":          rep.RunID,
		"report":          path,
		"report_json":     filepath.Join(st.Root, "runs", *runID, "report.json"),
		"verdict_counts":  rep.Counts,
		"execution_counts": rep.Execution,
		"compared_to":     *compare,
	})
}

func cmdUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:3910", "listen address")
	runsDir := fs.String("runs-dir", defaultRoot(), "run store root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "la-eval ui: http://%s  runs=%s\n", ln.Addr().String(), *runsDir)
	return http.Serve(ln, (&eval.UI{Store: store(*runsDir)}).Handler())
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}