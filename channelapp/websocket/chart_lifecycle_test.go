package websocket

import "testing"

func TestChartApplyRejectsStaleVersionAndRepeatsOnce(t *testing.T) {
	life := newChartLifecycle()
	life.rememberChart("s1", chartVersion{ChartID: "c1", SchemaVersion: 2, DataVersion: 4, Mode: "teaching"})

	wire := chartWireAction{ActionID: "a1", ChartID: "c1", SchemaVersion: 2, DataVersion: 4, Operation: "apply"}
	first, replay, err := life.acceptApply("s1", wire)
	if err != nil || replay || first.ActionID != "a1" {
		t.Fatalf("first=%#v replay=%v err=%v", first, replay, err)
	}
	life.commitApply("s1", "turn-1", wire)

	again, replay, err := life.acceptApply("s1", wire)
	if err != nil || !replay || again.ChildTurn != "turn-1" || again.DataVersion != 5 {
		t.Fatalf("repeat=%#v replay=%v err=%v", again, replay, err)
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
