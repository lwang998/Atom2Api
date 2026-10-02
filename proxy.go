package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Proxy struct {
	config *ConfigManager
	store  *Store
	router *ModelRouter
	oauth  *OAuthManager
	client *http.Client
}

type tokenUsage struct {
	Input     int64
	Output    int64
	Cached    int64
	Reasoning int64
}

type upstreamRequestError struct {
	code string
	err  error
}

func (e *upstreamRequestError) Error() string { return e.err.Error() }

func upstreamRequestErrorCode(err error) string {
	var requestError *upstreamRequestError
	if errors.As(err, &requestError) && requestError.code != "" {
		return requestError.code
	}
	return "upstream_error"
}

type auditResponseWriter struct {
	http.ResponseWriter
	status      int
	captureBody bool
	body        bytes.Buffer
}

type capturedReadCloser struct {
	io.Reader
	io.Closer
}

func (w *auditResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status >= http.StatusBadRequest {
		w.captureBody = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	written, err := w.ResponseWriter.Write(data)
	if written > 0 && w.captureBody {
		_, _ = w.body.Write(data[:written])
	}
	return written, err
}

func (w *auditResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *auditResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func NewProxy(config *ConfigManager, store *Store, router *ModelRouter, oauth *OAuthManager) *Proxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 20
	transport.ResponseHeaderTimeout = 60 * time.Second
	return &Proxy{
		config: config, store: store, router: router, oauth: oauth,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("upstream redirects are disabled")
			},
		},
	}
}

