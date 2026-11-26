package websocket

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// chartWireAction is the versioned part of a ui_action. The published API
// type does not have these fields yet, so they are read from the raw message
// and ignored when absent. That keeps older clients working.
type chartWireAction struct {
	ActionID      string `json:"action_id"`
	ChartID       string `json:"chart_id"`
	SchemaVersion int    `json:"schema_version"`
	DataVersion   int64  `json:"data_version"`
	Operation     string `json:"operation"`
}

func chartWireFromMessage(msg *Message) (chartWireAction, error) {
	if msg == nil || len(msg.Data) == 0 {
		return chartWireAction{}, nil
	}
	var wire chartWireAction
	if err := json.Unmarshal(msg.Data, &wire); err != nil {
		return chartWireAction{}, err
	}
	wire.ActionID = strings.TrimSpace(wire.ActionID)
	wire.ChartID = strings.TrimSpace(wire.ChartID)
	wire.Operation = strings.ToLower(strings.TrimSpace(wire.Operation))
	return wire, nil
}

// chartLifecycle is the latest applied version of one chart in one session.
// The same action id returns the stored result. An older data_version is
// refused. This is the app-side check from #509; it does not replace the
// session log.
type chartLifecycle struct {
	mu      sync.Mutex
	current map[string]chartVersion
	applied map[string]chartApplyResult
}

type chartVersion struct {
	ChartID       string
	SchemaVersion int
	DataVersion   int64
	Mode          string
}

type chartApplyResult struct {
	ActionID    string
	DataVersion int64
	ChildTurn   string
}

func newChartLifecycle() *chartLifecycle {
	return &chartLifecycle{
		current: map[string]chartVersion{},
		applied: map[string]chartApplyResult{},
	}
}

func chartKey(sessionID, chartID string) string {
	return strings.TrimSpace(sessionID) + "\x00" + strings.TrimSpace(chartID)
}

// rememberChart records the version a server delta declared. A later delta
// for the same chart replaces it. Legacy charts (no mode) are not tracked.
func (c *chartLifecycle) rememberChart(sessionID string, version chartVersion) {
	if c == nil || version.ChartID == "" || version.Mode == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current[chartKey(sessionID, version.ChartID)] = version
}

// acceptApply decides whether this action may start a child turn.
// A repeated action id is idempotent and does not start another turn.
func (c *chartLifecycle) acceptApply(sessionID string, wire chartWireAction) (chartApplyResult, bool, error) {
	if c == nil || wire.ChartID == "" {
		return chartApplyResult{}, false, nil
	}
	if wire.SchemaVersion < 1 || wire.DataVersion < 1 {
		return chartApplyResult{}, false, fmt.Errorf("chart %q requires schema_version and data_version", wire.ChartID)
	}
	switch wire.Operation {
	case "apply", "observe":
	default:
		return chartApplyResult{}, false, fmt.Errorf("chart %q has invalid operation", wire.ChartID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := chartKey(sessionID, wire.ChartID)
	current, ok := c.current[key]
	if !ok {
		return chartApplyResult{}, false, fmt.Errorf("chart %q is not in this session", wire.ChartID)
	}
	if wire.SchemaVersion != current.SchemaVersion {
		return chartApplyResult{}, false, fmt.Errorf("chart %q schema_version %d is not %d", wire.ChartID, wire.SchemaVersion, current.SchemaVersion)
	}
	if wire.ActionID != "" {
		if previous, seen := c.applied[key+"\x00"+wire.ActionID]; seen {
			return previous, true, nil
		}
	}
	if wire.DataVersion != current.DataVersion {
		return chartApplyResult{}, false, fmt.Errorf("chart %q data_version %d is stale; current is %d", wire.ChartID, wire.DataVersion, current.DataVersion)
	}
	if current.Mode == "data" && wire.Operation != "observe" {
		return chartApplyResult{}, false, fmt.Errorf("chart %q is a data chart and cannot be edited", wire.ChartID)
	}
	return chartApplyResult{ActionID: wire.ActionID, DataVersion: current.DataVersion}, false, nil
}

// commitApply stores the child turn for an accepted action and advances the
// data version by one. A second commit of the same action id is a no-op.
func (c *chartLifecycle) commitApply(sessionID, childTurn string, wire chartWireAction) chartVersion {
	if c == nil || wire.ChartID == "" {
		return chartVersion{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := chartKey(sessionID, wire.ChartID)
	current := c.current[key]
	if wire.ActionID != "" {
		applyKey := key + "\x00" + wire.ActionID
		if previous, seen := c.applied[applyKey]; seen {
			current.DataVersion = previous.DataVersion
			return current
		}
		c.applied[applyKey] = chartApplyResult{ActionID: wire.ActionID, DataVersion: current.DataVersion + 1, ChildTurn: childTurn}
	}
	current.DataVersion++
	c.current[key] = current
	return current
}
