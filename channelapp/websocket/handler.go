package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/config"
	"github.com/yurika0211/luckyagent-runtime/gateway"
	luckycollector "github.com/yurika0211/luckyagent-runtime/gateway/collector"
	"github.com/yurika0211/luckyagent-runtime/logger"
	"github.com/yurika0211/luckyagent-runtime/provider"
	"github.com/yurika0211/luckyagent-runtime/session"
	"github.com/yurika0211/luckyagent-runtime/tool"
)

type agentRuntime interface {
	Chat(ctx context.Context, userInput string) (string, error)
	ChatWithSession(ctx context.Context, sessionID, userInput string) (string, error)
	ChatWithSessionStream(ctx context.Context, sessionID, userInput string) (<-chan agent.ChatEvent, error)
	ChatWithSessionInput(ctx context.Context, sessionID string, input agent.UserTurnInput) (string, error)
	ChatWithSessionStreamInput(ctx context.Context, sessionID string, input agent.UserTurnInput) (<-chan agent.ChatEvent, error)
	Sessions() *session.Manager
	Tools() *tool.Registry
	RespondApprovalWithInput(ctx context.Context, providerName, approvalID, decision, input string) (tool.PendingApproval, error)
}

type runtimeConfigProvider interface {
	Config() *config.Manager
}

type progressFeedbackRuntime interface {
	ProgressFeedbackWithPrompt(ctx context.Context, userInput string, round int, observations []string, presentationPrompt string) (string, error)
}

type queuedRun struct {
	id       string
	parentID string
	data     ChatData
	resume   bool
	client   *Client
	// Bind an accepted turn to its original handle. A deleted session ID may
	// later be reused, but queued work must not create or adopt that new session.
	session *session.Session
	// Protected by AgentHandler.mu. Storage failures must win over cancellation
	// caused by stopping an execution whose output could not be persisted.
	persistErr error
}

type sessionRunner struct {
	queue     []*queuedRun
	active    *queuedRun
	cancel    context.CancelFunc
	cancelled bool
}

type clientReplayLock struct {
	mu    sync.Mutex
	users int
}

// AgentHandler 将 WebSocket 消息桥接到 Agent Loop
type AgentHandler struct {
	agent     agentRuntime
	pending   map[string]context.CancelFunc // sessionID → cancel
	done      map[string]chan struct{}      // sessionID → handler goroutine completion
	runners   map[string]*sessionRunner
	store     *runStore
	eventSink func(string, *Message)
	lucky     *luckycollector.Lucky
	started   bool
	replays   map[*Client]*clientReplayLock
	mu        sync.Mutex
}

// NewAgentHandler 创建 Agent 消息处理器
func NewAgentHandler(a agentRuntime) *AgentHandler {
	storeRoot := ""
	if provider, ok := a.(runtimeConfigProvider); ok && provider.Config() != nil {
		storeRoot = filepath.Join(provider.Config().HomeDir(), "runtime", "websocket")
	}
	return &AgentHandler{
		agent:   a,
		pending: make(map[string]context.CancelFunc),
		done:    make(map[string]chan struct{}),
		runners: make(map[string]*sessionRunner),
		store:   newRunStore(storeRoot),
		lucky:   luckycollector.NewLucky(),
	}
}

// SetEventSink makes agent output independent from the client that submitted it.
// The HTTP server wires this to Hub.SendToSession so reconnecting clients receive
// the same durable stream.
func (h *AgentHandler) SetEventSink(sink func(string, *Message)) {
	h.mu.Lock()
	h.eventSink = sink
	h.mu.Unlock()
}

// Start restores queued and interrupted runs after the runtime process starts.
func (h *AgentHandler) Start() {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return
	}
	h.started = true
	h.mu.Unlock()

	for _, persisted := range h.store.Restoreable() {
		data := ChatData{
			Message:     persisted.Message,
			Stream:      persisted.Stream,
			MaxIter:     persisted.MaxIter,
			ProfileID:   persisted.ProfileID,
			Attachments: persisted.Attachments,
		}
		run := &queuedRun{id: persisted.ID, parentID: persisted.ParentID, data: data}
		if h.agent != nil && h.agent.Sessions() != nil {
			run.session = h.agent.Sessions().Ensure(persisted.SessionID)
		}
		if persisted.TaskID != "" {
			run.data.Message = "继续前台任务 " + persisted.TaskID
			run.data.Attachments = nil
			run.resume = true
		}
		h.mu.Lock()
		runner := h.runners[persisted.SessionID]
		if runner == nil {
			runner = &sessionRunner{}
			h.runners[persisted.SessionID] = runner
		}
		runner.queue = append(runner.queue, run)
		h.mu.Unlock()
	}
	for sessionID := range h.runners {
		h.startNext(sessionID)
	}
}

// HandleMessage 处理来自 WebSocket 客户端的消息
func (h *AgentHandler) HandleMessage(client *Client, msg *Message) {
	switch msg.Type {
	case TypeChat:
		h.handleChat(client, msg)
	case TypeLucky:
		h.handleLucky(client, msg)
	case TypeCancel:
		h.handleCancel(client, msg)
	case TypeApprovalResponse:
		h.handleApprovalResponse(client, msg)
	case TypeReconnect:
		h.HandleReconnect(client, msg)
	case TypeStreamAck:
		// 流式确认，暂不处理
		logger.Debug("stream ack received", "client_id", client.ID, "msg_id", msg.ID)
	default:
		logger.Warn("unknown message type", "type", msg.Type, "client_id", client.ID)
	}
}

func (h *AgentHandler) handleApprovalResponse(client *Client, msg *Message) {
	var data ApprovalResponseData
	if err := msg.ParseData(&data); err != nil {
		errMsg, _ := NewMessage(TypeError, client.SessionID, ErrorData{Code: "INVALID_DATA", Message: "invalid approval response: " + err.Error()})
		client.TrySend(errMsg)
		return
	}
	if h.agent == nil {
		errMsg, _ := NewMessage(TypeError, client.SessionID, ErrorData{Code: "UNAVAILABLE", Message: "agent unavailable"})
		client.TrySend(errMsg)
		return
	}
	resolved, err := h.agent.RespondApprovalWithInput(context.Background(), data.Provider, data.RequestID, data.Decision, data.Input)
	if err != nil {
		errMsg, _ := NewMessage(TypeError, client.SessionID, ErrorData{Code: "APPROVAL_FAILED", Message: "resolve approval failed: " + err.Error()})
		client.TrySend(errMsg)
		return
	}
	status, _ := NewMessage(TypeStatus, client.SessionID, StatusData{
		State:   "approval_resolved",
		Message: resolved.Summary,
	})
	client.TrySend(status)
}