func (p *Proxy) HandleModels(w http.ResponseWriter, _ *http.Request, key APIKey) {
	models := p.router.Catalog()
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if !model.Enabled || model.AccountCount == 0 || !modelAllowedForKey(model, key) {
			continue
		}
		data = append(data, map[string]any{
			"id": model.Alias, "object": "model", "created": 0, "owned_by": "atom2api",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func modelAllowedForKey(model ModelView, key APIKey) bool {
	if len(key.AllowedModels) == 0 {
		return true
	}
	for _, allowed := range key.AllowedModels {
		allowed = trimContextSuffix(strings.TrimSpace(allowed))
		if allowed == model.Alias || allowed == model.Upstream {
			return true
		}
	}
	return false
}

func (p *Proxy) HandleRequest(w http.ResponseWriter, r *http.Request, key APIKey) {
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
	var finalAttemptBody *bytes.Buffer
	var finalAttemptStarted time.Time
	if debugEnabled {
		audit.RequestHeaders = auditHeaders(r.Header)
	}
	defer func() {
		finished := time.Now()
		if finalAttemptBody != nil && len(audit.Attempts) > 0 {
			finalAttempt := &audit.Attempts[len(audit.Attempts)-1]
			finalAttempt.ResponseBody = finalAttemptBody.String()
			finalAttempt.LatencyMS = finished.Sub(finalAttemptStarted).Milliseconds()
			finalAttempt.Error = audit.Error
		}
		audit.LatencyMS = finished.Sub(started).Milliseconds()
		if audit.Streaming && !firstTokenAt.IsZero() {
			audit.FirstTokenLatencyMS = firstTokenAt.Sub(started).Milliseconds()
			audit.CompletionLatencyMS = finished.Sub(firstTokenAt).Milliseconds()
		}
		audit.Status = captured.status
		if audit.Status == 0 {
			audit.Status = http.StatusInternalServerError
		}
		failed := audit.Status >= http.StatusBadRequest
		if (debugEnabled || failed) && len(audit.RequestHeaders) == 0 {
			audit.RequestHeaders = auditHeaders(r.Header)
		}
		if captured.captureBody {
			audit.ResponseBody = captured.body.String()
		}
		if (debugEnabled || failed) && len(audit.ResponseHeaders) == 0 {
			audit.ResponseHeaders = auditHeaders(captured.Header())
		}
		p.record(audit)
	}()
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	body, err := io.ReadAll(r.Body)
	if debugEnabled {
		audit.RequestBody = string(body)
	}
	if err != nil {
		audit.Error = "request body is too large or unreadable"
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "request body is too large or unreadable")
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		audit.Error = "request body must be a JSON object"
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	var requestedModel string
	if raw := payload["model"]; raw != nil {
		_ = json.Unmarshal(raw, &requestedModel)
	}
	audit.Model = requestedModel
	route, err := p.router.Resolve(requestedModel, key)
	if err != nil {
		status := http.StatusBadRequest
		code := "model_not_found"
		if strings.Contains(err.Error(), "no active account") {
			status = http.StatusTooManyRequests
			code = "insufficient_quota"
		}
		audit.Error = err.Error()
		writeOpenAIError(w, status, code, audit.Error)
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
			writeOpenAIError(w, http.StatusBadGateway, "upstream_auth_error", upstreamFailureMessage(http.StatusBadGateway, requestID))
			return
		}
	}
	modelJSON, _ := json.Marshal(route.Upstream)
	if config.SystemPromptEnabled {
		if err := applySystemPrompt(payload, r.URL.Path, route.Requested, config.SystemPrompt); err != nil {
			audit.Error = err.Error()
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", audit.Error)
			return
		}
	}
	payload["model"] = modelJSON
	streaming := false
	if raw := payload["stream"]; raw != nil {
		_ = json.Unmarshal(raw, &streaming)
	}
	audit.Streaming = streaming
	if streaming && r.URL.Path == "/v1/chat/completions" {
		var streamOptions map[string]any
		if raw := payload["stream_options"]; raw != nil {
			_ = json.Unmarshal(raw, &streamOptions)
		}
		if streamOptions == nil {
			streamOptions = map[string]any{}
		}
		streamOptions["include_usage"] = true
		payload["stream_options"], _ = json.Marshal(streamOptions)
	}
	body, err = json.Marshal(payload)
	if err != nil {
		audit.Error = "could not encode upstream request"
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "could not encode upstream request")
		return
	}
	timeout := time.Duration(config.RequestTimeoutSecs) * time.Second
	requestContext, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	requestPath := r.URL.Path
	requestBody := body
	var compatContext *responsesCompatContext
	responsesChatCompat := r.URL.Path == "/v1/responses" && route.ResponsesChatCompat
	if responsesChatCompat {
		requestBody, compatContext, err = responsesRequestToChat(payload)
		if err != nil {
			audit.Error = err.Error()
			writeOpenAIError(w, http.StatusBadRequest, "unsupported_parameter", audit.Error)
			return
		}
		requestPath = "/v1/chat/completions"
	}
	var request *http.Request
	var response *http.Response
	for attempt := 1; ; attempt++ {
		attemptStarted := time.Now()
		request, response, err = p.doUpstreamRequest(requestContext, route, requestPath, requestBody, streaming, requestID)
		if err != nil {
			if audit.RetryCount > 0 {
				audit.Attempts = append(audit.Attempts, RequestAttempt{
					Attempt: attempt, LatencyMS: time.Since(attemptStarted).Milliseconds(), Error: err.Error(),
				})
			}
			break
		}
		accountUnavailable := response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden
		if accountUnavailable {
			p.disableAccount(route.Account.ID, fmt.Sprintf("upstream authentication failed (%d)", response.StatusCode))
		}
		if accountUnavailable || audit.RetryCount >= config.RequestRetryCount || !retryStatuses.Contains(response.StatusCode) {
			if audit.RetryCount > 0 {
				finalAttemptStarted = attemptStarted
				finalAttemptBody = &bytes.Buffer{}
				response.Body = &capturedReadCloser{Reader: io.TeeReader(response.Body, finalAttemptBody), Closer: response.Body}
				audit.Attempts = append(audit.Attempts, RequestAttempt{
					Attempt: attempt, Status: response.StatusCode,
					LatencyMS: time.Since(attemptStarted).Milliseconds(), ResponseHeaders: auditHeaders(response.Header),
				})
			}
			break
		}

		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<20))
		_ = response.Body.Close()
		attemptError := compactError(responseBody)
		if readErr != nil {
			attemptError = readErr.Error()
		}
		audit.Attempts = append(audit.Attempts, RequestAttempt{
			Attempt: attempt, Status: response.StatusCode, LatencyMS: time.Since(attemptStarted).Milliseconds(),
			Error: attemptError, ResponseBody: string(responseBody), ResponseHeaders: auditHeaders(response.Header),
		})
		audit.RetryCount++
	}
	if err != nil {
		if request != nil {
			audit.RequestHeaders = auditHeaders(request.Header)
		}
		audit.Error = err.Error()
		writeOpenAIError(w, http.StatusBadGateway, upstreamRequestErrorCode(err), upstreamFailureMessage(http.StatusBadGateway, requestID))
		return
	}
	defer response.Body.Close()
	if debugEnabled || audit.RetryCount > 0 || response.StatusCode >= http.StatusBadRequest {
		audit.RequestHeaders = auditHeaders(request.Header)
	}
	if debugEnabled || audit.RetryCount > 0 || response.StatusCode != http.StatusOK {
		captured.captureBody = true
		audit.ResponseHeaders = auditHeaders(response.Header)
	}
	copyUpstreamHeaders(w.Header(), response.Header)
	w.Header().Set("X-Request-Id", requestID)
	w.Header().Set("X-Atom2api-Account", route.Account.ID)
	if responsesChatCompat {
		w.Header().Set(responsesFallbackHeader, "chat-completions")
	}
	if responsesChatCompat && response.StatusCode >= 200 && response.StatusCode < 300 {
		if streaming {
			usage, errorText := p.streamChatAsResponses(w, response, route, compatContext, markFirstToken)
			audit.InputTokens, audit.OutputTokens = usage.Input, usage.Output
			audit.CachedTokens, audit.ReasoningTokens = usage.Cached, usage.Reasoning
			audit.Error = errorText
			return
		}
		usage, errorText := p.bufferedChatAsResponses(w, response, route, compatContext, requestID)
		audit.InputTokens, audit.OutputTokens = usage.Input, usage.Output
		audit.CachedTokens, audit.ReasoningTokens = usage.Cached, usage.Reasoning
		audit.Error = errorText
		return
	}
	if streaming && response.StatusCode >= 200 && response.StatusCode < 300 {
		usage, errorText := p.streamResponse(w, response, route, markFirstToken)
		audit.InputTokens, audit.OutputTokens = usage.Input, usage.Output
		audit.CachedTokens, audit.ReasoningTokens = usage.Cached, usage.Reasoning
		audit.Error = errorText
		return
	}
	usage, errorText := p.bufferedResponse(w, response, route, requestID)
	audit.InputTokens, audit.OutputTokens = usage.Input, usage.Output
	audit.CachedTokens, audit.ReasoningTokens = usage.Cached, usage.Reasoning
	audit.Error = errorText
}

