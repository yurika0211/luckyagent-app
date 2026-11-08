package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ScriptedTurn is a deterministic turn outcome for tests and offline replays.
type ScriptedTurn struct {
	Result TurnResult
	Err    error
}

// ScriptedExecutor replays prepared evidence without an LLM. It is used by
// tests and by `la-eval start --executor scripted` for pipeline verification.
type ScriptedExecutor struct {
	Turns map[string]ScriptedTurn // key: caseID#turnIndex (0-based)
	// Requests records what the runner actually sent, so tests can assert
	// addendum and injection behavior.
	Requests []TurnRequest
}

func scriptKey(caseID string, turn int) string {
	return fmt.Sprintf("%s#%d", caseID, turn)
}

func (s *ScriptedExecutor) Execute(ctx context.Context, req TurnRequest) (TurnResult, error) {
	if err := ctx.Err(); err != nil {
		return TurnResult{}, err
	}
	s.Requests = append(s.Requests, req)
	turn, ok := s.Turns[scriptKey(req.Case.ID, req.TurnIndex)]
	if !ok {
		return TurnResult{}, fmt.Errorf("scripted executor: no turn %s", scriptKey(req.Case.ID, req.TurnIndex))
	}
	if turn.Err != nil {
		return TurnResult{}, turn.Err
	}
	return turn.Result, nil
}

// TextResult builds a completed response with trace evidence. TraceAvailable is
// true so trace validators can distinguish "not called" from "not observed".
func TextResult(response string, tools ...ToolEvidence) TurnResult {
	return TurnResult{
		Response:          response,
		ResponseAvailable: strings.TrimSpace(response) != "",
		Tools:             tools,
		TraceAvailable:    true,
	}
}

// WithMemory attaches write evidence to a scripted result.
func WithMemory(result TurnResult, writes ...MemoryWriteEvidence) TurnResult {
	result.MemoryWrites = append(result.MemoryWrites, writes...)
	return result
}

type scriptedFile struct {
	Turns map[string]scriptedTurnFile `json:"turns"`
}

type scriptedTurnFile struct {
	Response     string                `json:"response"`
	Tools        []ToolEvidence        `json:"tools"`
	MemoryWrites []MemoryWriteEvidence `json:"memory_writes"`
	Error        string                `json:"error"`
}

// LoadScripted reads a deterministic turn file for pipeline verification.
// It does not call a model. Trace evidence is marked available so a missing
// tool is a fail, not an unknown.
func LoadScripted(path string) (*ScriptedExecutor, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scripted turns: %w", err)
	}
	var file scriptedFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode scripted turns: %w", err)
	}
	if len(file.Turns) == 0 {
		return nil, fmt.Errorf("scripted turns file has no turns")
	}
	exec := &ScriptedExecutor{Turns: make(map[string]ScriptedTurn, len(file.Turns))}
	for key, turn := range file.Turns {
		item := ScriptedTurn{Result: TextResult(turn.Response, turn.Tools...)}
		item.Result.MemoryWrites = turn.MemoryWrites
		if strings.TrimSpace(turn.Error) != "" {
			item.Err = fmt.Errorf("%s", turn.Error)
		}
		exec.Turns[key] = item
	}
	return exec, nil
}