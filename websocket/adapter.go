// Package websocket preserves host imports while implementation is assembled from independent edge modules.
package websocket

import app "github.com/yurika0211/luckyagent-app/channelapp/websocket"

type AgentHandler = app.AgentHandler
type ApprovalRequestData = app.ApprovalRequestData
type ApprovalResponseData = app.ApprovalResponseData
type CancelData = app.CancelData
type ChatData = app.ChatData
type Client = app.Client
type CompactData = app.CompactData

var DefaultHubConfig = app.DefaultHubConfig

type ErrorData = app.ErrorData
type Hub = app.Hub
type HubConfig = app.HubConfig
type HubStats = app.HubStats
type LuckyData = app.LuckyData
type Message = app.Message
type MessageHandler = app.MessageHandler
type MessageType = app.MessageType

var NewAgentHandler = app.NewAgentHandler
var NewHub = app.NewHub
var NewMessage = app.NewMessage
var ParseMessage = app.ParseMessage

type ReasoningData = app.ReasoningData
type ReconnectData = app.ReconnectData
type ReconnectHandler = app.ReconnectHandler
type SessionActivity = app.SessionActivity
type StatusData = app.StatusData
type StreamChunkData = app.StreamChunkData
type StreamEndData = app.StreamEndData
type TaskEventData = app.TaskEventData
type ToolCallData = app.ToolCallData
type ToolResultData = app.ToolResultData

const TypeApproval = app.TypeApproval
const TypeApprovalResponse = app.TypeApprovalResponse
const TypeCancel = app.TypeCancel
const TypeChat = app.TypeChat
const TypeCompact = app.TypeCompact
const TypeError = app.TypeError
const TypeLucky = app.TypeLucky
const TypePing = app.TypePing
const TypePong = app.TypePong
const TypeReasoning = app.TypeReasoning
const TypeReconnect = app.TypeReconnect
const TypeStatus = app.TypeStatus
const TypeStreamAck = app.TypeStreamAck
const TypeStreamChunk = app.TypeStreamChunk
const TypeStreamEnd = app.TypeStreamEnd
const TypeTaskEvent = app.TypeTaskEvent
const TypeToolCall = app.TypeToolCall
const TypeToolResult = app.TypeToolResult
