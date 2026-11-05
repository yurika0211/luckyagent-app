package qqofficial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/config"
	"github.com/yurika0211/luckyagent-runtime/gateway"
)

func napcatProgressEvents(events ...agent.ChatEvent) (context.Context, context.CancelFunc, *gateway.Message, chan agent.ChatEvent) {
	ch := make(chan agent.ChatEvent, len(events))
	for _, evt := range events {
		ch <- evt
	}
	close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	msg := &gateway.Message{
		ID:   "msg-1",
		Chat: gateway.Chat{ID: "group:123", Type: gateway.ChatGroup},
	}
	return ctx, cancel, msg, ch
}

func TestNapCatDefaultSendsFinalAnswerOnly(t *testing.T) {
	sender := &qqHandlerTestSender{}
	handler := NewHandlerWithOptions(sender, nil, NapCatHandlerOptions(NapCatProgressConfig{}))
	if handler.finalAnswerOnly || handler.progressSummary || handler.showToolDetails {
		t.Fatalf("default napcat flags = final %v summary %v tools %v", handler.finalAnswerOnly, handler.progressSummary, handler.showToolDetails)
	}
	ctx, cancel, msg, events := napcatProgressEvents(
		agent.ChatEvent{Type: agent.ChatEventThinking, Content: "Thinking... (round 1)"},
		agent.ChatEvent{Type: agent.ChatEventToolCall, Name: "web_search", Args: `{"query":"test","api_key":"secret-key"}`},
		agent.ChatEvent{Type: agent.ChatEventToolResult, Name: "web_search", Result: "ok"},
		agent.ChatEvent{Type: agent.ChatEventDone, Content: "最终答案"},
	)
	defer cancel()
	if err := handler.handleChatEventStream(ctx, msg, events); err != nil {
		t.Fatalf("handleChatEventStream() error = %v", err)
	}
	messages := sender.Messages()
	if len(messages) != 1 || messages[0] != "最终答案" {
		t.Fatalf("expected one final answer, got %#v", messages)
	}
}

func TestNapCatProgressSummarySendsShortRoundNote(t *testing.T) {
	sender := &qqHandlerTestSender{}
	handler := NewHandlerWithOptions(sender, nil, HandlerOptions{
		PlatformName:           "napcat",
		ProgressSummaryWithLLM: true,
		RoundSummary: func(_ context.Context, _ string, round int, _ []string, _ string) (string, error) {
			return "先核对公开资料，再给你结论。", nil
		},
	})
	ctx, cancel, msg, events := napcatProgressEvents(
		agent.ChatEvent{Type: agent.ChatEventThinking, Content: "Thinking... (round 1)"},
		agent.ChatEvent{Type: agent.ChatEventDone, Content: "最终答案"},
	)
	defer cancel()
	if err := handler.handleChatEventStream(ctx, msg, events); err != nil {
		t.Fatalf("handleChatEventStream() error = %v", err)
	}
	messages := sender.Messages()
	if len(messages) != 2 {
		t.Fatalf("expected summary and final answer, got %#v", messages)
	}
	if messages[0] != "先核对公开资料，再给你结论。" || messages[1] != "最终答案" {
		t.Fatalf("unexpected messages: %#v", messages)
	}
	if strings.Contains(messages[0], "Thinking...") {
		t.Fatalf("summary dumped raw thinking: %q", messages[0])
	}
}

func TestNapCatProgressSummaryForwardsPrompt(t *testing.T) {
	sender := &qqHandlerTestSender{}
	var gotPrompt string
	var gotRound int
	handler := NewHandlerWithOptions(sender, nil, HandlerOptions{
		PlatformName:           "napcat",
		ProgressSummaryWithLLM: true,
		ProgressSummaryPrompt:  "用一句中文说进度",
		RoundSummary: func(_ context.Context, _ string, round int, _ []string, prompt string) (string, error) {
			gotPrompt = prompt
			gotRound = round
			return "进度：在核对来源。", nil
		},
	})
	ctx, cancel, msg, events := napcatProgressEvents(
		agent.ChatEvent{Type: agent.ChatEventThinking, Content: "Thinking... (round 1)"},
		agent.ChatEvent{Type: agent.ChatEventDone, Content: "最终答案"},
	)
	defer cancel()
	if err := handler.handleChatEventStream(ctx, msg, events); err != nil {
		t.Fatalf("handleChatEventStream() error = %v", err)
	}
	if gotPrompt != "用一句中文说进度" || gotRound != 1 {
		t.Fatalf("prompt %q round %d", gotPrompt, gotRound)
	}
}

func TestNapCatShowToolDetailsOffHidesToolName(t *testing.T) {
	sender := &qqHandlerTestSender{}
	handler := NewHandlerWithOptions(sender, nil, HandlerOptions{
		PlatformName:    "napcat",
		ShowToolDetails: false,
	})
	ctx, cancel, msg, events := napcatProgressEvents(
		agent.ChatEvent{Type: agent.ChatEventToolCall, Name: "web_search", Args: `{"api_key":"secret-key"}`},
		agent.ChatEvent{Type: agent.ChatEventToolResult, Name: "web_search", Result: "ok"},
		agent.ChatEvent{Type: agent.ChatEventDone, Content: "最终答案"},
	)
	defer cancel()
	if err := handler.handleChatEventStream(ctx, msg, events); err != nil {
		t.Fatalf("handleChatEventStream() error = %v", err)
	}
	joined := strings.Join(sender.Messages(), "\n")
	if strings.Contains(joined, "web_search") || strings.Contains(joined, "secret-key") {
		t.Fatalf("tool details leaked: %q", joined)
	}
	if joined != "最终答案" {
		t.Fatalf("expected final answer only, got %q", joined)
	}
}

