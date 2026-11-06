package evalplatform

import (
	"regexp"
	"strings"
)

const (
	ExecutionCompleted = "completed"
	ExecutionFailed    = "failed"
	ExecutionUnknown   = "unknown"

	VerdictPass    = "pass"
	VerdictFail    = "fail"
	VerdictSkip    = "skip"
	VerdictUnknown = "unknown"
)

// Judge scores one turn. Missing evaluation is skip. Missing evidence is unknown.
// A configured check that fails is fail. Nothing here calls a model.
func Judge(eval Eval, evidence Evidence, execution string) string {
	if !eval.configured() {
		return VerdictSkip
	}
	if execution == ExecutionUnknown || execution == "" {
		return VerdictUnknown
	}
	if execution == ExecutionFailed {
		return VerdictFail
	}
	if eval.Trace != nil && !evidence.ToolsSet {
		return VerdictUnknown
	}
	if eval.MemoryWrite != nil && !evidence.MemorySet {
		return VerdictUnknown
	}
	if eval.Keyword != nil {
		for _, want := range eval.Keyword.Contains {
			if want == "" {
				continue
			}
			if !strings.Contains(evidence.Assistant, want) {
				return VerdictFail
			}
		}
	}
	if eval.Regex != nil {
		for _, pattern := range eval.Regex.Patterns {
			re, err := regexp.Compile(pattern)
			if err != nil || !re.MatchString(evidence.Assistant) {
				return VerdictFail
			}
		}
	}
	if eval.Trace != nil {
		have := map[string]bool{}
		for _, name := range evidence.Tools {
			have[name] = true
		}
		for _, name := range eval.Trace.Required {
			if !have[name] {
				return VerdictFail
			}
		}
		for _, name := range eval.Trace.Forbidden {
			if have[name] {
				return VerdictFail
			}
		}
	}
	if eval.MemoryWrite != nil {
		found := false
		for _, write := range evidence.MemoryWrites {
			if strings.Contains(write.Content, eval.MemoryWrite.Content) &&
				(eval.MemoryWrite.Category == "" || write.Category == eval.MemoryWrite.Category) {
				found = true
				break
			}
		}
		if !found {
			return VerdictFail
		}
	}
	return VerdictPass
}

// SampleExecutor is the in-process transcript for the checked-in sample case.
// It does not call a provider.
type SampleExecutor struct{}

// Execute returns the sample transcript, or unknown evidence for any other id.
func (SampleExecutor) Execute(c Case) (string, []Evidence, error) {
	steps := c.Steps()
	if c.ID != "chat-tool-memory" {
		out := make([]Evidence, len(steps))
		return ExecutionUnknown, out, nil
	}
	return ExecutionCompleted, []Evidence{
		{
			Assistant: "I read the note and saved dark mode.",
			Tools:     []string{"file_read", "remember"},
			ToolsSet:  true,
			MemoryWrites: []MemoryWrite{{
				Content:  "user prefers dark mode",
				Category: "preference",
			}},
			MemorySet: true,
		},
		{
			Assistant:    "I saved dark mode as a preference.",
			Tools:        []string{},
			ToolsSet:     true,
			MemoryWrites: []MemoryWrite{},
			MemorySet:    true,
		},
	}, nil
}
