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

func TestChartFailureKeepsCurrentVersionAndParams(t *testing.T) {
	root := t.TempDir()
	life := newChartLifecycle(filepath.Join(root, "charts"))
	if err := life.rememberChart("s", chartVersion{
		ChartID: "c", SchemaVersion: 1, DataVersion: 2, Mode: "teaching",
		Params: map[string]string{"x": "day"},
	}); err != nil {
		t.Fatal(err)
	}
	bad := chartWireAction{
		ActionID: "bad", ChartID: "c", SchemaVersion: 1, DataVersion: 2, Operation: "apply",
		Params: map[string]string{"x": "not-a-field"},
	}
	if _, err := life.failApply("s", "turn-bad", bad, "validation failed"); err != nil {
		t.Fatal(err)
	}
	again, replay, err := life.acceptApply("s", bad)
	if err != nil || !replay || !again.Failed || again.DataVersion != 2 || again.ChildTurn != "turn-bad" {
		t.Fatalf("failed retry=%#v replay=%v err=%v", again, replay, err)
	}
	snap, ok, err := life.snapshot("s", "c")
	if err != nil || !ok || snap.Current.DataVersion != 2 || snap.Current.Params["x"] != "day" {
		t.Fatalf("current after failure=%#v ok=%v err=%v", snap.Current, ok, err)
	}

	good := chartWireAction{
		ActionID: "good", ChartID: "c", SchemaVersion: 1, DataVersion: 2, Operation: "apply",
		Params: map[string]string{"x": "hour"},
	}
	if _, err := life.commitApply("s", "turn-good", good); err != nil {
		t.Fatal(err)
	}
	if err := life.rememberChart("s", chartVersion{
		ChartID: "c", SchemaVersion: 1, DataVersion: 3, Mode: "teaching",
		Params: map[string]string{"x": "hour"}, State: "complete",
	}); err != nil {
		t.Fatal(err)
	}
	reloaded := newChartLifecycle(filepath.Join(root, "charts"))
	snap, ok, err = reloaded.snapshot("s", "c")
	if err != nil || !ok || snap.Current.DataVersion != 3 || snap.Current.Mode != "teaching" || snap.Current.Params["x"] != "hour" {
		t.Fatalf("reloaded current=%#v ok=%v err=%v", snap.Current, ok, err)
	}
	var sawOld bool
	for _, version := range snap.History {
		if version.DataVersion == 2 && version.Params["x"] == "day" {
			sawOld = true
		}
	}
	if !sawOld {
		t.Fatalf("history lost the previous chart: %#v", snap.History)
	}
	old := good
	old.ActionID = "late"
	old.DataVersion = 2
	if _, _, err := reloaded.acceptApply("s", old); err == nil {
		t.Fatal("action on the previous data_version was accepted")
	}
	same, replay, err := reloaded.acceptApply("s", good)
	if err != nil || !replay || same.DataVersion != 3 || same.ChildTurn != "turn-good" {
		t.Fatalf("successful retry=%#v replay=%v err=%v", same, replay, err)
	}
}
