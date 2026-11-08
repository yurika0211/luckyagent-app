package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	appboot "github.com/yurika0211/luckyagent-app/bootstrap"
	"github.com/yurika0211/luckyagent-runtime/config"
	"github.com/yurika0211/luckyagent-runtime/session"
)

// AgentExecutor runs frozen cases through the real Agent loop.
//
// The tested data surface (memory vault, sessions) lives under the run
// directory, never the operator's ~/.luckyagent. The operator config is read
// only to copy provider settings.
type AgentExecutor struct {
	RunDir       string
	OperatorHome string
	Provider     string
	Model        string
	APIKey       string
	APIBase      string
	Timeout      time.Duration

	cases map[string]*caseRuntime
}

type caseRuntime struct {
	agent   *agent.Agent
	session *session.Session
	home    string
}

func (e *AgentExecutor) runtimeFor(req TurnRequest) (*caseRuntime, error) {
	if e.cases == nil {
		e.cases = map[string]*caseRuntime{}
	}
	if rt, ok := e.cases[req.Case.ID]; ok {
		return rt, nil
	}
	if strings.TrimSpace(e.RunDir) == "" {
		return nil, fmt.Errorf("agent executor: run dir is required")
	}
	base := SafeID(req.Case.ID)
	caseRoot := filepath.Join(e.RunDir, "cases", base)
	home := filepath.Join(caseRoot, "home")
	workspace := resolveWorkspace(e.RunDir, caseRoot, req.Workspace)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return nil, fmt.Errorf("create case workspace: %w", err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create case home: %w", err)
	}

	mgr, err := config.NewManagerWithDir(home)
	if err != nil {
		return nil, fmt.Errorf("case config: %w", err)
	}
	if err := mgr.InitHome(); err != nil {
		return nil, fmt.Errorf("case init home: %w", err)
	}
	cfg, err := e.isolatedConfig()
	if err != nil {
		return nil, err
	}
	mgr.SetConfig(cfg)
	if err := mgr.Save(); err != nil {
		return nil, fmt.Errorf("save case config: %w", err)
	}

	a, err := appboot.New(mgr)
	if err != nil {
		return nil, fmt.Errorf("boot case agent: %w", err)
	}
	sess := session.NewSession("eval-"+base, filepath.Join(home, "sessions"))
	sess.SetTitle("Agent Eval " + req.Case.ID)
	sess.SetCwd(workspace)
	rt := &caseRuntime{agent: a, session: sess, home: home}
	e.cases[req.Case.ID] = rt
	return rt, nil
}

func (e *AgentExecutor) isolatedConfig() (*config.Config, error) {
	operatorHome := strings.TrimSpace(e.OperatorHome)
	var src *config.Manager
	var err error
	if operatorHome != "" {
		src, err = config.NewManagerWithDir(operatorHome)
	} else {
		src, err = config.NewManager()
	}
	if err != nil {
		return nil, fmt.Errorf("operator config: %w", err)
	}
	_ = src.Load()
	cfg := cloneConfig(src.Get())
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	if v := strings.TrimSpace(e.Provider); v != "" {
		cfg.LlmProvider.Name = v
		cfg.Provider = v
	}
	if v := strings.TrimSpace(e.Model); v != "" {
		cfg.LlmProvider.Model = v
		cfg.Model = v
	}
	if v := strings.TrimSpace(e.APIKey); v != "" {
		cfg.LlmProvider.APIKey = v
		cfg.APIKey = v
	}
	if v := strings.TrimSpace(e.APIBase); v != "" {
		cfg.LlmProvider.BaseURL = v
		cfg.APIBase = v
	}
	return cfg, nil
}

func cloneConfig(in *config.Config) *config.Config {
	if in == nil {
		return nil
	}
	data, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	var out config.Config
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	// json:"-" aliases are not in the serialized value.
	out.Provider = in.Provider
	out.Model = in.Model
	out.APIKey = in.APIKey
	out.APIBase = in.APIBase
	return &out
}

func resolveWorkspace(runDir, caseRoot, configured string) string {
	configured = strings.TrimSpace(configured)
	if configured == "" || configured == "." {
		return filepath.Join(caseRoot, "workspace")
	}
	if filepath.IsAbs(configured) {
		return configured
	}
	return filepath.Join(runDir, configured)
}

