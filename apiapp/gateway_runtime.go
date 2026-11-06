package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-app/gateway/qqofficial"
	"github.com/yurika0211/luckyagent-app/gateway/telegram"
	"github.com/yurika0211/luckyagent-runtime/config"
	"github.com/yurika0211/luckyagent-runtime/gateway"
	luckycollector "github.com/yurika0211/luckyagent-runtime/gateway/collector"
	"github.com/yurika0211/luckyagent-runtime/gateway/feishu"
	"github.com/yurika0211/luckyagent-runtime/gateway/napcat"
	"github.com/yurika0211/luckyagent-runtime/gateway/openclawweixin"
	"github.com/yurika0211/luckyagent-runtime/gateway/weixin"
	"github.com/yurika0211/luckyagent-runtime/tool"
)

// gatewayStartRequest contains optional per-start overrides. An omitted field
// always falls back to the corresponding msg_gateway config value, which lets
// the Android client start a configured channel with an empty JSON object.
// Secrets are accepted only in the request body and are never returned by the
// gateway status endpoints.
type gatewayStartRequest struct {
	Token             string   `json:"token,omitempty"`
	Proxy             string   `json:"proxy,omitempty"`
	AppID             string   `json:"app_id,omitempty"`
	AppSecret         string   `json:"app_secret,omitempty"`
	Sandbox           *bool    `json:"sandbox,omitempty"`
	APIBaseURL        string   `json:"api_base_url,omitempty"`
	GatewayURL        string   `json:"gateway_url,omitempty"`
	ListenAddr        string   `json:"listen_addr,omitempty"`
	Path              string   `json:"path,omitempty"`
	AccessToken       string   `json:"access_token,omitempty"`
	VerificationToken string   `json:"verification_token,omitempty"`
	EncryptKey        string   `json:"encrypt_key,omitempty"`
	AccountID         string   `json:"account_id,omitempty"`
	BaseURL           string   `json:"base_url,omitempty"`
	StateDir          string   `json:"state_dir,omitempty"`
	AllowedChats      []string `json:"allowed_chats,omitempty"`
	AllowedUsers      []string `json:"allowed_users,omitempty"`
	AdminIDs          []string `json:"admin_ids,omitempty"`
	GroupAllowedUsers []string `json:"group_allowed_users,omitempty"`
}

func decodeGatewayStartRequest(r *http.Request) (gatewayStartRequest, error) {
	var req gatewayStartRequest
	if r == nil || r.Body == nil {
		return req, nil
	}
	decErr := jsonAPI.NewDecoder(r.Body).Decode(&req)
	if errors.Is(decErr, io.EOF) {
		return req, nil
	}
	if decErr != nil {
		return req, decErr
	}
	return req, nil
}

func normalizeGatewayName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "tg", "telegram":
		return "telegram"
	case "qq", "qqo", "qqofficial", "official":
		return "qqofficial"
	case "nap", "napcat", "onebot":
		return "napcat"
	case "fs", "feishu", "lark":
		return "feishu"
	case "wx", "weixin", "ilink":
		return "weixin"
	case "ocwx", "openclaw", "openclawweixin":
		return "openclawweixin"
	default:
		return strings.TrimSpace(name)
	}
}

func preferString(override, configured string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	return strings.TrimSpace(configured)
}

func preferStrings(override, configured []string) []string {
	if override != nil {
		return append([]string(nil), override...)
	}
	return append([]string(nil), configured...)
}

func preferBool(override *bool, configured bool) bool {
	if override != nil {
		return *override
	}
	return configured
}

func (r gatewayStartRequest) hasOverrides() bool {
	return strings.TrimSpace(r.Token) != "" || strings.TrimSpace(r.Proxy) != "" ||
		strings.TrimSpace(r.AppID) != "" || strings.TrimSpace(r.AppSecret) != "" ||
		r.Sandbox != nil || strings.TrimSpace(r.APIBaseURL) != "" ||
		strings.TrimSpace(r.GatewayURL) != "" || strings.TrimSpace(r.ListenAddr) != "" ||
		strings.TrimSpace(r.Path) != "" || strings.TrimSpace(r.AccessToken) != "" ||
		strings.TrimSpace(r.VerificationToken) != "" || strings.TrimSpace(r.EncryptKey) != "" ||
		strings.TrimSpace(r.AccountID) != "" || strings.TrimSpace(r.BaseURL) != "" ||
		strings.TrimSpace(r.StateDir) != "" || r.AllowedChats != nil ||
		r.AllowedUsers != nil || r.AdminIDs != nil || r.GroupAllowedUsers != nil
}