func firstNonEmptyWS(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// HandleReconnect replays events after the supplied cursor. Without a usable
// cursor it only replays events belonging to runs that are still active.
func (h *AgentHandler) HandleReconnect(client *Client, msg *Message) {
	unlock := h.lockClientReplay(client)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.BeginReplay(); err != nil {
		client.Close()
		return
	}
	delivered := make(map[string]bool)
	defer func() {
		if err := client.EndReplay(ctx, delivered); err != nil {
			client.Close()
		}
	}()
	send := func(event *Message) bool {
		if err := client.SendReliable(ctx, event); err != nil {
			client.Close()
			return false
		}
		if event.EventID != "" {
			delivered[event.EventID] = true
		} else if event.ID != "" {
			delivered[event.ID] = true
		}
		return true
	}
	var data ReconnectData
	if len(msg.Data) > 0 && msg.ParseData(&data) != nil {
		data.LastMessageID = ""
	}
	events, err := h.store.ReplayForReconnectWithError(client.SessionID, strings.TrimSpace(data.LastMessageID))
	if err != nil {
		h.sendChatError(client, msg.ID, "RECOVERY_FAILED", "could not load durable session events")
		return
	}
	for _, event := range events {
		if !send(replayed(event)) {
			return
		}
	}
	for _, run := range h.store.ActiveRuns(client.SessionID) {
		status, _ := NewMessage(TypeStatus, client.SessionID, StatusData{State: runActivityState(run.State), Message: runActivityMessage(run.State)})
		status = withReplay(status)
		status.ParentID = run.ParentID
		status.RunID = run.ID
		status.ID = ""
		status.EventID = ""
		if !send(status) {
			return
		}
	}
	status, _ := NewMessage(TypeStatus, client.SessionID, StatusData{
		State:   "connected",
		Message: "reconnected",
	})
	status.ID = ""
	status.EventID = ""
	if !send(status) {
		return
	}
	logger.Info("client reconnecting", "client_id", client.ID, "last_msg", data.LastMessageID)
}

// handleCancel 取消指定 session 的进行中请求。客户端 Stop 必须走这条路径，
// 只关 WebSocket 不会停止已经跑起来的 agent turn。
func (h *AgentHandler) handleCancel(client *Client, msg *Message) {
	var data CancelData
	if len(msg.Data) > 0 {
		if err := msg.ParseData(&data); err != nil {
			errMsg, _ := NewMessage(TypeError, client.SessionID, ErrorData{
				Code:    "INVALID_DATA",
				Message: fmt.Sprintf("invalid cancel data: %v", err),
			})
			client.TrySend(errMsg)
			return
		}
	}
	sessionID := strings.TrimSpace(data.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(msg.SessionID)
	}
	if sessionID == "" && client != nil {
		sessionID = client.SessionID
	}
	if sessionID == "" {
		errMsg, _ := NewMessage(TypeError, "", ErrorData{
			Code:    "INVALID_DATA",
			Message: "cancel requires a session id",
		})
		if client != nil {
			client.TrySend(errMsg)
		}
		return
	}
	cancelled := h.cancelSession(sessionID)
	if len(cancelled) == 0 {
		status, _ := NewMessage(TypeStatus, sessionID, StatusData{
			State:   "cancelled",
			Message: "cancelled",
		})
		if msg != nil {
			status.ParentID = msg.ID
		}
		h.emit(client, sessionID, "", status)
	} else {
		for _, run := range cancelled {
			status, _ := NewMessage(TypeStatus, sessionID, StatusData{
				State:   "cancelled",
				Message: "cancelled",
			})
			status.ParentID = run.parentID
			status.RunID = run.id
			h.emit(client, sessionID, run.id, status)
		}
	}
	logger.Info("session cancelled", "session", sessionID)
}

// handleChat 处理聊天消息
func (h *AgentHandler) luckyCollector() *luckycollector.Lucky {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lucky == nil {
		h.lucky = luckycollector.NewLucky()
	}
	return h.lucky
}

func luckySessionKey(sessionID string) string {
	return "websocket|session:" + strings.TrimSpace(sessionID)
}

func (h *AgentHandler) emitLucky(client *Client, parentID, text string) {
	status, _ := NewMessage(TypeStatus, client.SessionID, StatusData{State: "lucky", Message: text})
	status.ParentID = parentID
	status.ID = ""
	status.EventID = ""
	h.emit(client, client.SessionID, "", status)
}

func (h *AgentHandler) handleLucky(client *Client, msg *Message) {
	var data LuckyData
	if len(msg.Data) > 0 {
		if err := msg.ParseData(&data); err != nil {
			errMsg, _ := NewMessage(TypeError, client.SessionID, ErrorData{Code: "INVALID_DATA", Message: fmt.Sprintf("invalid lucky data: %v", err)})
			client.TrySend(errMsg)
			return
		}
	}
	sessionID := strings.TrimSpace(msg.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(client.SessionID)
	}
	if sessionID == "" {
		errMsg, _ := NewMessage(TypeError, client.SessionID, ErrorData{Code: "INVALID_DATA", Message: "lucky requires a session id"})
		client.TrySend(errMsg)
		return
	}
	key := luckySessionKey(sessionID)
	collector := h.luckyCollector()
	switch luckycollector.ParseLuckyAction(data.Action) {
	case luckycollector.LuckyActionOn:
		status, err := collector.Start(key)
		if errors.Is(err, luckycollector.ErrAlreadyActive) {
			h.emitLucky(client, msg.ID, fmt.Sprintf("Lucky 已经在收集中：当前 %d 段，附件 %d 个。发送 /lucky off 提交，/lucky cancel 放弃。", status.SegmentCount, status.AttachmentCount))
			return
		}
		if err != nil {
			h.emitLucky(client, msg.ID, "Lucky 开启失败："+err.Error())
			return
		}
		h.emitLucky(client, msg.ID, "Lucky 已开启。接下来发送的多段消息会先被收集；发送 /lucky off 后再统一交给 agent。")
	case luckycollector.LuckyActionStatus:
		status := collector.Status(key)
		if !status.Active {
			h.emitLucky(client, msg.ID, "Lucky 未开启。发送 /lucky on 开始收集多段消息。")
			return
		}
		h.emitLucky(client, msg.ID, fmt.Sprintf("Lucky 正在收集：%d 段，附件 %d 个。发送 /lucky off 提交，/lucky cancel 放弃。", status.SegmentCount, status.AttachmentCount))
	case luckycollector.LuckyActionCancel:
		status, ok := collector.Cancel(key)
		if !ok {
			h.emitLucky(client, msg.ID, "当前没有正在进行的 Lucky 收集。")
			return
		}
		h.emitLucky(client, msg.ID, fmt.Sprintf("已取消 Lucky 收集，丢弃 %d 段消息和 %d 个附件。", status.SegmentCount, status.AttachmentCount))
	case luckycollector.LuckyActionOff:
		batch, err := collector.Finish(key)
		if errors.Is(err, luckycollector.ErrInactive) {
			h.emitLucky(client, msg.ID, "当前没有正在进行的 Lucky 收集。发送 /lucky on 开始。")
			return
		}
		if errors.Is(err, luckycollector.ErrEmptyBatch) {
			h.emitLucky(client, msg.ID, "没有收集到消息，已退出 Lucky 收集模式。")
			return
		}
		if err != nil {
			h.emitLucky(client, msg.ID, "Lucky 提交失败："+err.Error())
			return
		}
		h.emitLucky(client, msg.ID, fmt.Sprintf("Lucky 已提交 %d 段消息。", len(batch.Segments)))
		input := agent.UserTurnInputFromTurn(batch.UserTurnInput())
		chat := &Message{
			Type:      TypeChat,
			SessionID: sessionID,
			ID:        msg.ID,
			Data: mustChatData(ChatData{
				Message:     input.RoutingText,
				Stream:      true,
				Attachments: batch.Attachments(),
			}),
		}
		h.handleChat(client, chat)
	default:
		h.emitLucky(client, msg.ID, "用法：/lucky on | /lucky off | /lucky status | /lucky cancel")
	}
}

func mustChatData(data ChatData) json.RawMessage {
	raw, err := json.Marshal(data)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

func (h *AgentHandler) handleChat(client *Client, msg *Message) {
	var data ChatData
	if err := msg.ParseData(&data); err != nil {
		errMsg, _ := NewMessage(TypeError, client.SessionID, ErrorData{
			Code:    "INVALID_DATA",
			Message: fmt.Sprintf("invalid chat data: %v", err),
		})
		client.TrySend(errMsg)
		return
	}
	if data.MaxIter < 0 || data.MaxIter > 300 || (strings.TrimSpace(data.Message) == "" && len(data.Attachments) == 0) {
		h.sendChatError(client, msg.ID, "INVALID_DATA", "message or attachments are required and max_iterations must be between 0 and 300")
		return
	}
	if data.ProfileID != "" {
		h.sendChatError(client, msg.ID, "UNSUPPORTED_PARAMETER", "profile_id is not supported for a per-request model selection; use the model profile API")
		return
	}
	if data.MaxIter > 0 {
		if _, ok := h.agent.(loopConfigRuntime); !ok {
			h.sendChatError(client, msg.ID, "UNSUPPORTED_PARAMETER", "this runtime cannot apply max_iterations")
			return
		}
	}
	if h.agent == nil {
		h.sendChatError(client, msg.ID, "UNAVAILABLE", "agent runtime unavailable")
		return
	}

	run := &queuedRun{
		id:       msg.ID,
		parentID: msg.ID,
		data:     data,
		client:   client,
	}
	if run.id == "" {
		run.id = generateID()
		run.parentID = run.id
	}
	if h.collectLuckyChat(client, msg, data) {
		return
	}
	persisted := persistedRun{
		ID:          run.id,
		SessionID:   client.SessionID,
		ParentID:    run.parentID,
		Message:     data.Message,
		Stream:      data.Stream,
		MaxIter:     data.MaxIter,
		ProfileID:   data.ProfileID,
		Attachments: data.Attachments,
		State:       "queued",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	// Capture an existing handle before durable acceptance, so a concurrent
	// deletion during the store write cannot make this turn recreate its ID.
	if sessions := h.agent.Sessions(); sessions != nil {
		run.session, _ = sessions.Get(client.SessionID)
	}
	stored, created, err := h.store.CreateRun(persisted)
	if err != nil {
		logger.Error("persist websocket run failed", "session", client.SessionID, "error", err)
		code := "PERSIST_FAILED"
		if errors.Is(err, errRunConflict) {
			code = "REQUEST_CONFLICT"
		}
		h.sendChatError(client, run.parentID, code, err.Error())
		return
	}
	if !created {
		h.replayAcceptedRun(client, stored)
		return
	}
	if run.session == nil && h.agent.Sessions() != nil {
		run.session = h.agent.Sessions().Ensure(client.SessionID)
	}
	h.mu.Lock()
	runner := h.runners[client.SessionID]
	if runner == nil {
		runner = &sessionRunner{}
		h.runners[client.SessionID] = runner
	}
	wasBusy := runner.active != nil || len(runner.queue) > 0
	runner.queue = append(runner.queue, run)
	h.mu.Unlock()
	queued, _ := NewMessage(TypeStatus, client.SessionID, StatusData{
		State:   "queued",
		Message: map[bool]string{true: "message queued", false: "processing your message"}[wasBusy],
	})
	queued.ParentID = run.parentID
	queued.RunID = run.id
	h.emit(client, client.SessionID, run.id, queued)
	if !wasBusy {
		h.startNext(client.SessionID)
	}
}

func (h *AgentHandler) collectLuckyChat(client *Client, msg *Message, data ChatData) bool {
	sessionID := strings.TrimSpace(client.SessionID)
	if sessionID == "" {
		return false
	}
	collector := h.luckyCollector()
	key := luckySessionKey(sessionID)
	if !collector.Status(key).Active {
		return false
	}
	segment := &gateway.Message{
		ID:          msg.ID,
		Text:        data.Message,
		Chat:        gateway.Chat{ID: sessionID, Type: gateway.ChatPrivate},
		Attachments: data.Attachments,
	}
	status, err := collector.Append(key, segment)
	if err != nil {
		h.emitLucky(client, msg.ID, "这段没有送进 Lucky 收集："+err.Error())
		return true
	}
	h.emitLucky(client, msg.ID, fmt.Sprintf("已收集第 %d 段，附件 %d 个。发送 /lucky off 后一次提交。", status.SegmentCount, status.AttachmentCount))
	return true
}

func (h *AgentHandler) startNext(sessionID string) {
	h.mu.Lock()
	runner := h.runners[sessionID]
	if runner == nil || runner.active != nil || len(runner.queue) == 0 {
		h.mu.Unlock()
		return
	}
	run := runner.queue[0]
	runner.queue = runner.queue[1:]
	runner.active = run
	ctx, cancel := context.WithCancel(context.Background())
	runner.cancel = cancel
	runner.cancelled = false
	h.pending[sessionID] = cancel
	if h.done[sessionID] == nil {
		h.done[sessionID] = make(chan struct{})
	}
	h.mu.Unlock()
	go func() {
		defer cancel()
		var err error
		client := run.client
		if client == nil {
			client = &Client{SessionID: sessionID, Send: make(chan *Message, 1)}
		}
		if err = h.runPersistenceError(sessionID, run.id); err != nil {
			h.finishRun(sessionID, run, err)
			return
		}
		if err = h.store.UpdateRun(sessionID, run.id, func(record *persistedRun) { record.State = "running" }); err != nil {
			h.failPersistence(client, sessionID, run.id, run.parentID, err)
			h.finishRun(sessionID, run, err)
			return
		}
		if run.data.Stream {
			err = h.streamChatRun(ctx, client, run.data, run.parentID, run.id)
		} else {
			err = h.syncChatRun(ctx, client, run.data, run.parentID, run.id)
		}
		h.finishRun(sessionID, run, err)
	}()
}

func (h *AgentHandler) finishRun(sessionID string, run *queuedRun, err error) {
	h.mu.Lock()
	runner := h.runners[sessionID]
	if runner == nil || runner.active != run {
		h.mu.Unlock()
		return
	}
	wasCancelled := runner.cancelled
	persistErr := run.persistErr
	h.mu.Unlock()
	state := "completed"
	if persistErr != nil {
		state = "error"
	} else if wasCancelled || errors.Is(err, context.Canceled) {
		state = "cancelled"
	} else if err != nil {
		state = "error"
	}
	if updateErr := h.store.UpdateRun(sessionID, run.id, func(record *persistedRun) { record.State = state }); updateErr != nil {
		h.failPersistence(run.client, sessionID, run.id, run.parentID, updateErr)
	} else if persistErr == nil {
		// The run record is already terminal. Losing this frame must not leave
		// the activity query saying the session is still working.
		h.emit(run.client, sessionID, run.id, terminalStatus(sessionID, run.parentID, run.id, state))
	}
	h.mu.Lock()
	runner.active = nil
	runner.cancel = nil
	runner.cancelled = false
	delete(h.pending, sessionID)
	if len(runner.queue) == 0 {
		if done := h.done[sessionID]; done != nil {
			close(done)
			delete(h.done, sessionID)
		}
	}
	h.mu.Unlock()
	h.startNext(sessionID)
}

// runLocked finds both active and accepted queued runs. Queued status events are
// persisted before startNext, so their write failures must also prevent execution.
func (h *AgentHandler) runLocked(sessionID, runID string) *queuedRun {
	if runID == "" {
		return nil
	}
	if runner := h.runners[sessionID]; runner != nil {
		if runner.active != nil && runner.active.id == runID {
			return runner.active
		}
		for _, run := range runner.queue {
			if run.id == runID {
				return run
			}
		}
	}
	return nil
}

func (h *AgentHandler) runPersistenceError(sessionID, runID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if run := h.runLocked(sessionID, runID); run != nil {
		return run.persistErr
	}
	return nil
}

func (h *AgentHandler) failPersistence(client *Client, sessionID, runID, parentID string, err error) {
	h.mu.Lock()
	var cancel context.CancelFunc
	if run := h.runLocked(sessionID, runID); run != nil {
		if run.persistErr != nil {
			h.mu.Unlock()
			return
		}
		run.persistErr = err
		if runner := h.runners[sessionID]; runner.active == run {
			cancel = runner.cancel
		}
	}
	sink := h.eventSink
	h.mu.Unlock()
	logger.Error("persist websocket run failed", "session", sessionID, "run_id", runID, "error", err)
	if cancel != nil {
		cancel()
	}
	// This notification cannot use emit: the same failing store must not hide
	// the error or recursively try to persist it. It has no durable replay cursor.
	msg, _ := NewMessage(TypeError, sessionID, ErrorData{Code: "PERSIST_FAILED", Message: "run persistence failed; execution stopped"})
	msg.RunID, msg.ParentID = runID, parentID
	msg.ID, msg.EventID = "", ""
	// The terminal frame is best-effort and not replayable. emit would write
	// through the same failing store and could hide the original failure.
	// The activity query still reads the terminal record written before this call.
	terminal := terminalStatus(sessionID, parentID, runID, "error")
	terminal.ID, terminal.EventID = "", ""
	if sink != nil {
		sink(sessionID, msg)
		sink(sessionID, terminal)
	} else if client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.SendReliable(ctx, msg); err != nil {
			client.Close()
			return
		}
		if err := client.SendReliable(ctx, terminal); err != nil {
			client.Close()
		}
	}
}

func (h *AgentHandler) emit(client *Client, sessionID, runID string, msg *Message) {
	if msg == nil || h.runPersistenceError(sessionID, runID) != nil {
		return
	}
	msg.SessionID = sessionID
	if runID != "" {
		msg.RunID = runID
	}
	if err := h.store.AppendEvent(sessionID, runID, msg); err != nil {
		h.failPersistence(client, sessionID, runID, msg.ParentID, err)
		return
	}
	h.mu.Lock()
	sink := h.eventSink
	h.mu.Unlock()
	if sink != nil {
		sink(sessionID, msg)
		return
	}
	if client != nil {
		client.TrySend(msg)
	}
}

// syncChat 同步聊天（等待完整响应）
func (h *AgentHandler) syncChat(ctx context.Context, client *Client, data ChatData, parentID string) {
	_ = h.syncChatRun(ctx, client, data, parentID, "")
}

func (h *AgentHandler) syncChatRun(ctx context.Context, client *Client, data ChatData, parentID, runID string) error {
	// 发送 executing 状态
	status, _ := NewMessage(TypeStatus, client.SessionID, StatusData{
		State:   "running",
		Message: "agent is running",
	})
	status.ParentID = parentID
	h.emit(client, client.SessionID, runID, status)
	if ctx.Err() != nil {
		h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "cancelled"))
		return ctx.Err()
	}

	sess, err := h.sessionForRun(ctx, client.SessionID, runID)
	if err != nil {
		if ctx.Err() == nil {
			h.sendSessionHistoryError(client, runID, parentID, err)
		} else {
			h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "cancelled"))
		}
		return err
	}
	sessionID := client.SessionID
	if sess != nil {
		sessionID = sess.ID
	}
	turn := agent.MultimodalUserTurnInput(data.Message, data.Attachments)
	result, iterations, err := h.syncTurn(ctx, sess, sessionID, turn, data)
	if err != nil {
		if ctx.Err() != nil {
			h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "cancelled"))
			return ctx.Err()
		}
		if errors.Is(err, session.ErrSessionDeleted) {
			h.sendSessionHistoryError(client, runID, parentID, err)
			return err
		}
		errMsg, _ := NewMessage(TypeError, client.SessionID, ErrorData{
			Code:    "AGENT_ERROR",
			Message: err.Error(),
		})
		errMsg.ParentID = parentID
		h.emit(client, client.SessionID, runID, errMsg)
		h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "error"))
		return err
	}
	var createdAt *time.Time
	var usage *provider.TokenUsage
	if sessions := h.agent.Sessions(); sessions != nil {
		ok := sess != nil
		if !ok {
			sess, ok = sessions.Get(sessionID)
		}
		var messages []provider.Message
		var historyErr error
		if !ok {
			historyErr = fmt.Errorf("%w: %s", session.ErrSessionDeleted, sessionID)
		} else {
			messages, historyErr = sess.GetMessagesWithError()
		}
		if historyErr != nil {
			h.sendSessionHistoryError(client, runID, parentID, historyErr)
			return historyErr
		}
		if len(messages) > 0 && messages[len(messages)-1].Role == "assistant" {
			createdAt = messages[len(messages)-1].CreatedAt
			usage = messages[len(messages)-1].Usage
		}
	}

	// 发送完整响应
	response, responseAttachments := h.attachmentsFromResponse(result)
	endMsg, _ := NewMessage(TypeStreamEnd, client.SessionID, StreamEndData{
		FullResponse: response,
		Iterations:   iterations,
		CreatedAt:    createdAt,
		Usage:        provider.ToTurnTokenUsage(usage),
		Attachments:  appendUniqueAttachments(h.attachmentsFromToolResult("", result), responseAttachments...),
	})
	endMsg.ParentID = parentID
	h.emit(client, client.SessionID, runID, endMsg)

	h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "completed"))
	return nil
}