func applySystemPrompt(payload map[string]json.RawMessage, path, model, template string) error {
	prompt := strings.ReplaceAll(template, "{model}", model)
	if strings.TrimSpace(prompt) == "" {
		return nil
	}
	switch path {
	case "/v1/chat/completions":
		var messages []any
		raw := payload["messages"]
		if raw != nil {
			if err := json.Unmarshal(raw, &messages); err != nil {
				return errors.New("messages must be an array")
			}
		}
		messages = append([]any{map[string]any{"role": "system", "content": prompt}}, messages...)
		encoded, err := json.Marshal(messages)
		if err != nil {
			return errors.New("could not encode system prompt")
		}
		payload["messages"] = encoded
	case "/v1/responses":
		var instructions string
		if raw := payload["instructions"]; raw != nil {
			if err := json.Unmarshal(raw, &instructions); err != nil {
				return errors.New("instructions must be a string")
			}
		}
		if strings.TrimSpace(instructions) != "" {
			prompt += "\n\n" + instructions
		}
		encoded, err := json.Marshal(prompt)
		if err != nil {
			return errors.New("could not encode system prompt")
		}
		payload["instructions"] = encoded
	}
	return nil
}

func (p *Proxy) doUpstreamRequest(ctx context.Context, route ModelRoute, path string, body []byte, streaming bool, requestID string) (*http.Request, *http.Response, error) {
	upstreamURL, err := joinUpstreamURL(route.Model.BaseURL, path)
	if err != nil {
		return nil, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+route.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if streaming {
		request.Header.Set("Accept", "text/event-stream")
	}
	request.Header.Set("User-Agent", p.config.Snapshot().UserAgent)
	request.Header.Set("X-Request-Id", requestID)
	if err := p.applySignature(ctx, request, body, route); err != nil {
		return request, nil, &upstreamRequestError{code: "signing_error", err: fmt.Errorf("sign upstream request: %w", err)}
	}
	response, err := p.client.Do(request)
	if err != nil {
		return request, nil, &upstreamRequestError{code: "upstream_error", err: fmt.Errorf("upstream request failed: %w", err)}
	}
	return request, response, nil
}

func (p *Proxy) bufferedChatAsResponses(w http.ResponseWriter, response *http.Response, route ModelRoute, context *responsesCompatContext, requestID string) (tokenUsage, string) {
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", upstreamFailureMessage(http.StatusBadGateway, requestID))
		return tokenUsage{}, "could not read upstream response"
	}
	converted, usage, err := chatResponseToResponses(body, route.Requested, context)
	if err != nil {
		message := "could not convert Chat Completions response: " + err.Error()
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", upstreamFailureMessage(http.StatusBadGateway, requestID))
		return tokenUsage{}, message
	}
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(converted)
	return usage, ""
}

