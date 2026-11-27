package main

import (
	"testing"

	"github.com/yurika0211/luckyagent-tools/memory"
)

func TestExpandScenarios(t *testing.T) {
	got, err := expandScenarios("all")
	if err != nil {
		t.Fatalf("expandScenarios: %v", err)
	}
	want := []string{"lexical", "graph", "temporal", "scale", "route", "tidal"}
	if len(got) != len(want) {
		t.Fatalf("expected %d scenarios, got %#v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scenario %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEvaluateEntriesComputesQualityMetrics(t *testing.T) {
	query := benchQuery{
		WantIDs:   []string{"a", "b"},
		ForbidIDs: []string{"stale"},
	}
	entries := []memory.Entry{
		{ID: "noise"},
		{ID: "a"},
		{ID: "stale"},
	}

	got := evaluateEntries(query, entries, 3)
	if got.ResultCount != 3 {
		t.Fatalf("ResultCount = %d", got.ResultCount)
	}
	if got.HitCount != 1 {
		t.Fatalf("HitCount = %d", got.HitCount)
	}
	if got.ForbidHitCount != 1 {
		t.Fatalf("ForbidHitCount = %d", got.ForbidHitCount)
	}
	if got.RecallAtK != 0.5 {
		t.Fatalf("RecallAtK = %v", got.RecallAtK)
	}
	if got.PrecisionAtK != float64(1)/float64(3) {
		t.Fatalf("PrecisionAtK = %v", got.PrecisionAtK)
	}
	if got.MRR != 0.5 {
		t.Fatalf("MRR = %v", got.MRR)
	}
}

func TestSummarizeRecordsDetectsGraphLift(t *testing.T) {
	cfg := benchConfig{
		Variant:      "test",
		Dataset:      "synthetic",
		Size:         100,
		Rounds:       1,
		MinRecall:    0.5,
		MaxNoise:     1.0,
		MaxStaleHits: 0,
	}
	records := []benchRecord{
		{
			Mode:          "graph_off",
			DurationNS:    100,
			ExpectedCount: 2,
			RecallAtK:     0.5,
			NoiseAtK:      0.5,
			Clean:         true,
			QualityPass:   true,
		},
		{
			Mode:          "graph_on",
			DurationNS:    200,
			ExpectedCount: 2,
			RecallAtK:     1.0,
			NoiseAtK:      0.25,
			Clean:         true,
			QualityPass:   true,
		},
	}

	got := summarizeRecords(cfg, "graph", records)
	if !got.Clean || !got.QualityPass {
		t.Fatalf("expected clean quality summary: %#v", got)
	}
	if got.GraphRecallLift != 0.5 {
		t.Fatalf("GraphRecallLift = %v", got.GraphRecallLift)
	}
	if got.AvgGraphOnDurationNS != 200 || got.AvgGraphOffDurationNS != 100 {
		t.Fatalf("unexpected graph durations: %#v", got)
	}
}

func TestSyntheticDatasetLoadsMemoryStore(t *testing.T) {
	cfg := benchConfig{
		Dataset:     "synthetic",
		Size:        32,
		Seed:        42,
		KeepDataset: false,
	}
	ds, cleanup, err := loadSyntheticDataset(cfg)
	if err != nil {
		t.Fatalf("loadSyntheticDataset: %v", err)
	}
	defer cleanup()

	if ds.Store == nil {
		t.Fatalf("expected store")
	}
	if ds.Size < 32 {
		t.Fatalf("expected at least 32 notes, got %d", ds.Size)
	}
	if len(ds.QueriesForScenario("graph", "")) == 0 {
		t.Fatalf("expected graph queries")
	}
	if len(ds.QueriesForScenario("tidal", "")) == 0 {
		t.Fatalf("expected tidal queries")
	}
	results := ds.Store.Activate("telegram channel", memory.ActivationOptions{Limit: 3, IncludeGraph: true})
	if len(results) == 0 {
		t.Fatalf("expected activation results")
	}
}

func TestTidalScenarioReportsMRRLift(t *testing.T) {
	cfg := benchConfig{
		Variant:      "test",
		Dataset:      "synthetic",
		Size:         100,
		Rounds:       1,
		MinRecall:    0.5,
		MaxNoise:     1.0,
		MaxStaleHits: 0,
	}
	records := []benchRecord{
		{
			Mode:          "tidal_off",
			DurationNS:    100,
			ExpectedCount: 1,
			RecallAtK:     1.0,
			MRR:           0.5,
			NoiseAtK:      0.5,
			StaleHitRate:  0.25,
			Clean:         true,
			QualityPass:   true,
		},
		{
			Mode:          "tidal_on",
			DurationNS:    200,
			ExpectedCount: 1,
			RecallAtK:     1.0,
			MRR:           1.0,
			NoiseAtK:      0.5,
			StaleHitRate:  0.10,
			Clean:         true,
			QualityPass:   true,
		},
	}

	got := summarizeRecords(cfg, "tidal", records)
	if got.TidalMRRLift != 0.5 {
		t.Fatalf("TidalMRRLift = %v", got.TidalMRRLift)
	}
	if got.AvgTidalOnDurationNS != 200 || got.AvgTidalOffDurationNS != 100 {
		t.Fatalf("unexpected tidal durations: %#v", got)
	}
	if got.TidalStaleRateDelta >= 0 {
		t.Fatalf("expected tidal stale rate not to increase: %#v", got)
	}
}

// The tidal control arm keeps stale scoring enabled on purpose, so a stale hit
// there is the baseline for the tidal_on arm to beat, not a regression. Only
// that one arm is excused; every other tidal record is held to the clean bar.
func TestAllSummaryExcusesOnlyTidalControlArm(t *testing.T) {
	cfg := benchConfig{
		Variant:      "test",
		Dataset:      "synthetic",
		Size:         100,
		Rounds:       1,
		MinRecall:    0.5,
		MaxNoise:     0.5,
		MaxStaleHits: 0,
	}
	records := []benchRecord{
		{
			Scenario:      "graph",
			Mode:          "graph_on",
			DurationNS:    100,
			ExpectedCount: 1,
			RecallAtK:     1.0,
			NoiseAtK:      0.4,
			Clean:         true,
		},
		{
			Scenario:      "tidal",
			Mode:          "tidal_off",
			DurationNS:    100,
			ExpectedCount: 1,
			RecallAtK:     1.0,
			MRR:           0.5,
			NoiseAtK:      0.8,
			StaleHitRate:  0.25,
			Clean:         false,
		},
		{
			Scenario:      "tidal",
			Mode:          "tidal_on",
			DurationNS:    100,
			ExpectedCount: 1,
			RecallAtK:     1.0,
			MRR:           1.0,
			NoiseAtK:      0.8,
			StaleHitRate:  0.0,
			Clean:         true,
		},
	}

	got := summarizeRecords(cfg, "all", records)
	if !got.Clean || !got.QualityPass {
		t.Fatalf("control-arm stale hit must not fail the run: %#v", got)
	}
	if got.TidalMRRLift <= 0 || got.TidalStaleRateDelta >= 0 {
		t.Fatalf("expected tidal comparison metrics, got %#v", got)
	}
}

// A stale hit on the tidal_on arm means suppression did not work. That has to
// fail the run; excluding the whole tidal scenario is what hid this before.
func TestAllSummaryFailsWhenTidalOnArmIsDirty(t *testing.T) {
	cfg := benchConfig{
		Variant:      "test",
		Dataset:      "synthetic",
		Size:         100,
		Rounds:       1,
		MinRecall:    0.5,
		MaxNoise:     0.5,
		MaxStaleHits: 0,
	}
	records := []benchRecord{
		{
			Scenario:      "graph",
			Mode:          "graph_on",
			DurationNS:    100,
			ExpectedCount: 1,
			RecallAtK:     1.0,
			NoiseAtK:      0.4,
			Clean:         true,
		},
		{
			Scenario:       "tidal",
			Mode:           "tidal_on",
			DurationNS:     100,
			ExpectedCount:  1,
			RecallAtK:      1.0,
			MRR:            1.0,
			NoiseAtK:       0.8,
			StaleHitRate:   0.25,
			ForbidHitCount: 1,
			Clean:          false,
		},
	}

	got := summarizeRecords(cfg, "all", records)
	if got.Clean {
		t.Fatalf("a stale hit on the tidal_on arm must not be clean: %#v", got)
	}
	if got.QualityPass {
		t.Fatalf("a stale hit on the tidal_on arm must fail quality: %#v", got)
	}
}

// A query with no expected ids cannot assert recall. It used to report 1.00
// recall and 0.00 noise, which made an unmeasured query look flawless.
func TestEvaluateEntriesWithoutExpectationsIsNotPerfect(t *testing.T) {
	query := benchQuery{ForbidIDs: []string{"stale"}}
	entries := []memory.Entry{{ID: "whatever"}, {ID: "another"}}

	got := evaluateEntries(query, entries, 6)
	if !got.NoExpectation {
		t.Fatalf("expected NoExpectation to be set: %#v", got)
	}
	if got.RecallAtK != 0 || got.MRR != 0 || got.NDCGAtK != 0 {
		t.Fatalf("want-less query must not score as perfect: %#v", got)
	}
}

// The scale scenario measures latency against a fixed anchor. Without want ids
// it asserted nothing, so recall was hardcoded and noise was pinned at 1.00.
func TestScaleAnchorQueryCarriesGroundTruth(t *testing.T) {
	var anchor *benchQuery
	for _, query := range syntheticQueries() {
		if query.ID == "scale_anchor" {
			q := query
			anchor = &q
			break
		}
	}
	if anchor == nil {
		t.Fatalf("scale_anchor query missing")
	}
	if len(anchor.WantIDs) != 1 || anchor.WantIDs[0] != "mem_bench_scale_anchor" {
		t.Fatalf("scale_anchor WantIDs = %#v", anchor.WantIDs)
	}
	if anchor.Scenario != "scale" {
		t.Fatalf("scale_anchor scenario = %q", anchor.Scenario)
	}
}

// noise@k floors at (limit-want)/limit. With one expected memory and a limit of
// six the floor is 0.83, so a 0.60 threshold is unreachable and measures the
// limit instead of the retrieval. The gate must report itself inapplicable
// rather than turning every run red for nothing.
func TestNoiseGateSkippedWhenFewerExpectationsThanLimit(t *testing.T) {
	cfg := benchConfig{MinRecall: 0.65, MaxNoise: 0.6, MaxStaleHits: 0, Limit: 6, Rounds: 1}
	records := []benchRecord{{
		Mode:          "scale",
		DurationNS:    10,
		ExpectedCount: 1,
		RecallAtK:     1.0,
		MRR:           1.0,
		NoiseAtK:      0.83,
		Clean:         true,
		QualityPass:   true,
	}}

	got := summarizeRecords(cfg, "scale", records)
	if got.NoiseGated {
		t.Fatalf("noise gate must be reported inapplicable: %#v", got)
	}
	if !got.QualityPass {
		t.Fatalf("recall was fine, run must pass: %#v", got)
	}
}

// Once the expectations fill the limit the floor disappears and the noise
// threshold becomes a real constraint again.
func TestNoiseGateAppliesWhenExpectationsFillLimit(t *testing.T) {
	cfg := benchConfig{MinRecall: 0.65, MaxNoise: 0.6, MaxStaleHits: 0, Limit: 6, Rounds: 1}
	records := []benchRecord{{
		Mode:          "temporal",
		DurationNS:    10,
		ExpectedCount: 6,
		RecallAtK:     1.0,
		MRR:           1.0,
		NoiseAtK:      0.83,
		Clean:         true,
		QualityPass:   true,
	}}

	got := summarizeRecords(cfg, "temporal", records)
	if !got.NoiseGated {
		t.Fatalf("noise gate must apply when expectations fill the limit: %#v", got)
	}
	if got.QualityPass {
		t.Fatalf("0.83 noise must fail a 0.60 threshold: %#v", got)
	}
}

// A forbidden memory in the results is not clean regardless of averages.
func TestRecordCleanFailsOnForbiddenHit(t *testing.T) {
	record := benchRecord{ForbidHitCount: 1}
	markRecordQuality(&record, benchConfig{MinRecall: 0.65, MaxNoise: 0.6, MaxStaleHits: 0})
	if record.Clean {
		t.Fatalf("forbidden hit must not be clean: %#v", record)
	}
	if record.QualityPass {
		t.Fatalf("forbidden hit must not pass quality: %#v", record)
	}
}
