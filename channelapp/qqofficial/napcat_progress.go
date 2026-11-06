package qqofficial

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/yurika0211/luckyagent-runtime/config"
)

// NapCatProgressConfig is the app-side overlay for NapCat progress flags.
// The pinned runtime MsgGatewayNapCat may omit these keys. Missing keys stay
// false or empty, which keeps today's final-answer-only behavior.
type NapCatProgressConfig struct {
	ProgressSummaryWithLLM  bool   `json:"progress_summary_with_llm,omitempty"`
	ProgressSummaryPrompt   string `json:"progress_summary_prompt,omitempty"`
	ShowToolDetailsInResult bool   `json:"show_tool_details_in_result,omitempty"`
}

// DecodeNapCatProgress reads the three progress keys from a napcat JSON object.
// Unknown keys are ignored. A nil or empty payload returns the zero config.
func DecodeNapCatProgress(raw []byte) NapCatProgressConfig {
	var cfg NapCatProgressConfig
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg
	}
	_ = json.Unmarshal(raw, &cfg)
	cfg.ProgressSummaryPrompt = strings.TrimSpace(cfg.ProgressSummaryPrompt)
	return cfg
}

// LoadNapCatProgress reads msg_gateway.napcat from a config file. The pinned
// runtime unmarshal drops unknown fields, so this reads the file again and
// keeps only the progress keys. A missing file or missing object is zero.
func LoadNapCatProgress(configFile string) NapCatProgressConfig {
	path := strings.TrimSpace(configFile)
	if path == "" {
		return NapCatProgressConfig{}
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return NapCatProgressConfig{}
	}
	var envelope struct {
		MsgGateway struct {
			NapCat json.RawMessage `json:"napcat"`
		} `json:"msg_gateway"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return NapCatProgressConfig{}
	}
	return DecodeNapCatProgress(envelope.MsgGateway.NapCat)
}

// NapCatHandlerOptions builds NapCat handler options from the overlay.
// FinalAnswerOnly stays false. Feishu callers must not use this helper.
func NapCatHandlerOptions(progress NapCatProgressConfig) HandlerOptions {
	return HandlerOptions{
		PlatformName:           "napcat",
		DisplayName:            "NapCat QQ 网关",
		LogPrefix:              "napcat",
		ProgressSummaryWithLLM: progress.ProgressSummaryWithLLM,
		ProgressSummaryPrompt:  progress.ProgressSummaryPrompt,
		ShowToolDetails:        progress.ShowToolDetailsInResult,
	}
}

// NapCatProgressFromConfigFile loads the overlay for a runtime config manager.
// A nil manager returns the zero config.
func NapCatProgressFromConfigFile(mgr *config.Manager) NapCatProgressConfig {
	if mgr == nil {
		return NapCatProgressConfig{}
	}
	return LoadNapCatProgress(mgr.ConfigFile())
}
