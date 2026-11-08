package eval

import "testing"

func TestValidatorsMissingEvidenceIsNotPass(t *testing.T) {
	checks, verdict, reason := EvaluateChecks(&Evaluation{
		Keyword:     &KeywordCheck{All: []string{"在听"}},
		Regex:       &RegexCheck{Patterns: []string{"周三"}},
		Trace:       &TraceCheck{Required: []string{"remember"}},
		MemoryWrite: &MemoryWriteCheck{MustWrite: true, Expected: []string{"周三 10:00"}},
	}, Evidence{})
	if verdict == VerdictPass {
		t.Fatalf("missing evidence counted as pass: %+v", checks)
	}
	if verdict != VerdictUnknown {
		t.Fatalf("verdict = %s (%s), want unknown", verdict, reason)
	}
	for _, check := range checks {
		if check.Status == "pass" {
			t.Fatalf("validator %s passed without evidence", check.Validator)
		}
	}
}

func TestUnconfiguredEvaluationIsSkip(t *testing.T) {
	_, verdict, _ := EvaluateChecks(nil, Evidence{ResponseAvailable: true, Response: "好听的回复", TraceAvailable: true})
	if verdict != VerdictSkip {
		t.Fatalf("unconfigured verdict = %s, want skip", verdict)
	}
	_, verdict, _ = EvaluateChecks(&Evaluation{}, Evidence{ResponseAvailable: true, Response: "still nice"})
	if verdict != VerdictSkip {
		t.Fatalf("empty evaluation verdict = %s, want skip", verdict)
	}
}

func TestKeywordRegexTraceMemoryPassAndFail(t *testing.T) {
	ev := Evidence{
		Response:          "已记下，晨会改到周三 10:00，三楼会议室。",
		ResponseAvailable: true,
		TraceAvailable:    true,
		Tools:             []ToolEvidence{{Name: "remember", Arguments: `{"content":"晨会周三 10:00 三楼会议室"}`}},
		MemoryWrites: []MemoryWriteEvidence{{
			Tool:    "remember",
			Content: "晨会改到周三 10:00，地点仍是三楼会议室",
			OK:      true,
		}},
	}
	_, verdict, reason := EvaluateChecks(&Evaluation{
		Keyword: &KeywordCheck{All: []string{"周三", "10:00"}, Any: []string{"记下", "已记"}},
		Regex:   &RegexCheck{Patterns: []string{`周三\s*10:00`}},
		Trace:   &TraceCheck{Required: []string{"remember"}, Forbidden: []string{"web_search"}},
		MemoryWrite: &MemoryWriteCheck{
			MustWrite: true,
			Expected:  []string{"周三 10:00", "三楼会议室"},
			Forbidden: []string{"周二 09:30"},
		},
	}, ev)
	if verdict != VerdictPass {
		t.Fatalf("verdict = %s (%s)", verdict, reason)
	}

	ev.Tools = append(ev.Tools, ToolEvidence{Name: "web_search"})
	_, verdict, reason = EvaluateChecks(&Evaluation{Trace: &TraceCheck{Forbidden: []string{"web_search"}}}, ev)
	if verdict != VerdictFail || !contains(reason, "forbidden") {
		t.Fatalf("forbidden tool verdict = %s (%s)", verdict, reason)
	}

	ev.Tools = []ToolEvidence{{Name: "recall"}}
	ev.MemoryWrites = nil
	_, verdict, _ = EvaluateChecks(&Evaluation{Trace: &TraceCheck{Required: []string{"remember"}}}, ev)
	if verdict != VerdictFail {
		t.Fatalf("missing required tool verdict = %s", verdict)
	}
}

func TestMemoryWriteStaleValueFails(t *testing.T) {
	ev := Evidence{
		TraceAvailable: true,
		MemoryWrites: []MemoryWriteEvidence{{
			Tool:    "remember",
			Content: "晨会仍是周二 09:30",
			OK:      true,
		}},
	}
	_, verdict, reason := EvaluateChecks(&Evaluation{MemoryWrite: &MemoryWriteCheck{
		MustWrite: true,
		Expected:  []string{"周三 10:00"},
		Forbidden: []string{"周二 09:30"},
	}}, ev)
	if verdict != VerdictFail {
		t.Fatalf("stale memory verdict = %s (%s)", verdict, reason)
	}
}

func TestMemoryWriteWithoutEvidenceIsUnknown(t *testing.T) {
	_, verdict, _ := EvaluateChecks(&Evaluation{MemoryWrite: &MemoryWriteCheck{MustWrite: true}}, Evidence{})
	if verdict != VerdictUnknown {
		t.Fatalf("verdict = %s, want unknown", verdict)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || (len(s) > 0 && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()))
}
