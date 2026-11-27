package server

import (
	"strings"
	"testing"

	"github.com/yurika0211/lucky-agent/agent"
)

func TestHTTPSkillUseFrameCarriesSkillName(t *testing.T) {
	frame := chatEventFrame(agent.ChatEvent{
		Type:    agent.ChatEventThinking,
		Name:    "skill_use",
		Content: "已采用 skill obsidian 的指导。",
	})
	if frame["type"] != "reason" || frame["name"] != "skill_use" || !strings.Contains(frame["content"].(string), "obsidian") {
		t.Fatalf("frame=%#v", frame)
	}
	unused := chatEventFrame(agent.ChatEvent{Type: agent.ChatEventThinking, Content: "Thinking... (round 1)"})
	if unused["name"] == "skill_use" || strings.Contains(unused["content"].(string), "已采用") {
		t.Fatalf("unused candidate looked like skill use: %#v", unused)
	}
}