func TestNapCatShowToolDetailsSendsShortResultWithoutArgs(t *testing.T) {
	sender := &qqHandlerTestSender{}
	handler := NewHandlerWithOptions(sender, nil, HandlerOptions{
		PlatformName:    "napcat",
		ShowToolDetails: true,
	})
	ctx, cancel, msg, events := napcatProgressEvents(
		agent.ChatEvent{Type: agent.ChatEventToolCall, Name: "web_search", Args: `{"api_key":"secret-key-should-not-appear"}`},
		agent.ChatEvent{Type: agent.ChatEventToolResult, Name: "web_search", Result: "ok"},
		agent.ChatEvent{Type: agent.ChatEventDone, Content: "最终答案"},
	)
	defer cancel()
	if err := handler.handleChatEventStream(ctx, msg, events); err != nil {
		t.Fatalf("handleChatEventStream() error = %v", err)
	}
	messages := sender.Messages()
	joined := strings.Join(messages, "\n")
	if strings.Contains(joined, "secret-key-should-not-appear") || strings.Contains(joined, "api_key") {
		t.Fatalf("args leaked: %#v", messages)
	}
	if len(messages) != 2 || !strings.Contains(messages[0], "web_search") || !strings.Contains(messages[0], "ok") || messages[1] != "最终答案" {
		t.Fatalf("expected short result then final answer, got %#v", messages)
	}
}

func TestNapCatSummaryNilRuntimeDoesNotPanic(t *testing.T) {
	sender := &qqHandlerTestSender{}
	handler := NewHandlerWithOptions(sender, nil, HandlerOptions{
		PlatformName:           "napcat",
		ProgressSummaryWithLLM: true,
	})
	if handler.roundSummary != nil {
		t.Fatal("nil runtime should not install a summary func")
	}
	ctx, cancel, msg, events := napcatProgressEvents(
		agent.ChatEvent{Type: agent.ChatEventThinking, Content: "Thinking... (round 1)"},
		agent.ChatEvent{Type: agent.ChatEventDone, Content: "最终答案"},
	)
	defer cancel()
	if err := handler.handleChatEventStream(ctx, msg, events); err != nil {
		t.Fatalf("handleChatEventStream() error = %v", err)
	}
	messages := sender.Messages()
	if len(messages) != 2 || strings.Contains(messages[0], "Thinking...") || messages[1] != "最终答案" {
		t.Fatalf("unexpected messages: %#v", messages)
	}
}

func TestFeishuHandlerOptionsKeepFinalAnswerOnly(t *testing.T) {
	handler := NewHandlerWithOptions(&qqHandlerTestSender{}, nil, HandlerOptions{
		PlatformName:    "feishu",
		FinalAnswerOnly: true,
	})
	if !handler.finalAnswerOnly || handler.napcatQuiet() {
		t.Fatalf("feishu finalAnswerOnly=%v quiet=%v", handler.finalAnswerOnly, handler.napcatQuiet())
	}
}

func TestDecodeNapCatProgressMissingKeysStayZero(t *testing.T) {
	got := DecodeNapCatProgress([]byte(`{"listen_addr":"127.0.0.1:6701"}`))
	if got.ProgressSummaryWithLLM || got.ProgressSummaryPrompt != "" || got.ShowToolDetailsInResult {
		t.Fatalf("missing keys should stay zero, got %#v", got)
	}
	got = DecodeNapCatProgress([]byte(`{"progress_summary_with_llm":true,"progress_summary_prompt":" 简体 ","show_tool_details_in_result":true}`))
	if !got.ProgressSummaryWithLLM || got.ProgressSummaryPrompt != "简体" || !got.ShowToolDetailsInResult {
		t.Fatalf("decoded %#v", got)
	}
}

func TestLoadNapCatProgressFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
		"msg_gateway": {
			"napcat": {
				"listen_addr": "127.0.0.1:6701",
				"progress_summary_with_llm": true,
				"progress_summary_prompt": "短句",
				"show_tool_details_in_result": true
			}
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := config.NewManagerWithDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mgr.ConfigFile(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := NapCatProgressFromConfigFile(mgr)
	if !got.ProgressSummaryWithLLM || got.ProgressSummaryPrompt != "短句" || !got.ShowToolDetailsInResult {
		t.Fatalf("file overlay %#v from %s", got, mgr.ConfigFile())
	}
	opts := NapCatHandlerOptions(got)
	if opts.FinalAnswerOnly || opts.PlatformName != "napcat" || !opts.ShowToolDetails || opts.ProgressSummaryPrompt != "短句" {
		t.Fatalf("handler options %#v", opts)
	}
}