// streamChat 流式聊天（逐块推送）
func (h *AgentHandler) streamChat(ctx context.Context, client *Client, data ChatData, parentID string) {
	_ = h.streamChatRun(ctx, client, data, parentID, "")
}

func (h *AgentHandler) streamChatRun(ctx context.Context, client *Client, data ChatData, parentID, runID string) error {
	// 发送 executing 状态
	status, _ := NewMessage(TypeStatus, client.SessionID, StatusData{
		State:   "running",
		Message: "agent is streaming",
	})
	status.ParentID = parentID
	h.emit(client, client.SessionID, runID, status)
	if ctx.Err() != nil {
		h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "cancelled"))
		return ctx.Err()
	}

	sess, err := h.sessionForRun(ctx, client.SessionID, runID)
	if err != nil {
		if ctx.Err() == nil {
			h.sendSessionHistoryError(client, runID, parentID, err)
		} else {
			h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "cancelled"))
		}
		return err
	}
	sessionID := client.SessionID
	if sess != nil {
		sessionID = sess.ID
	}
	turn := agent.MultimodalUserTurnInput(data.Message, data.Attachments)
	var streamCh <-chan agent.ChatEvent
	if runtime, ok := h.agent.(boundStreamRuntime); ok && sess != nil {
		streamCh, err = runtime.ChatWithSessionStreamForSession(ctx, sess, turn, h.chatLoopConfig(data))
	} else if runtime, ok := h.agent.(loopConfigRuntime); ok {
		streamCh, err = runtime.ChatWithSessionStreamInputWithLoopConfig(ctx, sessionID, turn, h.chatLoopConfig(data))
	} else {
		streamCh, err = h.agent.ChatWithSessionStreamInput(ctx, sessionID, turn)
	}
	if err != nil {
		if ctx.Err() != nil {
			h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "cancelled"))
			return ctx.Err()
		}
		if errors.Is(err, session.ErrSessionDeleted) {
			h.sendSessionHistoryError(client, runID, parentID, err)
			return err
		}
		errMsg, _ := NewMessage(TypeError, client.SessionID, ErrorData{
			Code:    "AGENT_ERROR",
			Message: err.Error(),
		})
		errMsg.ParentID = parentID
		h.emit(client, client.SessionID, runID, errMsg)
		h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "error"))
		return err
	}

	var fullResponse strings.Builder
	currentRound := 0
	toolSeq := 0
	var pendingSteps []toolStepState
	var turnAttachments []gateway.Attachment
	progressSummaryEnabled, progressSummaryPrompt := h.progressSummaryConfig()
	var roundObservations []string
	var progressHistory []string
	lastProgress := ""

	emitRoundProgress := func(round int) bool {
		if !progressSummaryEnabled || len(roundObservations) == 0 {
			return false
		}
		runtime, ok := h.agent.(progressFeedbackRuntime)
		if !ok {
			return false
		}
		observations := append([]string(nil), roundObservations...)
		for _, previous := range progressHistory {
			if previous = strings.TrimSpace(previous); previous != "" {
				observations = append([]string{"Previous user-facing update: " + previous}, observations...)
			}
		}
		summaryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		summary, err := runtime.ProgressFeedbackWithPrompt(summaryCtx, data.Message, round, observations, progressSummaryPrompt)
		cancel()
		if err != nil {
			return false
		}
		summary = strings.TrimSpace(summary)
		if summary == "" || summary == lastProgress {
			return false
		}
		msg, _ := NewMessage(TypeReasoning, client.SessionID, ReasoningData{
			Summary: summary,
			Round:   round,
			Stage:   "progress",
		})
		msg.ParentID = parentID
		h.emit(client, client.SessionID, runID, msg)
		lastProgress = summary
		progressHistory = append(progressHistory, summary)
		return true
	}

	sendError := func(err error) {
		if err == nil {
			err = errors.New("agent stream failed")
		}
		errMsg, _ := NewMessage(TypeError, client.SessionID, ErrorData{
			Code:    "AGENT_ERROR",
			Message: err.Error(),
		})
		errMsg.ParentID = parentID
		h.emit(client, client.SessionID, runID, errMsg)
		h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "error"))
	}

	for {
		var evt agent.ChatEvent
		select {
		case <-ctx.Done():
			h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "cancelled"))
			return ctx.Err()
		case next, ok := <-streamCh:
			if !ok {
				if ctx.Err() != nil {
					h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "cancelled"))
					return ctx.Err()
				}
				err = errors.New("agent stream ended without a completed result")
				sendError(err)
				return err
			}
			evt = next
		}
		if ctx.Err() != nil {
			h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "cancelled"))
			return ctx.Err()
		}
		switch evt.Type {
		case agent.ChatEventThinking:
			if evt.TaskID != "" && runID != "" {
				if err := h.store.UpdateRun(client.SessionID, runID, func(record *persistedRun) { record.TaskID = evt.TaskID }); err != nil {
					h.failPersistence(client, client.SessionID, runID, parentID, err)
					return err
				}
			}
			reasoning, ok := reasoningDataForEvent(evt.Content)
			if !ok {
				continue
			}
			if reasoning.Round > currentRound {
				emittedSummary := false
				if currentRound > 0 {
					emittedSummary = emitRoundProgress(currentRound)
				}
				roundObservations = nil
				currentRound = reasoning.Round
				if emittedSummary {
					continue
				}
			}
			msg, _ := NewMessage(TypeReasoning, client.SessionID, reasoning)
			msg.ParentID = parentID
			h.emit(client, client.SessionID, runID, msg)

		case agent.ChatEventReasoningContent:
			if currentRound == 0 {
				currentRound = evt.Round
			}
			msg, _ := NewMessage(TypeReasoning, client.SessionID, ReasoningData{
				Content: evt.Content,
				Round:   evt.Round,
				Stage:   "content",
			})
			msg.ParentID = parentID
			h.emit(client, client.SessionID, runID, msg)

		case agent.ChatEventApprovalRequired:
			data := ApprovalRequestData{
				Provider: "runtime",
				Summary:  evt.Content,
				Tool:     evt.Name,
				Kind:     "approval",
			}
			if evt.Approval != nil {
				data.RequestID = evt.Approval.RequestID
				data.Tool = firstNonEmptyWS(evt.Approval.Tool, evt.Name)
				data.Action = evt.Approval.Action
				data.Reason = evt.Approval.Reason
				data.Prompt = evt.Approval.Prompt
				data.Secure = evt.Approval.Secure
				data.Fields = evt.Approval.Fields
				if evt.Approval.Kind != "" {
					data.Kind = evt.Approval.Kind
				}
			}
			msg, _ := NewMessage(TypeApproval, client.SessionID, data)
			msg.ParentID = parentID
			h.emit(client, client.SessionID, runID, msg)

		case agent.ChatEventToolCall:
			if currentRound == 0 {
				currentRound = 1
			}
			toolSeq++
			groupID := fmt.Sprintf("round-%d", currentRound)
			step := toolStepState{
				Name:       evt.Name,
				GroupID:    groupID,
				StepID:     fmt.Sprintf("%s-tool-%d", groupID, toolSeq),
				Visibility: h.toolVisibility(evt.Name),
			}
			pendingSteps = append(pendingSteps, step)
			if progressSummaryEnabled {
				roundObservations = append(roundObservations, fmt.Sprintf("Tool call: %s", formatToolCallDisplay(evt.Name, evt.Args)))
			}
			msg, _ := NewMessage(TypeToolCall, client.SessionID, ToolCallData{
				Name:       evt.Name,
				Params:     parseToolParams(evt.Args),
				Args:       evt.Args,
				Display:    formatToolCallDisplay(evt.Name, evt.Args),
				Phase:      "start",
				Round:      currentRound,
				GroupID:    step.GroupID,
				StepID:     step.StepID,
				Visibility: step.Visibility,
			})
			msg.ParentID = parentID
			h.emit(client, client.SessionID, runID, msg)

		case agent.ChatEventToolResult:
			if currentRound == 0 {
				currentRound = 1
			}
			step := matchPendingToolStep(&pendingSteps, evt.Name, currentRound)
			attachments := h.attachmentsFromToolResult(evt.Name, evt.Result)
			turnAttachments = appendUniqueAttachments(turnAttachments, attachments...)
			if progressSummaryEnabled {
				result := strings.TrimSpace(evt.Result)
				if len(result) > 600 {
					result = result[:600] + "..."
				}
				roundObservations = append(roundObservations, fmt.Sprintf("Tool result (%s): %s", evt.Name, result))
			}
			msg, _ := NewMessage(TypeToolResult, client.SessionID, ToolResultData{
				Name:        evt.Name,
				Success:     !looksLikeToolError(evt.Result),
				Output:      evt.Result,
				Display:     formatToolResultDisplay(evt.Name, evt.Result),
				Round:       currentRound,
				GroupID:     step.GroupID,
				StepID:      step.StepID,
				Visibility:  step.Visibility,
				Attachments: attachments,
				DurationMS:  evt.Duration.Milliseconds(),
			})
			msg.ParentID = parentID
			h.emit(client, client.SessionID, runID, msg)

		case agent.ChatEventUIDelta:
			if evt.UI == nil || len(evt.UI.Nodes) == 0 {
				continue
			}
			nodes := make([]UINodeData, 0, len(evt.UI.Nodes))
			for _, node := range evt.UI.Nodes {
				nodes = append(nodes, UINodeData{ID: node.ID, Type: node.Type, Policy: node.Policy, Props: node.Props, Children: node.Children})
			}
			msg, _ := NewMessage(TypeUIDelta, client.SessionID, UIDeltaData{TreeID: evt.UI.TreeID, State: evt.UI.State, Nodes: nodes, Patch: evt.UI.Patch})
			msg.ParentID = parentID
			h.emit(client, client.SessionID, runID, msg)

		case agent.ChatEventContent:
			fullResponse.WriteString(evt.Content)
			msg, _ := NewMessage(TypeStreamChunk, client.SessionID, StreamChunkData{
				Content: evt.Content,
				Done:    false,
			})
			msg.ParentID = parentID
			h.emit(client, client.SessionID, runID, msg)

		case agent.ChatEventCompact:
			data := CompactData{Phase: "progress", Message: evt.Content}
			if evt.Compact != nil {
				data = CompactData{
					Phase:               evt.Compact.Phase,
					Trigger:             evt.Compact.Trigger,
					Message:             evt.Compact.Message,
					BoundaryID:          evt.Compact.BoundaryID,
					SummarySource:       evt.Compact.SummarySource,
					Chunk:               evt.Compact.Chunk,
					Chunks:              evt.Compact.Chunks,
					PreTokenEstimate:    evt.Compact.PreTokenEstimate,
					PostTokenEstimate:   evt.Compact.PostTokenEstimate,
					DroppedMessages:     evt.Compact.DroppedMessages,
					RetainedMessages:    evt.Compact.RetainedMessages,
					RestoredAttachments: evt.Compact.RestoredAttachments,
					Model:               evt.Compact.Model,
					Error:               evt.Compact.Error,
				}
				if data.Message == "" {
					data.Message = evt.Content
				}
				if data.Phase == "" {
					data.Phase = evt.Name
				}
			}
			if data.Phase == "" {
				data.Phase = "progress"
			}
			msg, _ := NewMessage(TypeCompact, client.SessionID, data)
			msg.ParentID = parentID
			h.emit(client, client.SessionID, runID, msg)

		case agent.ChatEventDone:
			if _, err := h.sessionForRun(ctx, client.SessionID, runID); err != nil {
				if ctx.Err() == nil {
					h.sendSessionHistoryError(client, runID, parentID, err)
				} else {
					h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "cancelled"))
				}
				return err
			}
			emitRoundProgress(max(currentRound, 1))
			if evt.Content != "" {
				fullResponse.Reset()
				fullResponse.WriteString(evt.Content)
			}
			response, responseAttachments := h.attachmentsFromResponse(fullResponse.String())
			turnAttachments = appendUniqueAttachments(turnAttachments, responseAttachments...)
			endMsg, _ := NewMessage(TypeStreamEnd, client.SessionID, StreamEndData{
				FullResponse: response,
				Iterations:   max(currentRound, 1),
				CreatedAt:    evt.CreatedAt,
				Usage:        provider.ToTurnTokenUsage(evt.Usage),
				Attachments:  turnAttachments,
			})
			endMsg.ParentID = parentID
			h.emit(client, client.SessionID, runID, endMsg)
			h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "completed"))
			return nil

		case agent.ChatEventError:
			if ctx.Err() != nil {
				h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "cancelled"))
				return ctx.Err()
			}
			streamErr := evt.Err
			if streamErr == nil {
				streamErr = errors.New("agent stream failed")
			}
			sendError(streamErr)
			return streamErr
		}
	}
}

