package eval

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// EvaluateChecks runs configured validators in a stable order. It returns the
// per-check decisions plus the aggregate turn verdict. Unconfigured validators
// are skip; missing evidence is unknown; neither is ever pass.
func EvaluateChecks(e *Evaluation, ev Evidence) ([]CheckResult, Verdict, string) {
	if !e.Configured() {
		return nil, VerdictSkip, "no validators configured"
	}
	var checks []CheckResult
	if e.Keyword != nil {
		checks = append(checks, checkKeyword(e.Keyword, ev))
	}
	if e.Regex != nil {
		checks = append(checks, checkRegex(e.Regex, ev))
	}
	if e.Trace != nil {
		checks = append(checks, checkTrace(e.Trace, ev))
	}
	if e.MemoryWrite != nil {
		checks = append(checks, checkMemoryWrite(e.MemoryWrite, ev))
	}
	status, reason := aggregateChecks(checks)
	return checks, status, reason
}

func checkKeyword(c *KeywordCheck, ev Evidence) CheckResult {
	out := CheckResult{Validator: "keyword"}
	if !ev.ResponseAvailable {
		out.Status, out.Reason = "unknown", "no response evidence"
		return out
	}
	text := strings.ToLower(ev.Response)
	var missing []string
	for _, term := range c.All {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		if !strings.Contains(text, strings.ToLower(term)) {
			missing = append(missing, term)
		}
	}
	if len(missing) > 0 {
		out.Status = "fail"
		out.Reason = "missing keywords: " + strings.Join(missing, ", ")
		return out
	}
	if len(c.Any) > 0 {
		found := false
		for _, term := range c.Any {
			term = strings.TrimSpace(term)
			if term != "" && strings.Contains(text, strings.ToLower(term)) {
				found = true
				break
			}
		}
		if !found {
			out.Status = "fail"
			out.Reason = "none of the any-keywords appeared: " + strings.Join(c.Any, ", ")
			return out
		}
	}
	out.Status = "pass"
	return out
}

func checkRegex(c *RegexCheck, ev Evidence) CheckResult {
	out := CheckResult{Validator: "regex"}
	if !ev.ResponseAvailable {
		out.Status, out.Reason = "unknown", "no response evidence"
		return out
	}
	for _, pattern := range c.Patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			out.Status = "unknown"
			out.Reason = fmt.Sprintf("invalid regex %q: %v", pattern, err)
			return out
		}
		if !re.MatchString(ev.Response) {
			out.Status = "fail"
			out.Reason = fmt.Sprintf("response does not match %q", pattern)
			return out
		}
	}
	out.Status = "pass"
	return out
}

func checkTrace(c *TraceCheck, ev Evidence) CheckResult {
	out := CheckResult{Validator: "trace"}
	if !ev.TraceAvailable {
		out.Status, out.Reason = "unknown", "tool trace unavailable"
		return out
	}
	called := map[string]bool{}
	var names []string
	for _, tool := range ev.Tools {
		name := strings.ToLower(strings.TrimSpace(tool.Name))
		if name == "" {
			continue
		}
		called[name] = true
		names = append(names, tool.Name)
	}
	var missing []string
	for _, want := range c.Required {
		want = strings.TrimSpace(want)
		if want != "" && !called[strings.ToLower(want)] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		sort.Strings(names)
		out.Status = "fail"
		out.Reason = fmt.Sprintf("required tools not called: %s (called: %s)", strings.Join(missing, ", "), strings.Join(names, ", "))
		return out
	}
	var forbidden []string
	for _, deny := range c.Forbidden {
		deny = strings.TrimSpace(deny)
		if deny != "" && called[strings.ToLower(deny)] {
			forbidden = append(forbidden, deny)
		}
	}
	if len(forbidden) > 0 {
		out.Status = "fail"
		out.Reason = "forbidden tools called: " + strings.Join(forbidden, ", ")
		return out
	}
	out.Status = "pass"
	return out
}

func checkMemoryWrite(c *MemoryWriteCheck, ev Evidence) CheckResult {
	out := CheckResult{Validator: "memory_write"}
	evidenceAvailable := ev.TraceAvailable || ev.MemoryDumpAvailable
	if !evidenceAvailable && len(ev.MemoryWrites) == 0 {
		out.Status, out.Reason = "unknown", "no memory evidence (trace and vault snapshot unavailable)"
		return out
	}

	corpus := memoryCorpus(ev)
	observed := len(ev.MemoryWrites) > 0
	if !observed && ev.MemoryDumpAvailable && len(c.Expected) > 0 {
		observed = containsAll(corpus, c.Expected)
	}
	if !observed {
		if !ev.TraceAvailable && len(c.Expected) == 0 {
			out.Status, out.Reason = "unknown", "memory trace unavailable and no expected value to check"
			return out
		}
		if c.MustWrite {
			out.Status = "fail"
			out.Reason = "no memory write observed"
			return out
		}
		out.Status, out.Reason = "skip", "no memory write to inspect"
		return out
	}

	var missing []string
	for _, want := range c.Expected {
		want = strings.TrimSpace(want)
		if want != "" && !containsText(corpus, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		out.Status = "fail"
		out.Reason = "written memory does not contain corrected value(s): " + strings.Join(missing, ", ")
		return out
	}
	var stale []string
	writeCorpus := ""
	for _, w := range ev.MemoryWrites {
		writeCorpus += w.Content + "\n" + w.Result + "\n"
	}
	for _, deny := range c.Forbidden {
		deny = strings.TrimSpace(deny)
		if deny != "" && containsText(writeCorpus, deny) {
			stale = append(stale, deny)
		}
	}
	if len(stale) > 0 {
		out.Status = "fail"
		out.Reason = "written memory still contains stale value(s): " + strings.Join(stale, ", ")
		return out
	}
	out.Status = "pass"
	return out
}

func memoryCorpus(ev Evidence) string {
	var b strings.Builder
	for _, w := range ev.MemoryWrites {
		b.WriteString(w.Content)
		b.WriteByte('\n')
		b.WriteString(w.Result)
		b.WriteByte('\n')
	}
	if ev.MemoryDumpAvailable {
		b.WriteString(ev.MemoryDump)
	}
	return b.String()
}

func containsText(corpus, want string) bool {
	return strings.Contains(strings.ToLower(corpus), strings.ToLower(want))
}

func containsAll(corpus string, wants []string) bool {
	for _, want := range wants {
		want = strings.TrimSpace(want)
		if want != "" && !containsText(corpus, want) {
			return false
		}
	}
	return len(wants) > 0
}

func aggregateChecks(checks []CheckResult) (Verdict, string) {
	if len(checks) == 0 {
		return VerdictSkip, "no validators configured"
	}
	var reasons []string
	seenSkip, seenUnknown := false, false
	for _, check := range checks {
		switch check.Status {
		case "fail":
			reasons = append(reasons, check.Validator+": "+check.Reason)
		case "unknown":
			seenUnknown = true
			reasons = append(reasons, check.Validator+": "+check.Reason)
		case "skip":
			seenSkip = true
			reasons = append(reasons, check.Validator+": "+check.Reason)
		}
	}
	switch {
	case len(reasons) > 0 && !seenUnknown && !seenSkip:
		return VerdictFail, strings.Join(reasons, "; ")
	case seenUnknown:
		return VerdictUnknown, strings.Join(reasons, "; ")
	case seenSkip:
		return VerdictSkip, strings.Join(reasons, "; ")
	default:
		return VerdictPass, ""
	}
}