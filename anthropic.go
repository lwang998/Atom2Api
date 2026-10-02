package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Anthropic Messages API compatibility: POST /v1/messages is translated to the
// OpenAI chat-completions upstream, and responses/SSE are translated back so
// Claude Code and other Anthropic-protocol clients work unchanged. Auth reuses
// RequireAPIKey (x-api-key or Bearer).

const anthropicMaxTokensFallback = 4096

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools         json.RawMessage    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source,omitempty"`
}

type anthropicToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n"))
}

func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func (p *Proxy) HandleAnthropicMessages(w http.ResponseWriter, r *http.Request, key APIKey) {
	started := time.Now()
	var firstTokenAt time.Time
	markFirstToken := func() {
		if firstTokenAt.IsZero() {
			firstTokenAt = time.Now()
		}
	}
	requestID := randomID("req")
	w.Header().Set("X-Request-Id", requestID)
	config := p.config.Snapshot()
	debugEnabled := config.AuditDebugEnabled
	retryStatuses, _ := parseRetryStatusCodes(config.RetryStatusCodes)
	captured := &auditResponseWriter{ResponseWriter: w, captureBody: debugEnabled}
	w = captured
	audit := UsageRecord{
		ID: requestID, Timestamp: started.UTC(), Method: r.Method, Path: r.URL.Path, APIKeyID: key.ID,
	}
	defer func() {
		audit.LatencyMS = time.Since(started).Milliseconds()
		if audit.Streaming && !firstTokenAt.IsZero() {
			audit.FirstTokenLatencyMS = firstTokenAt.Sub(started).Milliseconds()
			audit.CompletionLatencyMS = time.Since(firstTokenAt).Milliseconds()
		}
		audit.Status = captured.status
		if audit.Status == 0 {
			audit.Status = http.StatusInternalServerError
		}
		if captured.captureBody {
			audit.ResponseBody = captured.body.String()
		}
		if debugEnabled || audit.Status >= http.StatusBadRequest {
			audit.RequestHeaders = auditHeaders(r.Header)
			audit.ResponseHeaders = auditHeaders(captured.Header())
		}
		p.record(audit)
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 20<<20))
	if debugEnabled {
		audit.RequestBody = string(body)
	}
	if err != nil {
		audit.Error = "request body is too large or unreadable"
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", audit.Error)
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		audit.Error = "request body must be an Anthropic Messages JSON object"
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", audit.Error)
		return
	}
	if req.Model == "" {
		audit.Error = "model is required"
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", audit.Error)
		return
	}
	audit.Model = req.Model

	route, err := p.router.Resolve(req.Model, key)
	if err != nil {
		audit.Error = err.Error()
		switch {
		case strings.Contains(err.Error(), "no active account"):
			writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error", err.Error())
		case strings.Contains(err.Error(), "model is not available"):
			writeAnthropicError(w, http.StatusNotFound, "not_found_error", err.Error())
		case strings.Contains(err.Error(), "not allowed to use this model"):
			writeAnthropicError(w, http.StatusForbidden, "permission_error", err.Error())
		default:
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		}
		return
	}
	audit.UpstreamModel = route.Upstream
	audit.AccountID = route.Account.ID
	if p.oauth != nil {
		if refreshed, refreshErr := p.oauth.Refresh(r.Context(), route.Account.ID); refreshErr == nil {
			route.Token = refreshed
		} else {
			audit.Error = refreshErr.Error()
			if oauthCredentialsUnavailable(refreshErr) {
				p.disableAccount(route.Account.ID, audit.Error)
			}
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream authentication failed")
			return
		}
	}

	chatPayload, err := anthropicToChatPayload(&req, route.Upstream)
	if err != nil {
		audit.Error = err.Error()
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	audit.Streaming = req.Stream
	if req.Stream {
		chatPayload["stream_options"] = map[string]any{"include_usage": true}
	}
	upstreamBody, err := json.Marshal(chatPayload)
	if err != nil {
		audit.Error = "could not encode upstream request"
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "could not encode upstream request")
		return
	}

	timeout := time.Duration(config.RequestTimeoutSecs) * time.Second
	requestContext, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	var request *http.Request
	var response *http.Response
	for attempt := 1; ; attempt++ {
		request, response, err = p.doUpstreamRequest(requestContext, route, "/v1/chat/completions", upstreamBody, req.Stream, requestID)
		if err != nil {
			break
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			p.disableAccount(route.Account.ID, fmt.Sprintf("upstream authentication failed (%d)", response.StatusCode))
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden ||
			audit.RetryCount >= config.RequestRetryCount || !retryStatuses.Contains(response.StatusCode) {
			break
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		attemptError := compactError(responseBody)
		if readErr != nil {
			attemptError = readErr.Error()
		}
		audit.RetryCount++
		audit.Error = attemptError
	}
	if err != nil {
		if request != nil {
			audit.RequestHeaders = auditHeaders(request.Header)
		}
		audit.Error = err.Error()
		writeAnthropicError(w, http.StatusBadGateway, "api_error", upstreamFailureMessage(http.StatusBadGateway, requestID))
		return
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		errorBody, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		audit.Error = compactError(errorBody)
		writeAnthropicError(w, response.StatusCode, anthropicErrorType(response.StatusCode), upstreamFailureMessage(response.StatusCode, requestID))
		return
	}

	if req.Stream {
		usage, errorText := p.streamAnthropicResponse(w, response, route.Requested, markFirstToken)
		audit.InputTokens, audit.OutputTokens = usage.Input, usage.Output
		audit.Error = errorText
		return
	}
	usage, errorText := p.bufferedAnthropicResponse(w, response, route.Requested)
	audit.InputTokens, audit.OutputTokens = usage.Input, usage.Output
	audit.Error = errorText
}

// anthropicToChatPayload converts an Anthropic Messages request into an OpenAI
// chat-completions payload addressed to the upstream model.
func anthropicToChatPayload(req *anthropicRequest, upstreamModel string) (map[string]any, error) {
	messages := make([]map[string]any, 0, len(req.Messages)+2)
	if systemText := anthropicSystemText(req.System); systemText != "" {
		messages = append(messages, map[string]any{"role": "system", "content": systemText})
	}
	for _, message := range req.Messages {
		converted, err := anthropicMessageToOpenAI(message)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicMaxTokensFallback
	}
	payload := map[string]any{
		"model":      upstreamModel,
		"messages":   messages,
		"max_tokens": maxTokens,
	}
	if req.Stream {
		payload["stream"] = true
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		payload["stop"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		tools, err := anthropicToolsToOpenAI(req.Tools)
		if err != nil {
			return nil, err
		}
		payload["tools"] = tools
	}
	if len(req.ToolChoice) > 0 {
		if choice := anthropicToolChoiceToOpenAI(req.ToolChoice); choice != nil {
			payload["tool_choice"] = choice
		}
	}
	return payload, nil
}

func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// anthropicMessageToOpenAI converts one Anthropic message into one or more
// OpenAI messages: tool_result blocks become role:"tool" messages, tool_use
// blocks become tool_calls, and the remaining text/image parts become a
// single user (or assistant) message.
func anthropicMessageToOpenAI(message anthropicMessage) ([]map[string]any, error) {
	if len(message.Content) == 0 {
		return []map[string]any{{"role": message.Role, "content": ""}}, nil
	}
	var text string
	if err := json.Unmarshal(message.Content, &text); err == nil {
		return []map[string]any{{"role": message.Role, "content": text}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil, fmt.Errorf("decode message content blocks: %w", err)
	}

	var textParts []map[string]any
	var toolCalls []map[string]any
	var result []map[string]any
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		result = append(result, map[string]any{"role": message.Role, "content": textParts})
		textParts = nil
	}
	flushToolCalls := func() {
		if len(toolCalls) == 0 {
			return
		}
		result = append(result, map[string]any{"role": message.Role, "content": nil, "tool_calls": toolCalls})
		toolCalls = nil
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				textParts = append(textParts, map[string]any{"type": "text", "text": block.Text})
			}
		case "image":
			if block.Source != nil {
				textParts = append(textParts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:" + block.Source.MediaType + ";base64," + block.Source.Data},
				})
			}
		case "thinking":
			// Upstream models produce their own reasoning; drop client thinking.
		case "tool_use":
			flushText()
			arguments, err := json.Marshal(block.Input)
			if err != nil || len(block.Input) == 0 {
				arguments = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]any{
					"name":      block.Name,
					"arguments": string(arguments),
				},
			})
		case "tool_result":
			flushText()
			flushToolCalls()
			result = append(result, map[string]any{
				"role":         "tool",
				"tool_call_id": block.ToolUseID,
				"content":      anthropicToolResultText(block),
			})
		}
	}
	flushToolCalls()
	flushText()
	if len(result) == 0 {
		return []map[string]any{{"role": message.Role, "content": ""}}, nil
	}
	return result, nil
}