type toolAttachmentPayload struct {
	Paths       []string             `json:"paths"`
	Path        string               `json:"path"`
	OutputPath  string               `json:"output_path"`
	FilePath    string               `json:"file_path"`
	URL         string               `json:"url"`
	FileURL     string               `json:"file_url"`
	DownloadURL string               `json:"download_url"`
	URLs        []string             `json:"urls"`
	FileURLs    []string             `json:"file_urls"`
	Attachments []gateway.Attachment `json:"attachments"`
}

func (h *AgentHandler) attachmentsFromToolResult(toolName, raw string) []gateway.Attachment {
	var payload toolAttachmentPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil
	}

	fallbackType := gateway.AttachmentDocument
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "image_generate":
		fallbackType = gateway.AttachmentImage
	case "text_to_speech":
		fallbackType = gateway.AttachmentAudio
	}

	attachments := make([]gateway.Attachment, 0, len(payload.Paths)+len(payload.Attachments)+len(payload.URLs)+len(payload.FileURLs)+4)
	for _, attachment := range payload.Attachments {
		if normalized, ok := normalizeRemoteAttachment(attachment, fallbackType); ok {
			attachments = appendUniqueAttachments(attachments, normalized)
			continue
		}
		if normalized, ok := h.attachmentForLocalPath(attachment.FilePath, fallbackType); ok {
			attachments = appendUniqueAttachments(attachments, normalized)
		}
	}
	for _, path := range append([]string{}, payload.Paths...) {
		if attachment, ok := h.attachmentForLocalPath(path, fallbackType); ok {
			attachments = appendUniqueAttachments(attachments, attachment)
		}
	}
	for _, path := range []string{payload.Path, payload.OutputPath, payload.FilePath} {
		if attachment, ok := h.attachmentForLocalPath(path, fallbackType); ok {
			attachments = appendUniqueAttachments(attachments, attachment)
		}
	}
	for _, rawURL := range append(append([]string{}, payload.URLs...), payload.FileURLs...) {
		if attachment, ok := remoteAttachment(rawURL, fallbackType); ok {
			attachments = appendUniqueAttachments(attachments, attachment)
		}
	}
	for _, rawURL := range []string{payload.URL, payload.FileURL, payload.DownloadURL} {
		if attachment, ok := remoteAttachment(rawURL, fallbackType); ok {
			attachments = appendUniqueAttachments(attachments, attachment)
		}
	}
	return attachments
}