// gatewayStartContext is deliberately independent from the HTTP request.
// Tests that invoke handlers directly get a background context; normal API
// requests use the context owned by Server.Start.
func (s *Server) gatewayStartContext() context.Context {
	if s != nil {
		s.mu.RLock()
		ctx := s.gatewayCtx
		s.mu.RUnlock()
		if ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

// acquireGatewayLease reserves process-wide resources that must not be shared
// with a separate gateway process. Telegram long polling is the first such
// resource: two consumers for one bot token cause Telegram to evict one of
// them. The lease is held until the gateway is stopped or the API server exits.
func (s *Server) acquireGatewayLease(name string) error {
	if name != "telegram" || s == nil || s.agent == nil || s.agent.Config() == nil {
		return nil
	}
	s.mu.RLock()
	hasLease := s.gatewayReleases != nil && s.gatewayReleases[name] != nil
	s.mu.RUnlock()
	if hasLease {
		return nil
	}
	homeDir := strings.TrimSpace(s.agent.Config().HomeDir())
	if homeDir == "" {
		return nil
	}
	release, err := gateway.AcquireTelegramGatewayLease(homeDir)
	if err != nil {
		return err
	}
	if name == "telegram" {
		// AcquireTelegramGatewayLease deliberately treats a fresh shared state
		// snapshot as authoritative. Keep that snapshot fresh for API-owned
		// gateways as the CLI does, otherwise a long-running server would look
		// stale after fifteen seconds and another process could take the lease.
		heartbeatCtx, heartbeatCancel := context.WithCancel(context.Background())
		heartbeatDone := make(chan struct{})
		go func() {
			defer close(heartbeatDone)
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			writeState := func() {
				state := gateway.SharedTelegramState{PID: os.Getpid(), Registered: true}
				if gw, ok := s.agent.MsgGateway().Get("telegram"); ok {
					state.Connected = gw.IsRunning()
					if stats, ok := s.agent.MsgGateway().Stats("telegram"); ok {
						state.MessagesSent = stats.MessagesSent
						state.MessagesReceived = stats.MessagesReceived
						state.Errors = stats.Errors
					}
				}
				_ = gateway.WriteSharedTelegramState(homeDir, state)
			}
			writeState()
			for {
				select {
				case <-heartbeatCtx.Done():
					return
				case <-ticker.C:
					writeState()
				}
			}
		}()
		originalRelease := release
		release = func() {
			heartbeatCancel()
			<-heartbeatDone
			_ = os.Remove(gateway.TelegramRuntimeStatePath(homeDir))
			originalRelease()
		}
	}
	s.mu.Lock()
	if s.gatewayReleases == nil {
		s.gatewayReleases = make(map[string]func())
	}
	if existing := s.gatewayReleases[name]; existing != nil {
		s.mu.Unlock()
		release()
		return nil
	}
	s.gatewayReleases[name] = release
	s.mu.Unlock()
	return nil
}

func (s *Server) releaseGatewayLease(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	release := s.gatewayReleases[name]
	delete(s.gatewayReleases, name)
	s.mu.Unlock()
	if release != nil {
		release()
	}
}

func (s *Server) releaseAllGatewayLeases() {
	if s == nil {
		return
	}
	s.mu.Lock()
	releases := make([]func(), 0, len(s.gatewayReleases))
	for name, release := range s.gatewayReleases {
		if release != nil {
			releases = append(releases, release)
		}
		delete(s.gatewayReleases, name)
	}
	s.mu.Unlock()
	for _, release := range releases {
		release()
	}
}

func (s *Server) handleGatewayStart(w http.ResponseWriter, r *http.Request, rawName string) {
	if r.Method != http.MethodPost {
		s.sendError(w, "method not allowed", http.StatusMethodNotAllowed, "")
		return
	}
	name := normalizeGatewayName(rawName)
	if !knownGatewayName(name) {
		s.sendError(w, "gateway not supported", http.StatusNotFound, name)
		return
	}
	req, err := decodeGatewayStartRequest(r)
	if err != nil {
		s.sendError(w, "invalid request body", http.StatusBadRequest, err.Error())
		return
	}
	if s == nil || s.agent == nil || s.agent.MsgGateway() == nil {
		s.sendError(w, "gateway manager unavailable", http.StatusInternalServerError, "")
		return
	}

	gm := s.agent.MsgGateway()
	if existing, exists := gm.Get(name); exists {
		if existing.IsRunning() {
			s.sendError(w, "gateway already running", http.StatusConflict, name)
			return
		}
		if req.hasOverrides() {
			// A stopped adapter can be replaced when the caller explicitly
			// supplies credentials or listener settings (for example, a new
			// Telegram token from the Android dialog).
			if err := s.acquireGatewayLease(name); err != nil {
				s.sendError(w, "gateway already running elsewhere", http.StatusConflict, err.Error())
				return
			}
			if err := gm.Unregister(name); err != nil {
				s.releaseGatewayLease(name)
				s.sendError(w, "failed to replace gateway", http.StatusInternalServerError, err.Error())
				return
			}
		} else {
			// Stop keeps the adapter registered. Starting it again is the safe
			// idempotent path for Android's start/stop controls; request overrides
			// apply when the adapter is first registered.
			if err := s.acquireGatewayLease(name); err != nil {
				s.sendError(w, "gateway already running elsewhere", http.StatusConflict, err.Error())
				return
			}
			if err := gm.Start(s.gatewayStartContext(), name); err != nil {
				s.releaseGatewayLease(name)
				s.sendError(w, "failed to start gateway", http.StatusInternalServerError, err.Error())
				return
			}
			s.sendJSON(w, http.StatusOK, map[string]interface{}{
				"name": name, "platform": name, "message": fmt.Sprintf("gateway %s started", name), "running": true,
			})
			return
		}
	}
	if err := s.acquireGatewayLease(name); err != nil {
		s.sendError(w, "gateway already running elsewhere", http.StatusConflict, err.Error())
		return
	}

	gw, err := s.buildGateway(name, req)
	if err != nil {
		s.releaseGatewayLease(name)
		s.sendError(w, "invalid gateway configuration", http.StatusBadRequest, err.Error())
		return
	}
	if err := gm.Register(gw); err != nil {
		s.releaseGatewayLease(name)
		s.sendError(w, "failed to register gateway", http.StatusInternalServerError, err.Error())
		return
	}
	if err := gm.Start(s.gatewayStartContext(), name); err != nil {
		// A failed first start must not leave a dead adapter blocking retries.
		_ = gm.Unregister(name)
		s.releaseGatewayLease(name)
		s.sendError(w, "failed to start gateway", http.StatusInternalServerError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, map[string]interface{}{
		"name": name, "platform": name, "message": fmt.Sprintf("gateway %s started", name), "running": true,
	})
}

func knownGatewayName(name string) bool {
	switch name {
	case "telegram", "qqofficial", "napcat", "feishu", "weixin", "openclawweixin":
		return true
	default:
		return false
	}
}

func (s *Server) buildGateway(name string, req gatewayStartRequest) (gateway.Gateway, error) {
	cfg := config.Config{}
	if s != nil && s.agent != nil && s.agent.Config() != nil && s.agent.Config().Get() != nil {
		cfg = *s.agent.Config().Get()
	}
	homeDir := ""
	if s != nil && s.agent != nil && s.agent.Config() != nil {
		homeDir = s.agent.Config().HomeDir()
	}
	mg := cfg.MsgGateway

	switch name {
	case "telegram":
		token := preferString(req.Token, mg.Telegram.Token)
		if err := requiredGatewayValue(name, "token", token); err != nil {
			return nil, err
		}
		adapter := telegram.NewAdapter(telegram.Config{
			Token:               token,
			Proxy:               preferString(req.Proxy, mg.Telegram.Proxy),
			AllowedChats:        preferStrings(req.AllowedChats, mg.Telegram.AllowedChats),
			AdminIDs:            preferStrings(req.AdminIDs, mg.Telegram.AdminIDs),
			DisableAutoReaction: mg.Telegram.DisableAutoReaction,
		})
		h := telegram.NewHandlerWithRuntime(adapter, telegram.NewRuntime(s.agent))
		if homeDir != "" {
			h.SetDataDir(filepath.Join(homeDir, "data", "telegram"))
		}
		adapter.SetHandler(func(ctx context.Context, msg *gateway.Message) error { return h.HandleMessage(ctx, msg) })
		return adapter, nil

	case "qqofficial":
		appID := preferString(req.AppID, mg.QQOfficial.AppID)
		appSecret := preferString(req.AppSecret, mg.QQOfficial.AppSecret)
		if err := requiredGatewayValue(name, "app_id", appID); err != nil {
			return nil, err
		}
		if err := requiredGatewayValue(name, "app_secret", appSecret); err != nil {
			return nil, err
		}
		sandbox := preferBool(req.Sandbox, mg.QQOfficial.Sandbox)
		adapter := qqofficial.NewAdapter(qqofficial.Config{
			AppID:         appID,
			AppSecret:     appSecret,
			Proxy:         preferString(req.Proxy, mg.QQOfficial.Proxy),
			Sandbox:       sandbox,
			APIBaseURL:    preferString(req.APIBaseURL, mg.QQOfficial.APIBaseURL),
			GatewayURL:    preferString(req.GatewayURL, mg.QQOfficial.GatewayURL),
			LogPath:       filepath.Join(homeDir, "logs", "qqofficial-gateway.log"),
			AllowedChats:  preferStrings(req.AllowedChats, mg.QQOfficial.AllowedChats),
			AllowedUsers:  preferStrings(req.AllowedUsers, mg.QQOfficial.AllowedUsers),
			RemoveAt:      mg.QQOfficial.RemoveAt,
			HeartbeatSec:  mg.QQOfficial.HeartbeatSec,
			ReconnectWait: mg.QQOfficial.ReconnectWait,
			Intents:       append([]string(nil), mg.QQOfficial.Intents...),
		})
		h := qqofficial.NewHandler(adapter, s.agent)
		if homeDir != "" {
			h.SetDataDir(filepath.Join(homeDir, "data", "qqofficial"))
		}
		adapter.SetHandler(func(ctx context.Context, msg *gateway.Message) error { return h.HandleMessage(ctx, msg) })
		return adapter, nil

	case "napcat":
		adapter := napcat.NewAdapter(napcat.Config{
			ListenAddr:       preferString(req.ListenAddr, mg.NapCat.ListenAddr),
			Path:             preferString(req.Path, mg.NapCat.Path),
			AccessToken:      preferString(req.AccessToken, mg.NapCat.AccessToken),
			AllowedChats:     preferStrings(req.AllowedChats, mg.NapCat.AllowedChats),
			AllowedUsers:     preferStrings(req.AllowedUsers, mg.NapCat.AllowedUsers),
			RemoveAt:         mg.NapCat.RemoveAt,
			GroupTriggerMode: mg.NapCat.GroupTriggerMode,
		})
		if mg.NapCat.CrossGroupRead.Enabled {
			s.agent.Tools().Register(tool.NapCatReadGroupTool(adapter, tool.NapCatCrossGroupReadConfig{
				Enabled: true, RequireConfirmation: mg.NapCat.CrossGroupRead.RequireConfirmation,
				AllowedGroups: append([]string(nil), mg.NapCat.CrossGroupRead.AllowedGroups...),
				BlockedGroups: append([]string(nil), mg.NapCat.CrossGroupRead.BlockedGroups...),
				LogAccess:     mg.NapCat.CrossGroupRead.LogAccess,
			}))
		}
		progress := qqofficial.NapCatProgressConfig{}
		if s != nil && s.agent != nil {
			progress = qqofficial.NapCatProgressFromConfigFile(s.agent.Config())
		}
		h := qqofficial.NewHandlerWithOptions(adapter, s.agent, qqofficial.NapCatHandlerOptions(progress))
		if homeDir != "" {
			h.SetDataDir(filepath.Join(homeDir, "data", "napcat"))
		}
		adapter.SetHandler(func(ctx context.Context, msg *gateway.Message) error { return h.HandleMessage(ctx, msg) })
		return adapter, nil

	case "feishu":
		appID := preferString(req.AppID, mg.Feishu.AppID)
		appSecret := preferString(req.AppSecret, mg.Feishu.AppSecret)
		if err := requiredGatewayValue(name, "app_id", appID); err != nil {
			return nil, err
		}
		if err := requiredGatewayValue(name, "app_secret", appSecret); err != nil {
			return nil, err
		}
		adapter := feishu.NewAdapter(feishu.Config{
			AppID:             appID,
			AppSecret:         appSecret,
			VerificationToken: preferString(req.VerificationToken, mg.Feishu.VerificationToken),
			EncryptKey:        preferString(req.EncryptKey, mg.Feishu.EncryptKey),
			RenderMode:        mg.Feishu.RenderMode,
			MediaEnabled:      mg.Feishu.MediaEnabled,
			MaxMediaBytes:     mg.Feishu.MaxMediaBytes,
			CardProgress:      mg.Feishu.CardProgress,
			ListenAddr:        preferString(req.ListenAddr, mg.Feishu.ListenAddr),
			Path:              preferString(req.Path, mg.Feishu.Path),
			APIBaseURL:        preferString(req.APIBaseURL, mg.Feishu.APIBaseURL),
			AllowedChats:      preferStrings(req.AllowedChats, mg.Feishu.AllowedChats),
			AllowedUsers:      preferStrings(req.AllowedUsers, mg.Feishu.AllowedUsers),
			RemoveAt:          mg.Feishu.RemoveAt,
			GroupTriggerMode:  mg.Feishu.GroupTriggerMode,
			DataDir:           filepath.Join(homeDir, "data", "feishu"),
		})
		h := qqofficial.NewHandlerWithOptions(adapter, s.agent, qqofficial.HandlerOptions{
			PlatformName: "feishu", DisplayName: "飞书网关", LogPrefix: "feishu", FinalAnswerOnly: true,
			DeliveryGuidance: func(input string) string { return feishu.MediaDeliveryGuidance(input, mg.Feishu.MediaEnabled) },
		})
		if homeDir != "" {
			h.SetDataDir(filepath.Join(homeDir, "data", "feishu"))
		}
		adapter.SetHandler(func(ctx context.Context, msg *gateway.Message) error { return h.HandleMessage(ctx, msg) })
		return adapter, nil

	case "weixin":
		token := preferString(req.Token, mg.Weixin.Token)
		accountID := preferString(req.AccountID, mg.Weixin.AccountID)
		if err := requiredGatewayValue(name, "token", token); err != nil {
			return nil, err
		}
		if err := requiredGatewayValue(name, "account_id", accountID); err != nil {
			return nil, err
		}
		adapter := weixin.NewAdapter(weixin.Config{
			Token: token, AccountID: accountID,
			BaseURL: preferString(req.BaseURL, mg.Weixin.BaseURL), DMPolicy: mg.Weixin.DMPolicy, GroupPolicy: mg.Weixin.GroupPolicy,
			AllowedUsers: preferStrings(req.AllowedUsers, mg.Weixin.AllowedUsers), GroupAllowedUsers: preferStrings(req.GroupAllowedUsers, mg.Weixin.GroupAllowedUsers),
			SplitMultilineMessages: mg.Weixin.SplitMultilineMessages, PollTimeoutMilliseconds: mg.Weixin.PollTimeoutMilliseconds, SendChunkDelayMS: mg.Weixin.SendChunkDelayMS,
		})
		adapter.SetHandler(newPlainGatewayHandler("weixin", s.agent, adapter))
		return adapter, nil

	case "openclawweixin":
		accountID := preferString(req.AccountID, mg.OpenClawWeixin.AccountID)
		if err := requiredGatewayValue(name, "account_id", accountID); err != nil {
			return nil, err
		}
		adapter := openclawweixin.NewAdapter(openclawweixin.Config{
			AccountID: accountID, StateDir: preferString(req.StateDir, mg.OpenClawWeixin.StateDir),
			DMPolicy: mg.OpenClawWeixin.DMPolicy, GroupPolicy: mg.OpenClawWeixin.GroupPolicy,
			AllowedUsers: preferStrings(req.AllowedUsers, mg.OpenClawWeixin.AllowedUsers), GroupAllowedUsers: preferStrings(req.GroupAllowedUsers, mg.OpenClawWeixin.GroupAllowedUsers),
			SplitMultilineMessages: mg.OpenClawWeixin.SplitMultilineMessages, PollTimeoutMilliseconds: mg.OpenClawWeixin.PollTimeoutMilliseconds, SendChunkDelayMS: mg.OpenClawWeixin.SendChunkDelayMS,
		})
		adapter.SetHandler(newPlainGatewayHandler("openclawweixin", s.agent, adapter))
		return adapter, nil
	default:
		return nil, fmt.Errorf("unsupported gateway %q", name)
	}
}

func requiredGatewayValue(name, field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s requires %s", name, field)
	}
	return nil
}

func newPlainGatewayHandler(platform string, runtime *agent.Agent, gw gateway.Gateway) gateway.MessageHandler {
	lucky := luckycollector.NewLucky()
	return func(ctx context.Context, msg *gateway.Message) error {
		if msg == nil || runtime == nil || gw == nil {
			return nil
		}
		if msg.IsCommand && strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(msg.Command), "/"), "lucky") {
			return handlePlainLuckyCommand(ctx, platform, gw, lucky, msg, runtime)
		}
		key := luckycollector.KeyForMessage(platform, msg)
		if status := lucky.Status(key); status.Active {
			status, err := lucky.Append(key, msg)
			if err != nil {
				return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, "Lucky 收集失败："+err.Error())
			}
			return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, fmt.Sprintf("已收集第 %d 段（附件 %d 个）。发送 /lucky off 提交。", status.SegmentCount, status.AttachmentCount))
		}
		if strings.TrimSpace(msg.Text) == "" && len(msg.Attachments) == 0 {
			return nil
		}
		return sendPlainGatewayInput(ctx, platform, runtime, gw, msg, agent.MultimodalUserTurnInput(msg.Text, msg.Attachments))
	}
}

