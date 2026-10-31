package websocket

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/openui"
	"github.com/yurika0211/luckyagent-runtime/session"
	"github.com/yurika0211/luckyagent-runtime/tool"
)

func TestHandleUIActionRejectsUnknownTree(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewAgentHandler(&stubAgentRuntime{sessions: mgr, tools: tool.NewRegistry()})
	client := &Client{ID: "android", SessionID: "ui-session", Send: make(chan *Message, 4)}
	data, err := json.Marshal(UIActionData{TreeID: "missing", WidgetID: "go"})
	if err != nil {
		t.Fatal(err)
	}
	h.HandleMessage(client, &Message{Type: TypeUIAction, SessionID: client.SessionID, ID: "action-1", Data: data})

	select {
	case msg := <-client.Send:
		if msg.Type != TypeError {
			t.Fatalf("message type = %s, want error", msg.Type)
		}
		var got ErrorData
		if err := msg.ParseData(&got); err != nil {
			t.Fatal(err)
		}
		if got.Code != "UI_ACTION_REJECTED" || !strings.Contains(got.Message, "unavailable") {
			t.Fatalf("error = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("missing rejection")
	}
}

func TestHandleUIActionQueuesValidatedChoice(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan agent.UserTurnInput, 1)
	runtime := &stubAgentRuntime{
		sessions: mgr,
		tools:    tool.NewRegistry(),
		chatStreamInputFn: func(ctx context.Context, _ string, input agent.UserTurnInput) (<-chan agent.ChatEvent, error) {
			started <- input
			out := make(chan agent.ChatEvent, 1)
			out <- agent.ChatEvent{Type: agent.ChatEventDone, Content: "done"}
			close(out)
			return out, nil
		},
	}
	h := NewAgentHandler(runtime)
	h.recordUIDelta("ui-session", openui.Delta{
		TreeID: "tree-1",
		State:  openui.StateWaitingInput,
		Nodes: []openui.Node{{
			ID:     "choices",
			Type:   "Actions",
			Policy: openui.PolicyMutate,
			Props: map[string]any{
				"items": []any{map[string]any{"id": "go", "label": "继续"}},
			},
		}},
	})
	client := &Client{ID: "android", SessionID: "ui-session", Send: make(chan *Message, 16)}
	data, err := json.Marshal(UIActionData{
		TreeID:   "tree-1",
		WidgetID: "go",
		Values:   map[string]any{"label": "继续"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.HandleMessage(client, &Message{Type: TypeUIAction, SessionID: client.SessionID, ID: "action-1", Data: data})

	select {
	case input := <-started:
		if !strings.Contains(input.RoutingText, "[OpenUI action]") ||
			!strings.Contains(input.RoutingText, "tree-1") ||
			!strings.Contains(input.RoutingText, "继续") {
			t.Fatalf("action prompt = %q", input.RoutingText)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("validated action did not start a run")
	}
	if !h.WaitSession(client.SessionID, 2*time.Second) {
		t.Fatal("action run did not finish")
	}
	if _, err := h.takeUIAction(client.SessionID, openui.Action{TreeID: "tree-1", WidgetID: "go"}); err == nil {
		t.Fatal("action tree was reusable after a successful dispatch")
	}
}

func TestHandleUIActionRejectsStreamingAndUnknownWidget(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewAgentHandler(&stubAgentRuntime{sessions: mgr, tools: tool.NewRegistry()})
	h.recordUIDelta("ui-session", openui.Delta{
		TreeID: "streaming-tree",
		State:  openui.StateStreaming,
		Nodes: []openui.Node{{ID: "choices", Type: "Actions", Policy: openui.PolicyMutate, Props: map[string]any{
			"items": []any{map[string]any{"id": "go", "label": "继续"}},
		}}},
	})
	client := &Client{ID: "android", SessionID: "ui-session", Send: make(chan *Message, 4)}
	data, _ := json.Marshal(UIActionData{TreeID: "streaming-tree", WidgetID: "go"})
	h.HandleMessage(client, &Message{Type: TypeUIAction, SessionID: client.SessionID, ID: "action-streaming", Data: data})
	assertUIActionError(t, client, "UI_ACTION_REJECTED", "not ready")

	h.recordUIDelta("ui-session", openui.Delta{
		TreeID: "complete-tree",
		State:  openui.StateComplete,
		Nodes: []openui.Node{{ID: "choices", Type: "Actions", Policy: openui.PolicyMutate, Props: map[string]any{
			"items": []any{map[string]any{"id": "go", "label": "继续"}},
		}}},
	})
	data, _ = json.Marshal(UIActionData{TreeID: "complete-tree", WidgetID: "missing"})
	h.HandleMessage(client, &Message{Type: TypeUIAction, SessionID: client.SessionID, ID: "action-widget", Data: data})
	assertUIActionError(t, client, "UI_ACTION_REJECTED", "not an action")
}

func assertUIActionError(t *testing.T, client *Client, code, text string) {
	t.Helper()
	select {
	case msg := <-client.Send:
		if msg.Type != TypeError {
			t.Fatalf("message type = %s, want error", msg.Type)
		}
		var got ErrorData
		if err := msg.ParseData(&got); err != nil {
			t.Fatal(err)
		}
		if got.Code != code || !strings.Contains(got.Message, text) {
			t.Fatalf("error = %+v, want code %q containing %q", got, code, text)
		}
	case <-time.After(time.Second):
		t.Fatalf("missing %s rejection", code)
	}
}
