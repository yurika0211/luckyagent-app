package luckyagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	transport "github.com/yurika0211/luckyagent-api"
	appboot "github.com/yurika0211/luckyagent-app/bootstrap"
	"github.com/yurika0211/luckyagent-runtime/config"
	"github.com/yurika0211/luckyagent-runtime/tool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// All chat tests use an explicit local provider and a temporary runtime home.
func grpcProviderFixture(t *testing.T, handler http.HandlerFunc) (*Server, *config.Manager) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	mgr, err := config.NewManagerWithDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"provider": "openai", "api_key": "fixture-key", "api_base": upstream.URL,
		"model": "gpt-test", "stream_mode": "native", "agent.foreground.enabled": "false",
	} {
		if err := mgr.Set(key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}
	a, err := appboot.New(mgr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return NewServer(a, nil, nil, nil), mgr
}

func grpcNetworkClient(t *testing.T, service *Server, options ...GRPCServerOptions) LuckyAgentServiceClient {
	t.Helper()
	var server *GRPCServer
	if len(options) == 0 {
		server = NewGRPCServer("127.0.0.1:0", service)
	} else {
		server = NewGRPCServerWithOptions("127.0.0.1:0", service, options[0])
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(server.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewLuckyAgentServiceClient(conn)
}

func TestGRPCChatPersistsAndContinuesSessionWithActualUsage(t *testing.T) {
	var calls atomic.Int32
	var secondHadHistory atomic.Bool
	s, _ := grpcProviderFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "Write exactly one concise sentence to help the user recall") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"You asked a question."}}]}`)
			return
		}
		if calls.Add(1) == 2 {
			for _, message := range req.Messages {
				if message.Role == "assistant" && message.Content == "first answer" {
					secondHadHistory.Store(true)
				}
			}
		}
		answer := "first answer"
		if calls.Load() == 2 {
			answer = "second answer"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":7,"completion_tokens":6,"total_tokens":13}}`, answer)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := s.Chat(ctx, &ChatRequest{Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionId == "" || first.Response != "first answer" || first.Iterations != 1 || first.TokensUsed != 13 {
		t.Fatalf("unexpected first result: %v", first)
	}
	if duration, err := time.ParseDuration(first.Duration); err != nil || duration <= 0 {
		t.Fatalf("duration = %q, error = %v", first.Duration, err)
	}
	second, err := s.Chat(ctx, &ChatRequest{Message: "continue", SessionId: first.SessionId})
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionId != first.SessionId || second.Response != "second answer" || !secondHadHistory.Load() {
		t.Fatalf("session did not continue: %v; had history = %v", second, secondHadHistory.Load())
	}
	sess, exists := s.agent.Sessions().Get(first.SessionId)
	if !exists || len(sess.GetMessages()) != 4 {
		t.Fatalf("turns were not saved: exists=%v, session=%v", exists, sess)
	}
}

func TestGRPCChatRequestPolicy(t *testing.T) {
	var calls atomic.Int32
	s, mgr := grpcProviderFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	for _, tc := range []struct {
		name string
		req  *ChatRequest
		code codes.Code
	}{
		{"nil", nil, codes.InvalidArgument},
		{"empty", &ChatRequest{Message: "  "}, codes.InvalidArgument},
		{"negative iterations", &ChatRequest{Message: "hello", MaxIterations: -1}, codes.InvalidArgument},
		{"excess iterations", &ChatRequest{Message: "hello", MaxIterations: 301}, codes.InvalidArgument},
		{"unsupported metadata", &ChatRequest{Message: "hello", Metadata: map[string]string{"ignored": "value"}}, codes.InvalidArgument},
		{"unknown session", &ChatRequest{Message: "hello", SessionId: "missing-session"}, codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Chat(context.Background(), tc.req)
			if status.Code(err) != tc.code {
				t.Fatalf("Chat error = %v, want %v", err, tc.code)
			}
			stream := &grpcRecordingStream{ctx: context.Background()}
			if err := s.ChatStream(tc.req, stream); status.Code(err) != tc.code || len(stream.chunks) != 0 {
				t.Fatalf("ChatStream error = %v, chunks = %v", err, stream.chunks)
			}
		})
	}
	if calls.Load() != 0 || s.agent.Sessions().Count() != 0 {
		t.Fatalf("invalid requests executed: calls=%d sessions=%d", calls.Load(), s.agent.Sessions().Count())
	}
	if err := mgr.Set("agent.max_iterations", "7"); err != nil {
		t.Fatal(err)
	}
	_, defaults, err := s.prepareChat(context.Background(), &ChatRequest{Message: "hello"})
	if err != nil || defaults.MaxIterations != 7 || defaults.AutoApprove || defaults.Source != "grpc" {
		t.Fatalf("wrong config defaults: %+v, error=%v", defaults, err)
	}
	_, override, err := s.prepareChat(context.Background(), &ChatRequest{Message: "hello", MaxIterations: 2, AutoApprove: true})
	if err != nil || override.MaxIterations != 2 || !override.AutoApprove {
		t.Fatalf("owner overrides not applied: %+v, error=%v", override, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Chat(ctx, &ChatRequest{Message: "hello"})
	if status.Code(err) != codes.Canceled {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestGRPCChatIterationOverrideAndToolStatistics(t *testing.T) {
	var calls atomic.Int32
	s, _ := grpcProviderFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		for _, message := range req.Messages {
			if message.Role == "tool" {
				fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"tool complete"}}],"usage":{"prompt_tokens":7,"completion_tokens":6,"total_tokens":13}}`)
				return
			}
		}
		fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"fixture-call","type":"function","function":{"name":"fixture_ping","arguments":"{}"}}]}}],"usage":{"prompt_tokens":7,"completion_tokens":6,"total_tokens":13}}`)
	})
	s.agent.Tools().Register(&tool.Tool{
		Name: "fixture_ping", Enabled: true, Permission: tool.PermAuto,
		Handler: func(map[string]any) (string, error) { return "pong", nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.Chat(ctx, &ChatRequest{Message: "Use fixture_ping and report the result", MaxIterations: 1, AutoApprove: true})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "max iterations (1)") || calls.Load() != 1 {
		t.Fatalf("iteration cap did not stop first turn: calls=%d, error=%v", calls.Load(), err)
	}
	response, err := s.Chat(ctx, &ChatRequest{Message: "Use fixture_ping and report the result", MaxIterations: 2, AutoApprove: true})
	if err != nil {
		t.Fatal(err)
	}
	if response.Iterations != 2 || response.TokensUsed != 26 || len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != "fixture_ping" || response.ToolCalls[0].Result != "pong" {
		t.Fatalf("incorrect execution statistics: %v", response)
	}
}

func TestGRPCChatStreamSendsChineseBeforeGenerationFinishes(t *testing.T) {
	first := strings.Repeat("中", 50)
	release := make(chan struct{})
	defer close(release)
	s, _ := grpcProviderFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", first)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"后续\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	client := grpcNetworkClient(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ChatStream(ctx, &ChatRequest{Message: "reply in Chinese"})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := stream.Recv()
	if err != nil || chunk.Content != first || chunk.Done || chunk.SessionId == "" || !utf8.ValidString(chunk.Content) {
		t.Fatalf("first live chunk = %v, error=%v", chunk, err)
	}
	if _, err := proto.Marshal(chunk); err != nil {
		t.Fatalf("Chinese chunk is not valid protobuf: %v", err)
	}
	// Receiving the first frame above must not depend on this release.
	release <- struct{}{}
	response := chunk.Content
	done := 0
	for {
		next, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if next.SessionId != chunk.SessionId || !utf8.ValidString(next.Content) {
			t.Fatalf("bad subsequent chunk: %v", next)
		}
		response += next.Content
		if next.Done {
			done++
		}
	}
	if response != first+"后续" || done != 1 {
		t.Fatalf("response=%q done=%d", response, done)
	}
}

func TestGRPCChatStreamCancellationReachesProvider(t *testing.T) {
	cancelled := make(chan struct{})
	s, _ := grpcProviderFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"开始\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	})
	client := grpcNetworkClient(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ChatStream(ctx, &ChatRequest{Message: "continue until cancelled"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("stream cancellation = %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("provider survived cancelled gRPC stream")
	}
}

func TestGRPCChatStreamFailureDoesNotSendSuccessfulEnd(t *testing.T) {
	s, _ := grpcProviderFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"fixture rejected request"}}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := &grpcRecordingStream{ctx: ctx}
	err := s.ChatStream(&ChatRequest{Message: "hello"}, stream)
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "fixture rejected request") {
		t.Fatalf("stream error = %v", err)
	}
	for _, chunk := range stream.chunks {
		if chunk.Done {
			t.Fatalf("failed stream sent success: %v", chunk)
		}
	}
}

func TestGRPCServerInheritsAuthenticationForUnaryAndStreams(t *testing.T) {
	s, mgr := grpcProviderFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected provider call") })
	if err := mgr.Set("server.api_keys", "owner-key"); err != nil {
		t.Fatal(err)
	}
	client := grpcNetworkClient(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tc := range []struct {
		key, value string
		code       codes.Code
	}{
		{"", "", codes.Unauthenticated},
		{"x-api-key", "wrong", codes.PermissionDenied},
		{"x-api-key", "owner-key", codes.OK},
		{"authorization", "Bearer owner-key", codes.OK},
	} {
		callCtx := ctx
		if tc.key != "" {
			callCtx = metadata.AppendToOutgoingContext(callCtx, tc.key, tc.value)
		}
		if _, err := client.HealthCheck(callCtx, &emptypb.Empty{}); status.Code(err) != tc.code {
			t.Fatalf("unary %s=%q: %v", tc.key, tc.value, err)
		}
		stream, err := client.ChatStream(callCtx, &ChatRequest{Message: "hello", SessionId: "not-found"})
		if err == nil {
			_, err = stream.Recv()
		}
		want := tc.code
		if want == codes.OK {
			want = codes.NotFound
		}
		if status.Code(err) != want {
			t.Fatalf("stream %s=%q: %v, want %v", tc.key, tc.value, err, want)
		}
	}
}

func TestGRPCServerLimitsAuthenticatedCredentialAcrossMethods(t *testing.T) {
	cfg := transport.DefaultConfig()
	cfg.APIKeys, cfg.RateLimit = []string{"one", "two"}, 1
	client := grpcNetworkClient(t, NewServer(nil, nil, nil, nil), GRPCServerOptions{Config: &cfg})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	one := metadata.AppendToOutgoingContext(ctx, "x-api-key", "one")
	if _, err := client.HealthCheck(one, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.HealthCheck(one, &emptypb.Empty{}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("unary rate limit = %v", err)
	}
	stream, err := client.ChatStream(one, &ChatRequest{Message: "hello"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("stream rate limit = %v", err)
	}
	two := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer two")
	if _, err := client.HealthCheck(two, &emptypb.Empty{}); err != nil {
		t.Fatalf("second owner incorrectly shared first owner's quota: %v", err)
	}
}

func TestGRPCLocalOnlyPolicyRejectsRemoteAndForwardedPeers(t *testing.T) {
	limiter := transport.NewLimiter(60)
	defer limiter.Stop()
	admissionLimiter := transport.NewLimiter(600)
	defer admissionLimiter.Stop()
	policy := grpcAccessPolicy{config: transport.DefaultConfig(), limiter: limiter, admissionLimiter: admissionLimiter}
	for _, tc := range []struct {
		name, ip string
		md       metadata.MD
		code     codes.Code
	}{
		{"IPv4 loopback", "127.0.0.1", nil, codes.OK},
		{"IPv6 loopback", "::1", nil, codes.OK},
		{"remote", "192.0.2.4", nil, codes.Unauthenticated},
		{"forwarded local", "127.0.0.1", metadata.Pairs("x-forwarded-for", "192.0.2.4"), codes.Unauthenticated},
		{"real IP proxy", "127.0.0.1", metadata.Pairs("x-real-ip", "192.0.2.4"), codes.Unauthenticated},
		{"unknown peer", "", nil, codes.Unauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(context.Background(), tc.md)
			if tc.ip != "" {
				ctx = peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(tc.ip), Port: 9000}})
			}
			if err := policy.admit(ctx); status.Code(err) != tc.code {
				t.Fatalf("admission = %v, want %v", err, tc.code)
			}
		})
	}
}

func TestGRPCInterceptorsLimitAuthenticationFailuresByPeer(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		stream    bool
	}{
		{name: "unary wrong key", key: "private-wrong-key"},
		{name: "stream wrong key", key: "private-wrong-key", stream: true},
		{name: "unary missing key"},
		{name: "stream missing key", stream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			business := transport.NewLimiter(2)
			defer business.Stop()
			admission := transport.NewLimiter(grpcAdmissionLimit(2))
			defer admission.Stop()
			policy := grpcAccessPolicy{
				config:  transport.Config{APIKeys: []string{"valid"}, RateLimit: 2},
				limiter: business, admissionLimiter: admission,
			}
			called := false
			invoke := func(ip, key string) error {
				ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", key))
				ctx = peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 1234}})
				if tc.stream {
					return policy.stream(nil, &grpcRecordingStream{ctx: ctx}, &grpc.StreamServerInfo{}, func(any, grpc.ServerStream) error { called = true; return nil })
				}
				_, err := policy.unary(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { called = true; return nil, nil })
				return err
			}
			for i := 0; i < 61; i++ {
				err := invoke("192.0.2.7", tc.key)
				want := codes.PermissionDenied
				if tc.key == "" {
					want = codes.Unauthenticated
				}
				if i == 60 {
					want = codes.ResourceExhausted
				}
				if status.Code(err) != want {
					t.Fatalf("attempt %d error=%v, want %v", i+1, err, want)
				}
				if strings.Contains(err.Error(), "private-wrong-key") {
					t.Fatalf("credential leaked in error: %v", err)
				}
			}
			if called {
				t.Fatal("unauthenticated request reached handler")
			}
			if err := invoke("192.0.2.8", tc.key); status.Code(err) == codes.ResourceExhausted {
				t.Fatalf("independent peer shared admission quota: %v", err)
			}
			if err := invoke("192.0.2.8", "valid"); err != nil || !called {
				t.Fatalf("authentication failures consumed owner business quota: called=%v error=%v", called, err)
			}
		})
	}
}

func TestGRPCAdmissionLimitDoesNotOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	if grpcAdmissionLimit(2) != 60 || grpcAdmissionLimit(60) != 600 || grpcAdmissionLimit(maxInt) != maxInt {
		t.Fatal("unexpected admission limits")
	}
}

func TestGRPCOptionalBackendsReturnUnavailable(t *testing.T) {
	s := NewServer(nil, nil, nil, nil)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"memory store", func() error { _, err := s.MemoryStore(ctx, &MemoryStoreRequest{}); return err }},
		{"memory recall", func() error { _, err := s.MemoryRecall(ctx, &MemoryRecallRequest{}); return err }},
		{"memory list", func() error { _, err := s.MemoryList(ctx, &emptypb.Empty{}); return err }},
		{"memory delete", func() error { _, err := s.MemoryDelete(ctx, &MemoryDeleteRequest{}); return err }},
		{"RAG index", func() error { _, err := s.RAGIndex(ctx, &RAGIndexRequest{}); return err }},
		{"RAG search", func() error { _, err := s.RAGSearch(ctx, &RAGSearchRequest{}); return err }},
		{"workflow create", func() error { _, err := s.WorkflowCreate(ctx, &WorkflowCreateRequest{}); return err }},
		{"workflow get", func() error { _, err := s.WorkflowGet(ctx, &WorkflowGetRequest{}); return err }},
		{"workflow list", func() error { _, err := s.WorkflowList(ctx, &emptypb.Empty{}); return err }},
		{"workflow delete", func() error { _, err := s.WorkflowDelete(ctx, &WorkflowDeleteRequest{}); return err }},
		{"workflow start", func() error { _, err := s.WorkflowStart(ctx, &WorkflowStartRequest{}); return err }},
		{"workflow instance get", func() error { _, err := s.WorkflowInstanceGet(ctx, &WorkflowInstanceGetRequest{}); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); status.Code(err) != codes.Unavailable {
				t.Fatalf("error=%v, want Unavailable", err)
			}
		})
	}
	health, err := s.HealthCheck(ctx, &emptypb.Empty{})
	if err != nil || health.Status != "degraded" {
		t.Fatalf("empty runtime health=%v, error=%v", health, err)
	}
}

