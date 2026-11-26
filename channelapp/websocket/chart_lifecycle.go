package websocket

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	root    string
	loaded  map[string]bool
	current map[string]chartVersion
	applied map[string]chartApplyResult
}

type persistedChartLifecycle struct {
	Version int                         `json:"version"`
	Current map[string]chartVersion     `json:"current,omitempty"`
	Applied map[string]chartApplyResult `json:"applied,omitempty"`
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

func newChartLifecycle(root ...string) *chartLifecycle {
	return &chartLifecycle{
		root:    strings.TrimSpace(firstString(root)),
		loaded:  map[string]bool{},
		current: map[string]chartVersion{},
		applied: map[string]chartApplyResult{},
	}
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (c *chartLifecycle) statePath(sessionID string) string {
	if c == nil || c.root == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(sessionID))
	return filepath.Join(c.root, fmt.Sprintf("%x.json", hash[:]))
}

func (c *chartLifecycle) loadLocked(sessionID string) error {
	if c.loaded[sessionID] {
		return nil
	}
	c.loaded[sessionID] = true
	path := c.statePath(sessionID)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read chart lifecycle: %w", err)
	}
	var state persistedChartLifecycle
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse chart lifecycle: %w", err)
	}
	prefix := sessionID + "\x00"
	for chartID, version := range state.Current {
		c.current[prefix+chartID] = version
	}
	for actionID, result := range state.Applied {
		c.applied[prefix+actionID] = result
	}
	return nil
}

func (c *chartLifecycle) saveLocked(sessionID string) error {
	path := c.statePath(sessionID)
	if path == "" {
		return nil
	}
	prefix := sessionID + "\x00"
	state := persistedChartLifecycle{Version: 1, Current: map[string]chartVersion{}, Applied: map[string]chartApplyResult{}}
	for key, version := range c.current {
		if strings.HasPrefix(key, prefix) {
			state.Current[strings.TrimPrefix(key, prefix)] = version
		}
	}
	for key, result := range c.applied {
		if strings.HasPrefix(key, prefix) {
			state.Applied[strings.TrimPrefix(key, prefix)] = result
		}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal chart lifecycle: %w", err)
	}
	if err := os.MkdirAll(c.root, 0o700); err != nil {
		return fmt.Errorf("create chart lifecycle dir: %w", err)
	}
	tmp, err := os.CreateTemp(c.root, ".chart-*.tmp")
	if err != nil {
		return fmt.Errorf("create chart lifecycle temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod chart lifecycle temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write chart lifecycle temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close chart lifecycle temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace chart lifecycle: %w", err)
	}
	return nil
}

func chartKey(sessionID, chartID string) string {
	return strings.TrimSpace(sessionID) + "\x00" + strings.TrimSpace(chartID)
}

// rememberChart records the version a server delta declared. A later delta
// for the same chart replaces it. Legacy charts (no mode) are not tracked.
func (c *chartLifecycle) rememberChart(sessionID string, version chartVersion) error {
	if c == nil || version.ChartID == "" || version.Mode == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(sessionID); err != nil {
		return err
	}
	key := chartKey(sessionID, version.ChartID)
	if previous, ok := c.current[key]; ok && previous.SchemaVersion == version.SchemaVersion && previous.DataVersion > version.DataVersion {
		return nil
	}
	c.current[key] = version
	return c.saveLocked(sessionID)
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
	if err := c.loadLocked(sessionID); err != nil {
		return chartApplyResult{}, false, err
	}
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
func (c *chartLifecycle) commitApply(sessionID, childTurn string, wire chartWireAction) (chartVersion, error) {
	if c == nil || wire.ChartID == "" {
		return chartVersion{}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(sessionID); err != nil {
		return chartVersion{}, err
	}
	key := chartKey(sessionID, wire.ChartID)
	current := c.current[key]
	if wire.ActionID != "" {
		applyKey := key + "\x00" + wire.ActionID
		if previous, seen := c.applied[applyKey]; seen {
			current.DataVersion = previous.DataVersion
			return current, nil
		}
		nextVersion := current.DataVersion
		if wire.Operation == "apply" {
			nextVersion++
		}
		c.applied[applyKey] = chartApplyResult{ActionID: wire.ActionID, DataVersion: nextVersion, ChildTurn: childTurn}
	}
	if wire.Operation == "apply" {
		current.DataVersion++
	}
	c.current[key] = current
	return current, c.saveLocked(sessionID)
}