func anthropicToolResultText(block anthropicBlock) string {
	if len(block.Content) == 0 {
		return ""
	}
	var single string
	if err := json.Unmarshal(block.Content, &single); err == nil {
		return single
	}
	var parts []anthropicBlock
	if err := json.Unmarshal(block.Content, &parts); err == nil {
		texts := make([]string, 0, len(parts))
		for _, part := range parts {
			if part.Type == "text" && part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(block.Content)
}

func anthropicToolsToOpenAI(raw json.RawMessage) ([]map[string]any, error) {
	var tools []anthropicBlock
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("decode tools: %w", err)
	}
	converted := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if tool.Name == "" {
			continue
		}
		parameters := tool.Input
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		converted = append(converted, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Text,
				"parameters":  parameters,
			},
		})
	}
	return converted, nil
}

func anthropicToolChoiceToOpenAI(raw json.RawMessage) any {
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil
	}
	switch choice.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}
	default:
		return nil
	}
}

// ─── Response translation ────────────────────────────────────────────────────

func anthropicStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// anthropicEmitter converts OpenAI chat chunks into Anthropic SSE events.
// Anthropic streams carry at most one open content block at a time; blocks are
// closed (and their index advanced) as soon as a different block type begins.
type anthropicEmitter struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	blockType string
	blockIdx  int
	toolSeen  map[int]bool
}