func (p *Proxy) bufferedResponse(w http.ResponseWriter, response *http.Response, route ModelRoute, requestID string) (tokenUsage, string) {
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", upstreamFailureMessage(http.StatusBadGateway, requestID))
		return tokenUsage{}, "could not read upstream response"
	}
	usage := extractUsage(body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		errorText := compactError(body)
		w.Header().Del("Content-Length")
		writeOpenAIError(w, response.StatusCode, "upstream_error", upstreamFailureMessage(response.StatusCode, requestID))
		return usage, errorText
	}
	body = rewriteResponseModel(body, route.Requested)
	w.Header().Del("Content-Length")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
	return usage, ""
}

func (p *Proxy) streamResponse(w http.ResponseWriter, response *http.Response, route ModelRoute, markFirstToken func()) (tokenUsage, string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming is not supported by this server")
		return tokenUsage{}, "streaming is not supported by this server"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Del("Content-Length")
	w.WriteHeader(response.StatusCode)
	reader := bufio.NewReader(response.Body)
	var usage tokenUsage
	errorText := ""
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
				if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
					if streamChunkHasOutput(data) {
						markFirstToken()
					}
					chunkUsage := extractUsage(data)
					if chunkUsage.Input > 0 || chunkUsage.Output > 0 {
						usage = chunkUsage
					}
					rewritten := rewriteResponseModel(data, route.Requested)
					line = append(append([]byte("data: "), rewritten...), '\n')
				}
			}
			if _, writeErr := w.Write(line); writeErr != nil {
				errorText = "client disconnected"
				break
			}
			flusher.Flush()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				errorText = err.Error()
			}
			break
		}
	}
	return usage, errorText
}

func streamChunkHasOutput(data []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return false
	}
	if eventType, _ := payload["type"].(string); strings.HasSuffix(eventType, ".delta") && hasStreamDelta(payload["delta"]) {
		return true
	}
	choices, _ := payload["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		for _, field := range []string{"content", "reasoning_content", "reasoning"} {
			if hasStreamDelta(delta[field]) {
				return true
			}
		}
		if toolCalls, ok := delta["tool_calls"].([]any); ok && len(toolCalls) > 0 {
			return true
		}
	}
	return false
}

func hasStreamDelta(value any) bool {
	switch typed := value.(type) {
	case string:
		return typed != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return false
	}
}