func (h *AgentHandler) attachmentsFromResponse(raw string) (string, []gateway.Attachment) {
	references := append(agent.MediaReferences(raw), agent.ArtifactReferences(raw)...)
	if len(references) == 0 {
		return raw, nil
	}

	resolved := make([]string, 0, len(references))
	attachments := make([]gateway.Attachment, 0, len(references))
	for _, reference := range references {
		var attachment gateway.Attachment
		var ok bool
		if strings.HasPrefix(strings.ToLower(reference), "http://") || strings.HasPrefix(strings.ToLower(reference), "https://") {
			attachment, ok = remoteAttachment(reference, gateway.AttachmentDocument)
		} else {
			attachment, ok = h.attachmentForLocalPath(reference, gateway.AttachmentDocument)
		}
		if !ok {
			continue
		}
		attachments = appendUniqueAttachments(attachments, attachment)
		resolved = append(resolved, reference)
	}
	return agent.StripMediaReferences(raw, resolved), attachments
}

func (h *AgentHandler) attachmentForLocalPath(path string, fallbackType gateway.AttachmentType) (gateway.Attachment, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return gateway.Attachment{}, false
	}
	provider, ok := h.agent.(runtimeConfigProvider)
	if !ok || provider.Config() == nil {
		return gateway.Attachment{}, false
	}
	home := strings.TrimSpace(provider.Config().HomeDir())
	if home == "" {
		return gateway.Attachment{}, false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return gateway.Attachment{}, false
	}
	for _, root := range []struct {
		prefix string
		path   string
	}{
		{prefix: "workspace", path: filepath.Join(home, "workspace")},
		{prefix: "uploads", path: filepath.Join(home, "uploads")},
	} {
		rootPath, err := filepath.Abs(root.path)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootPath, absPath)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		info, err := os.Stat(absPath)
		if err != nil || !info.Mode().IsRegular() {
			return gateway.Attachment{}, false
		}
		mimeType := mime.TypeByExtension(filepath.Ext(absPath))
		attachmentType := attachmentTypeForMIME(mimeType, fallbackType)
		return gateway.Attachment{
			Type:     attachmentType,
			FileURL:  "/api/v1/artifacts?path=" + url.QueryEscape(root.prefix+"/"+filepath.ToSlash(rel)),
			FilePath: absPath,
			FileName: filepath.Base(absPath),
			MimeType: mimeType,
			FileSize: info.Size(),
		}, true
	}
	return gateway.Attachment{}, false
}

