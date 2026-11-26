package websocket

import (
	"path/filepath"
	"testing"
)

func TestChartApplyRejectsStaleVersionAndRepeatsOnce(t *testing.T) {
	life := newChartLifecycle()
	life.rememberChart("s1", chartVersion{ChartID: "c1", SchemaVersion: 2, DataVersion: 4, Mode: "teaching"})

	wire := chartWireAction{ActionID: "a1", ChartID: "c1", SchemaVersion: 2, DataVersion: 4, Operation: "apply"}
	first, replay, err := life.acceptApply("s1", wire)
	if err != nil || replay || first.ActionID != "a1" {
		t.Fatalf("first=%#v replay=%v err=%v", first, replay, err)
	}
	if _, err := life.commitApply("s1", "turn-1", wire); err != nil {
		t.Fatal(err)
	}

	again, replay, err := life.acceptApply("s1", wire)
	if err != nil || !replay || again.ChildTurn != "turn-1" || again.DataVersion != 4 {
		t.Fatalf("repeat=%#v replay=%v err=%v", again, replay, err)
	}
	life.rememberChart("s1", chartVersion{ChartID: "c1", SchemaVersion: 2, DataVersion: 5, Mode: "teaching"})
	again, replay, err = life.acceptApply("s1", wire)
	if err != nil || !replay || again.DataVersion != 5 {
		t.Fatalf("committed repeat=%#v replay=%v err=%v", again, replay, err)
	}

	stale := wire
	stale.ActionID = "a2"
	if _, _, err := life.acceptApply("s1", stale); err == nil {
		t.Fatal("stale data_version was accepted")
	}
	data := chartWireAction{ActionID: "a3", ChartID: "c1", SchemaVersion: 2, DataVersion: 5, Operation: "apply"}
	life.rememberChart("s1", chartVersion{ChartID: "c1", SchemaVersion: 2, DataVersion: 5, Mode: "data"})
	if _, _, err := life.acceptApply("s1", data); err == nil {
		t.Fatal("data chart accepted an edit")
	}
	data.Operation = "observe"
	if _, replay, err := life.acceptApply("s1", data); err != nil || replay {
		t.Fatalf("observe err=%v replay=%v", err, replay)
	}
}

func TestChartWireIgnoresLegacyAction(t *testing.T) {
	msg := &Message{Data: []byte(`{"tree_id":"t","widget_id":"w","values":{"window":"4"}}`)}
	wire, err := chartWireFromMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	if wire.ChartID != "" || wire.DataVersion != 0 {
		t.Fatalf("legacy wire=%#v", wire)
	}
	life := newChartLifecycle()
	if _, replay, err := life.acceptApply("s", wire); err != nil || replay {
		t.Fatalf("legacy should pass through, replay=%v err=%v", replay, err)
	}
}

func TestChartLifecyclePersistsVersionsAndIdempotency(t *testing.T) {
	root := t.TempDir()
	life := newChartLifecycle(filepath.Join(root, "charts"))
	life.rememberChart("session", chartVersion{ChartID: "c", SchemaVersion: 1, DataVersion: 3, Mode: "teaching"})
	wire := chartWireAction{ActionID: "a", ChartID: "c", SchemaVersion: 1, DataVersion: 3, Operation: "apply"}
	if _, _, err := life.acceptApply("session", wire); err != nil {
		t.Fatal(err)
	}
	if _, err := life.commitApply("session", "turn", wire); err != nil {
		t.Fatal(err)
	}

	reloaded := newChartLifecycle(filepath.Join(root, "charts"))
	result, replay, err := reloaded.acceptApply("session", wire)
	if err != nil || !replay || result.ChildTurn != "turn" || result.DataVersion != 3 {
		t.Fatalf("replayed=%#v replay=%v err=%v", result, replay, err)
	}
	reloaded.rememberChart("session", chartVersion{ChartID: "c", SchemaVersion: 1, DataVersion: 4, Mode: "teaching"})
	result, replay, err = reloaded.acceptApply("session", wire)
	if err != nil || !replay || result.DataVersion != 4 {
		t.Fatalf("committed replay=%#v replay=%v err=%v", result, replay, err)
	}
	stale := wire
	stale.ActionID = "new"
	if _, _, err := reloaded.acceptApply("session", stale); err == nil {
		t.Fatal("stale action was accepted after reload")
	}
}
