package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/stretchr/testify/require"
)

// streamStub 仅允许流式调用，以验证适配器不会重新发送非流式请求。
type streamStub struct {
	chatDelegate
	chunks []types.StreamResponse
	err    error
	stall  bool
}

// ChatStream 接收原始调用参数，返回预设事件或错误；stall 模拟等待取消的上游。
func (s *streamStub) ChatStream(ctx context.Context, _ []Message, _ *ChatOptions) (<-chan types.StreamResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	ch := make(chan types.StreamResponse, len(s.chunks))
	for _, chunk := range s.chunks {
		ch <- chunk
	}
	if s.stall {
		go func() { <-ctx.Done(); close(ch) }()
	} else {
		close(ch)
	}
	return ch, nil
}

// TestStreamingChatCollect 验证多块正文、推理、工具快照、结束原因和最后到达的用量均保留。
// 输入模拟上游流，输出应等价于完整 ChatResponse，不能在首个 Done 时丢失尾部数据。
func TestStreamingChatCollect(t *testing.T) {
	calls := []types.LLMToolCall{{ID: "call-1", Type: "function"}}
	usage := &types.TokenUsage{TotalTokens: 12}
	model := &streamingChat{chatDelegate: &streamStub{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeThinking, Content: "思考"},
		{ResponseType: types.ResponseTypeThinking, Done: true},
		{ResponseType: types.ResponseTypeAnswer, Content: "你", ToolCalls: calls},
		{ResponseType: types.ResponseTypeAnswer, Content: "好", Done: true, ToolCalls: calls, FinishReason: "stop"},
		{ResponseType: types.ResponseTypeAnswer, Done: true, Usage: usage, Data: map[string]interface{}{"reasoning_signature": "sig", "reasoning_metadata": types.ProviderMetadata{"test": json.RawMessage(`true`)}}},
	}}}
	result, err := model.Chat(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "你好", result.Content)
	require.Equal(t, "思考", result.ReasoningContent)
	require.Equal(t, calls, result.ToolCalls)
	require.Equal(t, *usage, result.Usage)
	require.Equal(t, "stop", result.FinishReason)
	require.Equal(t, "sig", result.ReasoningSignature)
	require.JSONEq(t, `true`, string(result.ReasoningMetadata["test"]))
}

// TestStreamingChatFailures 覆盖启动失败、空流、半截响应、思考结束、流内错误和取消。
// 所有失败均须返回错误而不是可被业务层误用的部分结果。
func TestStreamingChatFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		stub *streamStub
	}{
		{"startup", &streamStub{err: errors.New("upstream failed")}},
		{"empty", &streamStub{}},
		{"partial", &streamStub{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "{"}}}},
		{"thinking_done", &streamStub{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeThinking, Done: true}}}},
		{"stream_error", &streamStub{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeError, Content: "failed", Done: true}}}},
		{"incomplete", &streamStub{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: types.FinishReasonIncomplete}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := (&streamingChat{chatDelegate: tc.stub}).Chat(context.Background(), nil, nil)
			require.Error(t, err)
			require.Nil(t, result)
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result, err := (&streamingChat{chatDelegate: &streamStub{stall: true}}).Chat(ctx, nil, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, result)
}

// TestDialogueChatUsesStream 验证真实工厂把对话模型的 Chat 调用转换为 stream=true 的 HTTP 请求。
// 输入普通消息，模拟 SSE 输出，最终仍向调用方返回完整正文和用量。
func TestDialogueChatUsesStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, true, body["stream"])
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	secutils.ResetSSRFWhitelistForTest()
	t.Cleanup(secutils.ResetSSRFWhitelistForTest)
	model, err := NewChat(&ChatConfig{ModelType: types.ModelTypeKnowledgeQA, Source: types.ModelSourceRemote, Provider: "generic", BaseURL: server.URL + "/v1", ModelName: "test", APIKey: "test"}, nil)
	require.NoError(t, err)
	result, err := model.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	require.NoError(t, err)
	require.Equal(t, "hello", result.Content)
	require.Equal(t, 3, result.Usage.TotalTokens)
}

// TestStreamingChatTerminalResults 验证无正文的工具调用以及达到长度限制的响应仍能返回，
// 保留 finish_reason 供原业务层自行决定是否重试，不能把 length 当作传输失败。
func TestStreamingChatTerminalResults(t *testing.T) {
	for _, reason := range []string{"tool_calls", "length"} {
		t.Run(reason, func(t *testing.T) {
			model := &streamingChat{chatDelegate: &streamStub{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: reason}}}}
			result, err := model.Chat(context.Background(), nil, nil)
			require.NoError(t, err)
			require.Equal(t, reason, result.FinishReason)
		})
	}
}

// TestStreamingFactoryScope 验证只有显式标记为对话类型的模型启用转换，独立视觉模型保持原行为。
func TestStreamingFactoryScope(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "8.8.8.8")
	secutils.ResetSSRFWhitelistForTest()
	t.Cleanup(secutils.ResetSSRFWhitelistForTest)
	for _, modelType := range []types.ModelType{types.ModelTypeKnowledgeQA, types.ModelTypeVLLM, ""} {
		t.Run(string(modelType), func(t *testing.T) {
			config := ConfigFromModel(&types.Model{Type: modelType, Name: "test", Source: types.ModelSourceRemote}, "", "")
			require.Equal(t, modelType, config.ModelType)
			config.Provider = "generic"
			config.BaseURL = "https://8.8.8.8/v1" // 仅构建客户端，不发送请求；固定公网 IP 避免依赖 DNS。
			model, err := NewChat(config, nil)
			require.NoError(t, err)
			outer, ok := model.(*concurrencyChat)
			require.True(t, ok)
			_, streaming := outer.inner.(*streamingChat)
			require.Equal(t, modelType == types.ModelTypeKnowledgeQA, streaming)
		})
	}
}