func (e *anthropicEmitter) raw(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, data)
	e.flusher.Flush()
}

func (e *anthropicEmitter) startBlock(blockType string, block map[string]any) {
	e.closeBlock()
	e.blockType = blockType
	e.raw("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         e.blockIdx,
		"content_block": block,
	})
}

func (e *anthropicEmitter) closeBlock() {
	if e.blockType == "" {
		return
	}
	e.raw("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.blockIdx})
	e.blockType = ""
	e.blockIdx++
}

func (p *Proxy) streamAnthropicResponse(w http.ResponseWriter, response *http.Response, model string, markFirstToken func()) (tokenUsage, string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming is not supported by this server")
		return tokenUsage{}, "streaming is not supported by this server"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusOK)

	emitter := &anthropicEmitter{w: w, flusher: flusher, toolSeen: map[int]bool{}}
	emitter.raw("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": randomID("msg"), "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})

	reader := bufio.NewReader(response.Body)
	var usage tokenUsage
	var stopReason string
	errorText := ""
	finalize := func() {
		emitter.closeBlock()
		if emitter.blockIdx == 0 {
			// Anthropic responses must carry at least one content block.
			emitter.raw("content_block_start", map[string]any{
				"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			emitter.raw("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		}
		if stopReason == "" {
			stopReason = "end_turn"
		}
		deltaUsage := map[string]any{"output_tokens": usage.Output}
		if usage.Input > 0 {
			deltaUsage["input_tokens"] = usage.Input
		}
		emitter.raw("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": deltaUsage,
		})
		emitter.raw("message_stop", map[string]any{"type": "message_stop"})
	}

	for {
		line, readErr := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
			if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
				if streamChunkHasOutput(data) {
					markFirstToken()
				}
				chunkUsage, chunkStop := emitter.translateChunk(data)
				if chunkUsage.Input > 0 || chunkUsage.Output > 0 {
					usage = chunkUsage
				}
				if chunkStop != "" {
					stopReason = chunkStop
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				errorText = readErr.Error()
			}
			finalize()
			return usage, errorText
		}
	}
}

// translateChunk maps one OpenAI SSE chunk to Anthropic events. Returns the
// chunk usage (when present) and the finish reason (when present).
func (e *anthropicEmitter) translateChunk(data []byte) (tokenUsage, string) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return tokenUsage{}, ""
	}
	usage := tokenUsage{}
	if chunk.Usage != nil {
		usage = tokenUsage{Input: chunk.Usage.PromptTokens, Output: chunk.Usage.CompletionTokens}
	}
	stop := ""
	for _, choice := range chunk.Choices {
		for _, reasoning := range []string{choice.Delta.ReasoningContent, choice.Delta.Reasoning} {
			if reasoning == "" {
				continue
			}
			if e.blockType != "thinking" {
				e.startBlock("thinking", map[string]any{"type": "thinking", "thinking": ""})
			}
			e.raw("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": e.blockIdx,
				"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
			})
		}
		if text := choice.Delta.Content; text != "" {
			if e.blockType != "text" {
				e.startBlock("text", map[string]any{"type": "text", "text": ""})
			}
			e.raw("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": e.blockIdx,
				"delta": map[string]any{"type": "text_delta", "text": text},
			})
		}
		for _, toolCall := range choice.Delta.ToolCalls {
			isNew := !e.toolSeen[toolCall.Index]
			if isNew {
				e.toolSeen[toolCall.Index] = true
				e.startBlock("tool_use", map[string]any{
					"type": "tool_use", "id": toolCall.ID, "name": toolCall.Function.Name, "input": map[string]any{},
				})
			}
			if toolCall.Function.Arguments != "" && e.blockType == "tool_use" {
				e.raw("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": e.blockIdx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": toolCall.Function.Arguments},
				})
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			stop = anthropicStopReason(*choice.FinishReason)
		}
	}
	return usage, stop
}