func TestGRPCInterceptorsRecoverWithoutDisclosingPanic(t *testing.T) {
	limiter := transport.NewLimiter(60)
	defer limiter.Stop()
	admission := transport.NewLimiter(600)
	defer admission.Stop()
	policy := grpcAccessPolicy{config: transport.Config{APIKeys: []string{"valid"}}, limiter: limiter, admissionLimiter: admission}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "valid"))
	ctx = peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1234}})
	response, err := policy.unary(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { panic("private-provider-secret") })
	if response != nil || status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private-provider-secret") {
		t.Fatalf("unsafe unary panic response=%v, error=%v", response, err)
	}
	err = policy.stream(nil, &grpcRecordingStream{ctx: ctx}, &grpc.StreamServerInfo{}, func(any, grpc.ServerStream) error { panic("private-provider-secret") })
	if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private-provider-secret") {
		t.Fatalf("unsafe stream panic: %v", err)
	}
	response, err = policy.unary(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { return "still available", nil })
	if response != "still available" || err != nil {
		t.Fatalf("service did not survive panic: response=%v error=%v", response, err)
	}
}

type grpcRecordingStream struct {
	ctx    context.Context
	chunks []*ChatChunk
}

func (s *grpcRecordingStream) Send(chunk *ChatChunk) error {
	s.chunks = append(s.chunks, chunk)
	return nil
}
func (s *grpcRecordingStream) Context() context.Context     { return s.ctx }
func (s *grpcRecordingStream) SetHeader(metadata.MD) error  { return nil }
func (s *grpcRecordingStream) SendHeader(metadata.MD) error { return nil }
func (s *grpcRecordingStream) SetTrailer(metadata.MD)       {}
func (s *grpcRecordingStream) SendMsg(any) error            { return nil }
func (s *grpcRecordingStream) RecvMsg(any) error            { return io.EOF }
