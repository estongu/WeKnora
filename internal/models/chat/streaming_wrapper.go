package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/types"
)

// streamingChat 将完整结果接口转为流式传输，原有 ChatStream 和模型信息直接透传。
// chatDelegate 避免嵌入字段 Chat 与覆盖的 Chat 方法同名。
type chatDelegate interface{ Chat }

type streamingChat struct{ chatDelegate }

// Chat 接收上下文、消息及生成选项，通过流式请求汇总完整响应。
// 返回正文、推理、最终工具调用与用量；请求失败、取消或流未正常结束时返回错误，
// 避免将半截 JSON 当作成功结果交给 Wiki、记忆等后台任务。
func (s *streamingChat) Chat(ctx context.Context, messages []Message, opts *ChatOptions) (response *types.ChatResponse, callErr error) {
	started := time.Now()
	purpose, _ := types.LLMCallMetadataFromContext(ctx)
	stage := "start_stream"
	logger.Infof(ctx, "[LLM Stream Aggregate] model_id=%q model=%q purpose=%q stream=true event=start",
		s.GetModelID(), s.GetModelName(), purpose)
	// 将请求方式与调用位置附加到原始错误，保留错误链供超时判断和重试使用。
	defer func() {
		if callErr != nil {
			callErr = fmt.Errorf("LLM stream aggregate model_id=%q model=%q purpose=%q stream=true stage=%s: %w",
				s.GetModelID(), s.GetModelName(), purpose, stage, callErr)
			logger.Errorf(ctx, "[LLM Stream Aggregate] elapsed_ms=%d error=%v", time.Since(started).Milliseconds(), callErr)
			return
		}
		logger.Infof(ctx, "[LLM Stream Aggregate] model_id=%q purpose=%q stream=true event=complete elapsed_ms=%d finish_reason=%q",
			s.GetModelID(), purpose, time.Since(started).Milliseconds(), response.FinishReason)
	}()
	// 即使调用方已有截止时间，也必须能在流内错误时主动取消上游读取。
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	ctx, cancel := api.WithLLMTimeout(ctx, api.DefaultChatTimeout)
	defer cancel()
	stream, err := s.chatDelegate.ChatStream(ctx, messages, opts)
	if err != nil {
		return nil, err
	}
	if stream == nil {
		return nil, fmt.Errorf("chat stream is nil")
	}
	stage = "read_stream"
	var content, reasoning strings.Builder
	result := &types.ChatResponse{}
	completed := false
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case chunk, ok := <-stream:
			if !ok {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if !completed || result.FinishReason == types.FinishReasonIncomplete {
					return nil, fmt.Errorf("chat stream ended without a complete response")
				}
				result.Content = content.String()
				result.ReasoningContent = reasoning.String()
				return result, nil
			}
			switch chunk.ResponseType {
			case types.ResponseTypeError:
				return nil, fmt.Errorf("chat stream: %s", chunk.Content)
			case types.ResponseTypeAnswer:
				content.WriteString(chunk.Content)
				completed = completed || chunk.Done
			case types.ResponseTypeThinking:
				reasoning.WriteString(chunk.Content)
			}
			// 工具调用为已组装的快照，不能逐块追加；用量可能晚于首个 Done 到达。
			if len(chunk.ToolCalls) > 0 {
				result.ToolCalls = chunk.ToolCalls
			}
			if chunk.Usage != nil {
				result.Usage = *chunk.Usage
			}
			if chunk.FinishReason != "" {
				result.FinishReason = chunk.FinishReason
			}
			if signature, ok := chunk.Data["reasoning_signature"].(string); ok {
				result.ReasoningSignature = signature
			}
			if metadata, ok := chunk.Data["reasoning_metadata"].(types.ProviderMetadata); ok {
				result.ReasoningMetadata = metadata
			}
		}
	}
}