func (p *Proxy) bufferedAnthropicResponse(w http.ResponseWriter, response *http.Response, model string) (tokenUsage, string) {
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "could not read upstream response")
		return tokenUsage{}, "could not read upstream response"
	}
	var chat struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content          string              `json:"content"`
				ReasoningContent string              `json:"reasoning_content"`
				ToolCalls        []anthropicToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &chat); err != nil || len(chat.Choices) == 0 {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "could not parse upstream response")
		return tokenUsage{}, "could not parse upstream response"
	}
	message := chat.Choices[0].Message
	content := make([]map[string]any, 0, 2)
	if message.ReasoningContent != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": message.ReasoningContent})
	}
	if message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": message.Content})
	}
	for _, toolCall := range message.ToolCalls {
		var input any = map[string]any{}
		if len(toolCall.Function.Arguments) > 0 {
			if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &input); err != nil {
				input = map[string]any{}
			}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": toolCall.ID, "name": toolCall.Function.Name, "input": input,
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	usage := tokenUsage{}
	if chat.Usage != nil {
		usage.Input = chat.Usage.PromptTokens
		usage.Output = chat.Usage.CompletionTokens
	}
	stopReason := "end_turn"
	if chat.Choices[0].FinishReason != "" {
		stopReason = anthropicStopReason(chat.Choices[0].FinishReason)
	}
	id := chat.ID
	if id == "" {
		id = randomID("msg")
	}
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	payload, _ := json.Marshal(map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stopReason, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": usage.Input, "output_tokens": usage.Output},
	})
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n"))
	return usage, ""
}

// HandleAnthropicCountTokens estimates the token count of an Anthropic request
// (~4 characters per token) without contacting the upstream.
func (p *Proxy) HandleAnthropicCountTokens(w http.ResponseWriter, r *http.Request, _ APIKey) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 20<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body is too large or unreadable")
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body must be an Anthropic Messages JSON object")
		return
	}
	total := len(anthropicSystemText(req.System))
	for _, message := range req.Messages {
		total += len(message.Content) + 8
	}
	if len(req.Tools) > 0 {
		total += len(req.Tools) / 3
	}
	tokens := total / 4
	if tokens < 1 {
		tokens = 1
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"input_tokens":%d}`+"\n", tokens)
}