func remoteAttachment(rawURL string, fallbackType gateway.AttachmentType) (gateway.Attachment, bool) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return gateway.Attachment{}, false
	}
	name := filepath.Base(parsed.Path)
	if name == "." || name == "/" || name == "" {
		name = "attachment"
	}
	mimeType := mime.TypeByExtension(filepath.Ext(name))
	return gateway.Attachment{
		Type:     attachmentTypeForMIME(mimeType, fallbackType),
		FileURL:  parsed.String(),
		FileName: name,
		MimeType: mimeType,
	}, true
}

func normalizeRemoteAttachment(attachment gateway.Attachment, fallbackType gateway.AttachmentType) (gateway.Attachment, bool) {
	if strings.TrimSpace(attachment.FileURL) == "" {
		return gateway.Attachment{}, false
	}
	normalized, ok := remoteAttachment(attachment.FileURL, fallbackType)
	if !ok {
		return gateway.Attachment{}, false
	}
	normalized.Type = attachmentTypeForMIME(attachment.MimeType, attachment.Type)
	if normalized.Type == "" {
		normalized.Type = attachmentTypeForMIME(normalized.MimeType, fallbackType)
	}
	normalized.FileID = attachment.FileID
	normalized.FileName = firstNonEmptyAttachmentString(attachment.FileName, normalized.FileName)
	normalized.MimeType = firstNonEmptyAttachmentString(attachment.MimeType, normalized.MimeType)
	normalized.FileSize = attachment.FileSize
	normalized.Metadata = attachment.Metadata
	return normalized, true
}

