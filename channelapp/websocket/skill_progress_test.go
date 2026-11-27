package websocket

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/session"
	"github.com/yurika0211/luckyagent-runtime/tool"
)

func TestWebSocketSkillUseReachesClient(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewAgentHandler(&stubAgentRuntime{
		sessions: mgr,
		tools:    tool.NewRegistry(),
		chatStreamSess: func(context.Context, string, string) (<-chan agent.ChatEvent, error) {
			ch := make(chan agent.ChatEvent, 4)
			ch <- agent.ChatEvent{Type: agent.ChatEventThinking, Name: "skill_use", Content: "已采用 skill obsidian 的指导。"}
			ch <- agent.ChatEvent{Type: agent.ChatEventDone, Content: "done"}
			close(ch)
			return ch, nil
		},
	})
	client := &Client{SessionID: "ws-skill", Send: make(chan *Message, 8)}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.streamChat(ctx, client, ChatData{Message: "notes", Stream: true}, "parent")
	var saw bool
	for len(client.Send) > 0 {
		msg := <-client.Send
		if msg.Type != TypeReasoning {
			continue
		}
		var data ReasoningData
		if msg.ParseData(&data) != nil {
			continue
		}
		if data.Stage == "skill_use" && strings.Contains(data.Content, "obsidian") {
			saw = true
		}
	}
	if !saw {
		t.Fatal("client did not receive the skill name")
	}
}