func (e *AgentExecutor) Execute(ctx context.Context, req TurnRequest) (TurnResult, error) {
	rt, err := e.runtimeFor(req)
	if err != nil {
		return TurnResult{}, err
	}
	loopCfg := agent.LoopConfig{
		AutoApprove: true,
		Ephemeral:   true,
		Source:      "eval",
		SessionID:   rt.session.ID,
		Timeout:     e.Timeout,
		MaxIterations: maxIterations(req.Profile),
	}
	result, err := rt.agent.RunLoopWithSessionInput(ctx, rt.session, agent.TextUserTurnInput(req.Input), loopCfg)
	if result == nil {
		if err != nil {
			return TurnResult{}, err
		}
		return TurnResult{}, fmt.Errorf("agent returned nil result")
	}

	out := TurnResult{
		Response:          result.Response,
		ResponseAvailable: strings.TrimSpace(result.Response) != "",
		TraceAvailable:    true,
	}
	for _, call := range result.ToolCalls {
		out.Tools = append(out.Tools, ToolEvidence{
			Name:      call.Name,
			Arguments: truncate(call.Arguments, 8_000),
			Result:    truncate(call.Result, 8_000),
		})
		if write, ok := memoryWriteFromCall(call.Name, call.Arguments, call.Result, err); ok {
			out.MemoryWrites = append(out.MemoryWrites, write)
		}
	}
	out.MemoryDump, out.MemoryDumpAvailable = scanMemoryVault(rt.home)
	out.SessionMessages = len(rt.session.GetMessages())
	out.SessionTranscriptPath = filepath.Join(rt.home, "sessions", rt.session.ID+".jsonl")
	return out, err
}

func (e *AgentExecutor) CloseCase(caseID string) error {
	if e.cases == nil {
		return nil
	}
	rt, ok := e.cases[caseID]
	if !ok {
		return nil
	}
	delete(e.cases, caseID)
	if rt.agent != nil {
		return rt.agent.Close()
	}
	return nil
}

func (e *AgentExecutor) Close() error {
	for id := range e.cases {
		_ = e.CloseCase(id)
	}
	return nil
}

func maxIterations(profile string) int {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "long_task", "long-task", "longtask":
		return 80
	default:
		return 0
	}
}

func memoryWriteFromCall(name, args, result string, loopErr error) (MemoryWriteEvidence, bool) {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower != "remember" && lower != "memory" {
		return MemoryWriteEvidence{}, false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(args), &payload); err != nil {
		return MemoryWriteEvidence{}, false
	}
	if lower == "memory" {
		action, _ := payload["action"].(string)
		if !strings.EqualFold(strings.TrimSpace(action), "remember") {
			return MemoryWriteEvidence{}, false
		}
	}
	content, _ := payload["content"].(string)
	if strings.TrimSpace(content) == "" {
		return MemoryWriteEvidence{}, false
	}
	ok := loopErr == nil && strings.TrimSpace(result) != "" && !looksLikeToolError(result)
	return MemoryWriteEvidence{Tool: name, Content: content, Result: truncate(result, 4_000), OK: ok}, true
}

func looksLikeToolError(result string) bool {
	lower := strings.ToLower(strings.TrimSpace(result))
	return strings.HasPrefix(lower, "error") || strings.HasPrefix(lower, "failed") || strings.Contains(lower, "not configured")
}

// scanMemoryVault snapshots the isolated markdown vault. Availability is true
// even when empty: "no write" is then a real failure, not missing evidence.
func scanMemoryVault(home string) (string, bool) {
	root := filepath.Join(home, "memory")
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", false
	}
	var b strings.Builder
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) > 512*1024 {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		fmt.Fprintf(&b, "\n<!-- %s -->\n", rel)
		b.Write(data)
		if b.Len() > 400_000 {
			return filepath.SkipAll
		}
		return nil
	})
	return b.String(), true
}

// SafeID keeps case ids usable as directory names.
func SafeID(raw string) string {
	raw = strings.TrimSpace(raw)
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "case"
	}
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}