func attachmentTypeForMIME(mimeType string, fallback gateway.AttachmentType) gateway.AttachmentType {
	switch {
	case strings.HasPrefix(strings.ToLower(mimeType), "image/"):
		return gateway.AttachmentImage
	case strings.HasPrefix(strings.ToLower(mimeType), "audio/"):
		return gateway.AttachmentAudio
	case strings.HasPrefix(strings.ToLower(mimeType), "video/"):
		return gateway.AttachmentVideo
	default:
		return fallback
	}
}

func firstNonEmptyAttachmentString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func appendUniqueAttachments(attachments []gateway.Attachment, candidates ...gateway.Attachment) []gateway.Attachment {
	for _, candidate := range candidates {
		key := candidate.FileURL
		if key == "" {
			key = candidate.FilePath
		}
		if key == "" {
			continue
		}
		duplicate := false
		for _, existing := range attachments {
			existingKey := existing.FileURL
			if existingKey == "" {
				existingKey = existing.FilePath
			}
			if existingKey == key {
				duplicate = true
				break
			}
		}
		if !duplicate {
			attachments = append(attachments, candidate)
		}
	}
	return attachments
}

// CancelSession 取消指定 session 的进行中请求
func (h *AgentHandler) CancelSession(sessionID string) {
	h.cancelSession(sessionID)
}

func (h *AgentHandler) cancelSession(sessionID string) []*queuedRun {
	h.mu.Lock()
	runner := h.runners[sessionID]
	var cancel context.CancelFunc
	var queued []*queuedRun
	var cancelled []*queuedRun
	if runner != nil {
		runner.cancelled = true
		if runner.active != nil {
			cancelled = append(cancelled, runner.active)
		}
		cancel = runner.cancel
		queued = append(queued, runner.queue...)
		runner.queue = nil
		if runner.active == nil {
			if done := h.done[sessionID]; done != nil {
				close(done)
				delete(h.done, sessionID)
			}
		}
	}
	if cancel == nil {
		cancel = h.pending[sessionID]
	}
	delete(h.pending, sessionID)
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, run := range queued {
		if err := h.store.UpdateRun(sessionID, run.id, func(record *persistedRun) { record.State = "cancelled" }); err != nil {
			h.failPersistence(run.client, sessionID, run.id, run.parentID, err)
		}
	}
	cancelled = append(cancelled, queued...)
	return cancelled
}

// PendingCount 返回进行中的请求数
func (h *AgentHandler) PendingCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.pending)
}