func handlePlainLuckyCommand(ctx context.Context, platform string, gw gateway.Gateway, lucky *luckycollector.Lucky, msg *gateway.Message, runtime *agent.Agent) error {
	key := luckycollector.KeyForMessage(platform, msg)
	switch luckycollector.ParseLuckyAction(msg.Args) {
	case luckycollector.LuckyActionOn:
		status, err := lucky.Start(key)
		if errors.Is(err, luckycollector.ErrAlreadyActive) {
			return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, fmt.Sprintf("Lucky 已经在收集中：当前 %d 段。", status.SegmentCount))
		}
		if err != nil {
			return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, "Lucky 开启失败："+err.Error())
		}
		return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, "Lucky 已开启。发送 /lucky off 提交。")
	case luckycollector.LuckyActionOff:
		batch, err := lucky.Finish(key)
		if errors.Is(err, luckycollector.ErrInactive) {
			return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, "当前没有正在进行的 Lucky 收集。")
		}
		if errors.Is(err, luckycollector.ErrEmptyBatch) {
			return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, "没有收集到消息，已退出 Lucky 收集模式。")
		}
		if err != nil {
			return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, "Lucky 提交失败："+err.Error())
		}
		return sendPlainGatewayInput(ctx, platform, runtime, gw, msg, agent.UserTurnInputFromTurn(batch.UserTurnInput()))
	case luckycollector.LuckyActionCancel:
		status, ok := lucky.Cancel(key)
		if !ok {
			return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, "当前没有正在进行的 Lucky 收集。")
		}
		return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, fmt.Sprintf("已取消 Lucky 收集，丢弃 %d 段消息。", status.SegmentCount))
	case luckycollector.LuckyActionStatus:
		status := lucky.Status(key)
		if !status.Active {
			return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, "Lucky 未开启。发送 /lucky on 开始收集。")
		}
		return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, fmt.Sprintf("Lucky 正在收集：%d 段，附件 %d 个。", status.SegmentCount, status.AttachmentCount))
	default:
		return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, "用法：/lucky on | /lucky off | /lucky status | /lucky cancel")
	}
}

func sendPlainGatewayInput(ctx context.Context, platform string, runtime *agent.Agent, gw gateway.Gateway, msg *gateway.Message, input agent.UserTurnInput) error {
	sessionID := platform + ":" + msg.Chat.ID
	if sessions := runtime.Sessions(); sessions != nil {
		sessions.Ensure(sessionID)
	}
	reply, err := runtime.ChatWithSessionInput(ctx, sessionID, input)
	if err != nil {
		return gw.Send(ctx, msg.Chat.ID, "处理消息失败："+err.Error())
	}
	return gw.SendWithReply(ctx, msg.Chat.ID, msg.ID, reply)
}