func (p *Proxy) applySignature(ctx context.Context, request *http.Request, body []byte, route ModelRoute) error {
	if !isAtomGitGateway(request.URL) {
		return nil
	}
	config := p.config.Snapshot()
	if strings.TrimSpace(config.SignerURL) == "" {
		version := strings.TrimSpace(strings.TrimPrefix(config.UserAgent, "atomcode/"))
		if version == "" || version == config.UserAgent {
			return errors.New("user_agent must use the atomcode/<version> format for built-in signing")
		}
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return fmt.Errorf("generate signing nonce: %w", err)
		}
		headers, err := signAtomGitRequest(request.Method, request.URL.EscapedPath(), body, route.Token, route.Account.User.ID, version, time.Now().Unix(), nonce, "")
		if err != nil {
			return err
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		return nil
	}
	payload := map[string]any{
		"method": request.Method, "path": request.URL.EscapedPath(), "body": base64.RawStdEncoding.EncodeToString(body),
		"oauth_token": route.Token, "user_id": route.Account.User.ID,
		"timestamp_unix": time.Now().Unix(), "nonce": secureNonce(16), "client_version": strings.TrimPrefix(config.UserAgent, "atomcode/"),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	signRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, config.SignerURL, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	signRequest.Header.Set("Content-Type", "application/json")
	if config.SignerToken != "" {
		signRequest.Header.Set("Authorization", "Bearer "+config.SignerToken)
	}
	response, err := p.client.Do(signRequest)
	if err != nil {
		return fmt.Errorf("request signer failed: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("request signer returned %d: %s", response.StatusCode, compactError(responseBody))
	}
	var signed struct {
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(responseBody, &signed); err != nil {
		return fmt.Errorf("decode signer response: %w", err)
	}
	if len(signed.Headers) == 0 {
		return errors.New("request signer returned no headers")
	}
	for name, value := range signed.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "" || canonical == "Authorization" || canonical == "Host" || canonical == "Content-Length" || isHopByHop(canonical) {
			continue
		}
		request.Header.Set(canonical, value)
	}
	return nil
}

// atomcode-signing-v1 was independently documented by the MIT-licensed
// atomgit-opencode-bridge project. Keep the primitive isolated so the master
// key can be replaced without changing proxy or credential storage code.
func signAtomGitRequest(method, path string, body []byte, oauthToken, userID, clientVersion string, timestamp int64, nonce []byte, masterKeyHex string) (map[string]string, error) {
	if oauthToken == "" || userID == "" {
		return nil, errors.New("AtomGit signing requires an OAuth token and user id")
	}
	if len(nonce) != 16 {
		return nil, errors.New("AtomGit signing nonce must be 16 bytes")
	}
	if masterKeyHex == "" {
		masterKeyHex = "e97250f05303162c8ecd68c688b2f55c1d81e508d243d88466472e7f54637123"
	}
	masterKey, err := hex.DecodeString(masterKeyHex)
	if err != nil || len(masterKey) != 32 {
		return nil, errors.New("AtomGit signing master key must be 32-byte hex")
	}
	tokenHash := sha256.Sum256([]byte(oauthToken))
	versionHash := sha256.Sum256([]byte(clientVersion))
	salt := make([]byte, 0, len(userID)+1+8+sha256.Size*2)
	salt = append(salt, []byte(userID)...)
	salt = append(salt, 1)
	timeBucket := make([]byte, 8)
	binary.LittleEndian.PutUint64(timeBucket, uint64(timestamp/3600))
	salt = append(salt, timeBucket...)
	salt = append(salt, tokenHash[:]...)
	salt = append(salt, versionHash[:]...)

	// HKDF-SHA256 extract + one expand block (the requested output is 32 bytes).
	extract := hmac.New(sha256.New, salt)
	_, _ = extract.Write(masterKey)
	pseudorandomKey := extract.Sum(nil)
	expand := hmac.New(sha256.New, pseudorandomKey)
	_, _ = expand.Write([]byte("atomcode-signing-v1"))
	_, _ = expand.Write([]byte{1})
	signingKey := expand.Sum(nil)

	bodyHash := sha256.Sum256(body)
	nonceHex := hex.EncodeToString(nonce)
	canonical := strings.Join([]string{
		"v1", strings.ToUpper(method), path, fmt.Sprintf("%d", timestamp), nonceHex, hex.EncodeToString(bodyHash[:]),
	}, "\n")
	signer := hmac.New(sha256.New, signingKey)
	_, _ = signer.Write([]byte(canonical))
	signature := hex.EncodeToString(signer.Sum(nil))
	return map[string]string{
		"X-AtomCode-Sig":   "v1:" + signature,
		"X-AtomCode-Ts":    fmt.Sprintf("%d", timestamp),
		"X-AtomCode-Nonce": nonceHex,
		"X-AtomCode-Alg":   "1",
		"X-AtomCode-Ver":   clientVersion,
	}, nil
}

func extractUsage(data []byte) tokenUsage {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil {
		return tokenUsage{}
	}
	if raw := envelope["usage"]; raw != nil {
		return parseUsageObject(raw)
	}
	if raw := envelope["response"]; raw != nil {
		var response map[string]json.RawMessage
		if json.Unmarshal(raw, &response) == nil && response["usage"] != nil {
			return parseUsageObject(response["usage"])
		}
	}
	return tokenUsage{}
}

func parseUsageObject(raw json.RawMessage) tokenUsage {
	var usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		InputTokens      int64 `json:"input_tokens"`
		OutputTokens     int64 `json:"output_tokens"`
		PromptDetails    struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		InputDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		CompletionDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		OutputDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	}
	if json.Unmarshal(raw, &usage) != nil {
		return tokenUsage{}
	}
	input := usage.PromptTokens
	if input == 0 {
		input = usage.InputTokens
	}
	output := usage.CompletionTokens
	if output == 0 {
		output = usage.OutputTokens
	}
	cached := usage.PromptDetails.CachedTokens
	if cached == 0 {
		cached = usage.InputDetails.CachedTokens
	}
	reasoning := usage.CompletionDetails.ReasoningTokens
	if reasoning == 0 {
		reasoning = usage.OutputDetails.ReasoningTokens
	}
	return tokenUsage{Input: input, Output: output, Cached: cached, Reasoning: reasoning}
}

func rewriteResponseModel(data []byte, model string) []byte {
	if model == "" {
		return data
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil {
		return data
	}
	encodedModel, _ := json.Marshal(model)
	changed := false
	if _, exists := envelope["model"]; exists {
		envelope["model"] = encodedModel
		changed = true
	}
	if raw := envelope["response"]; raw != nil {
		var response map[string]json.RawMessage
		if json.Unmarshal(raw, &response) == nil {
			if _, exists := response["model"]; exists {
				response["model"] = encodedModel
				envelope["response"], _ = json.Marshal(response)
				changed = true
			}
		}
	}
	if !changed {
		return data
	}
	rewritten, err := json.Marshal(envelope)
	if err != nil {
		return data
	}
	return rewritten
}

func joinUpstreamURL(base, requestPath string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("account model has an invalid upstream URL")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	path := requestPath
	if strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}
	parsed.Path = basePath + "/" + strings.TrimLeft(path, "/")
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func isAtomGitGateway(endpoint *url.URL) bool {
	if endpoint == nil || endpoint.Scheme != "https" {
		return false
	}
	switch strings.ToLower(endpoint.Hostname()) {
	case "llm-api.atomgit.com", "pre-llm-api-cce.atomgit.com", "api-ai.gitcode.com":
		return true
	default:
		return false
	}
}

func copyUpstreamHeaders(destination, source http.Header) {
	for _, name := range []string{"Content-Type", "Cache-Control", "Retry-After", "OpenAI-Request-ID", "X-Request-ID"} {
		for _, value := range source.Values(name) {
			destination.Add(name, value)
		}
	}
}

func auditHeaders(headers http.Header) map[string][]string {
	if len(headers) == 0 {
		return nil
	}
	sanitized := make(map[string][]string, len(headers))
	for name, values := range headers {
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "" {
			continue
		}
		if isSensitiveAuditHeader(canonical) {
			sanitized[canonical] = []string{"[REDACTED]"}
			continue
		}
		sanitized[canonical] = append([]string(nil), values...)
	}
	return sanitized
}

func isSensitiveAuditHeader(name string) bool {
	lower := strings.ToLower(name)
	if lower == "authorization" || lower == "proxy-authorization" || lower == "cookie" || lower == "set-cookie" {
		return true
	}
	return strings.Contains(lower, "api-key") || strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "token") || strings.Contains(lower, "secret") ||
		strings.Contains(lower, "credential") || strings.Contains(lower, "password") ||
		strings.Contains(lower, "access-key") || strings.Contains(lower, "auth-key") ||
		strings.Contains(lower, "private-key") || strings.Contains(lower, "signature") ||
		strings.HasSuffix(lower, "-sig")
}

func isHopByHop(name string) bool {
	switch name {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": message, "type": code, "param": nil, "code": code,
	}})
}

func upstreamFailureMessage(status int, requestID string) string {
	return fmt.Sprintf("status_code=%d,upstream request failed. request_id=%s", status, requestID)
}

func (p *Proxy) record(record UsageRecord) {
	if err := p.store.RecordUsage(record); err != nil {
		log.Printf("record usage: %v", err)
	}
}

func (p *Proxy) disableAccount(accountID, reason string) {
	_, err := p.store.UpdateAccount(accountID, func(account *Account) error {
		account.Enabled = false
		account.Status = "error"
		account.LastError = reason
		return nil
	})
	if err != nil {
		log.Printf("disable unavailable account %s: %v", accountID, err)
	}
}