// WaitSession waits for the handler goroutine to exit after cancellation.
// It is useful to make resource cleanup deterministic for callers that own
// temporary runtime directories.
func (h *AgentHandler) WaitSession(sessionID string, timeout time.Duration) bool {
	h.mu.Lock()
	done := h.done[sessionID]
	h.mu.Unlock()
	if done == nil {
		return true
	}
	if timeout <= 0 {
		<-done
		return true
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

func (h *AgentHandler) ensureSession(sessionID string) string {
	if h.agent == nil || h.agent.Sessions() == nil {
		return sessionID
	}
	if strings.TrimSpace(sessionID) == "" {
		return h.agent.Sessions().New().ID
	}
	return h.agent.Sessions().Ensure(sessionID).ID
}

func (h *AgentHandler) sessionForRun(ctx context.Context, sessionID, runID string) (*session.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bound := h.boundRunSession(sessionID, runID)
	if bound != nil {
		if _, err := bound.GetMessagesWithError(1); err != nil {
			return nil, err
		}
		return bound, nil
	}
	// Legacy direct syncChat/streamChat callers have no queued run to bind.
	if h.agent == nil || h.agent.Sessions() == nil {
		return nil, nil
	}
	return h.agent.Sessions().Ensure(sessionID), nil
}

func (h *AgentHandler) boundRunSession(sessionID, runID string) *session.Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	if run := h.runLocked(sessionID, runID); run != nil {
		return run.session
	}
	return nil
}

func (h *AgentHandler) sendSessionHistoryError(client *Client, runID, parentID string, err error) {
	code := "SESSION_HISTORY_FAILED"
	if errors.Is(err, session.ErrSessionDeleted) {
		code = "SESSION_DELETED"
	}
	msg, _ := NewMessage(TypeError, client.SessionID, ErrorData{Code: code, Message: "load session history failed: " + err.Error()})
	msg.ParentID = parentID
	h.emit(client, client.SessionID, runID, msg)
	h.emit(client, client.SessionID, runID, terminalStatus(client.SessionID, parentID, runID, "error"))
}

// SessionActivity returns unfinished chat runs for a session. Completed history
// is omitted. A store error is returned instead of an empty working set.
func (h *AgentHandler) activeRuns(sessionID string) ([]persistedRun, error) {
	if _, _, err := h.store.GetRun(sessionID, "activity-probe"); err != nil {
		return nil, err
	}
	return h.store.ActiveRuns(sessionID), nil
}

func (h *AgentHandler) SessionActivity(sessionID string) ([]SessionActivity, error) {
	if h == nil || h.store == nil {
		return nil, nil
	}
	runs, err := h.activeRuns(sessionID)
	if err != nil {
		return nil, err
	}
	activity := make([]SessionActivity, 0, len(runs))
	for _, run := range runs {
		activity = append(activity, SessionActivity{
			RunID:     run.ID,
			State:     runActivityState(run.State),
			UpdatedAt: run.UpdatedAt.UTC(),
		})
	}
	return activity, nil
}

// SessionActivity is one unfinished chat run. State is only queued or running.
type SessionActivity struct {
	RunID     string    `json:"run_id"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

func runActivityState(state string) string {
	if state == "running" {
		return "running"
	}
	return "queued"
}

func runActivityMessage(state string) string {
	if state == "running" {
		return "agent is running"
	}
	return "message queued"
}

func terminalStatus(sessionID, parentID, runID, state string) *Message {
	message := "completed"
	switch state {
	case "cancelled":
		message = "cancelled"
	case "error":
		message = "run failed"
	}
	status, _ := NewMessage(TypeStatus, sessionID, StatusData{State: state, Message: message})
	status.ParentID = parentID
	status.RunID = runID
	return status
}

// replayed marks an outbound copy. The stored event stays unmarked so a later
// reconnect does not persist the replay flag back into the journal.
func replayed(event *Message) *Message {
	if event == nil || event.Type != TypeStatus {
		return event
	}
	return withReplay(event)
}

func withReplay(event *Message) *Message {
	if event == nil {
		return nil
	}
	copyMsg := *event
	var payload map[string]any
	if len(event.Data) > 0 && json.Unmarshal(event.Data, &payload) != nil {
		return event
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["replay"] = true
	raw, err := json.Marshal(payload)
	if err != nil {
		return event
	}
	copyMsg.Data = raw
	return &copyMsg
}

func replayMarked(event *Message) bool {
	var payload struct {
		Replay bool `json:"replay"`
	}
	return event != nil && event.ParseData(&payload) == nil && payload.Replay
}

func reasoningDataForEvent(content string) (ReasoningData, bool) {
	content = strings.TrimSpace(content)
	if content == "" {
		return ReasoningData{}, false
	}
	if round := extractRoundNumber(content); round > 0 {
		summary := "Analyzing the request"
		stage := "start"
		if round > 1 {
			summary = "Continuing reasoning after tool results"
			stage = "continue"
		}
		return ReasoningData{Summary: summary, Round: round, Stage: stage}, true
	}
	return ReasoningData{Summary: content, Stage: "update"}, true
}

func (h *AgentHandler) progressSummaryConfig() (bool, string) {
	provider, ok := h.agent.(runtimeConfigProvider)
	if !ok || provider.Config() == nil {
		return false, ""
	}
	cfg := provider.Config().Get()
	return cfg.Server.ProgressSummaryWithLLM, strings.TrimSpace(cfg.Server.ProgressSummaryPrompt)
}

func extractRoundNumber(thinking string) int {
	var round int
	if _, err := fmt.Sscanf(strings.TrimSpace(thinking), "Thinking... (round %d)", &round); err == nil && round > 0 {
		return round
	}
	return 0
}

func parseToolParams(raw string) map[string]interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var params map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		return nil
	}
	return params
}

func formatToolCallDisplay(name, rawArgs string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "unknown_tool"
	}
	if rawArgs == "" {
		return name
	}
	return fmt.Sprintf("%s %s", name, rawArgs)
}

func formatToolResultDisplay(name, result string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	trimmed := strings.TrimSpace(result)
	if len(trimmed) > 160 {
		trimmed = trimmed[:157] + "..."
	}
	if trimmed == "" {
		return fmt.Sprintf("%s completed", name)
	}
	return fmt.Sprintf("%s: %s", name, trimmed)
}

func looksLikeToolError(result string) bool {
	result = strings.TrimSpace(strings.ToLower(result))
	return strings.HasPrefix(result, "error:")
}

type toolStepState struct {
	Name       string
	GroupID    string
	StepID     string
	Visibility string
}

func matchPendingToolStep(pending *[]toolStepState, name string, round int) toolStepState {
	if pending == nil || len(*pending) == 0 {
		groupID := fmt.Sprintf("round-%d", max(round, 1))
		return toolStepState{
			Name:       name,
			GroupID:    groupID,
			StepID:     fmt.Sprintf("%s-tool-unknown", groupID),
			Visibility: "visible",
		}
	}

	steps := *pending
	idx := 0
	for i, step := range steps {
		if step.Name == name {
			idx = i
			break
		}
	}
	matched := steps[idx]
	*pending = append(steps[:idx], steps[idx+1:]...)
	return matched
}

func (h *AgentHandler) toolVisibility(name string) string {
	if h.agent == nil {
		return classifyToolVisibility(nil, name)
	}
	return classifyToolVisibility(h.agent.Tools(), name)
}

func classifyToolVisibility(reg *tool.Registry, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "visible"
	}

	if reg != nil {
		if meta, ok := reg.Get(name); ok && meta != nil {
			if meta.HiddenFromModel || meta.Category == tool.CatSkill || meta.Category == tool.CatDelegate {
				return "hidden"
			}
		}
	}

	lower := strings.ToLower(name)
	switch {
	case lower == "request_credential":
		return "hidden"
	case lower == "skill_read",
		lower == "remember",
		lower == "recall",
		lower == "rag_search",
		lower == "rag_index",
		strings.HasPrefix(lower, "cron"):
		return "compact"
	case strings.HasPrefix(lower, "skill_"),
		strings.HasPrefix(lower, "delegate_"),
		strings.HasPrefix(lower, "autonomy_"),
		strings.HasPrefix(lower, "heartbeat_"):
		return "hidden"
	default:
		return "visible"
	}
}
