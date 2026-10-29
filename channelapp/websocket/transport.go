package websocket

import transport "github.com/yurika0211/luckyagent-api/websocket"

type ApprovalRequestData = transport.ApprovalRequestData
type ApprovalResponseData = transport.ApprovalResponseData
type CancelData = transport.CancelData
type ChatData = transport.ChatData
type Client = transport.Client
type CompactData = transport.CompactData

var DefaultHubConfig = transport.DefaultHubConfig

type ErrorData = transport.ErrorData
type Hub = transport.Hub
type HubConfig = transport.HubConfig
type HubStats = transport.HubStats
type LuckyData = transport.LuckyData
type Message = transport.Message
type MessageHandler = transport.MessageHandler
type MessageType = transport.MessageType

var NewHub = transport.NewHub
var NewMessage = transport.NewMessage
var ParseMessage = transport.ParseMessage

type ReasoningData = transport.ReasoningData
type ReconnectData = transport.ReconnectData
type ReconnectHandler = transport.ReconnectHandler
type StatusData = transport.StatusData
type StreamChunkData = transport.StreamChunkData
type StreamEndData = transport.StreamEndData
type TaskEventData = transport.TaskEventData
type ToolCallData = transport.ToolCallData
type ToolResultData = transport.ToolResultData
type UIActionData = transport.UIActionData
type UIDeltaData = transport.UIDeltaData
type UINodeData = transport.UINodeData

const TypeApproval = transport.TypeApproval
const TypeApprovalResponse = transport.TypeApprovalResponse
const TypeCancel = transport.TypeCancel
const TypeChat = transport.TypeChat
const TypeCompact = transport.TypeCompact
const TypeError = transport.TypeError
const TypeLucky = transport.TypeLucky
const TypePing = transport.TypePing
const TypePong = transport.TypePong
const TypeReasoning = transport.TypeReasoning
const TypeReconnect = transport.TypeReconnect
const TypeStatus = transport.TypeStatus
const TypeStreamAck = transport.TypeStreamAck
const TypeStreamChunk = transport.TypeStreamChunk
const TypeStreamEnd = transport.TypeStreamEnd
const TypeTaskEvent = transport.TypeTaskEvent
const TypeToolCall = transport.TypeToolCall
const TypeToolResult = transport.TypeToolResult
const TypeUIAction = transport.TypeUIAction
const TypeUIDelta = transport.TypeUIDelta

var generateID = transport.NewID
