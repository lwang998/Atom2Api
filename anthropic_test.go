package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAnthropicToChatPayloadConvertsBlocksAndTools(t *testing.T) {
	req := &anthropicRequest{
		Model:  "glm5.3-flash[1M]",
		System: []byte(`[{"type":"text","text":"You are helpful."}]`),
		Messages: []anthropicMessage{
			{Role: "user", Content: []byte(`"What is the weather?"`)},
			{Role: "assistant", Content: []byte(`[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}]`)},
			{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"},{"type":"text","text":"Thanks!"}]`)},
		},
		MaxTokens: 512,
		Tools:     []byte(`[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]`),
	}
	payload, err := anthropicToChatPayload(req, "glm5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "glm5.3-flash" || payload["max_tokens"] != 512 {
		t.Fatalf("model/max_tokens = %v/%v", payload["model"], payload["max_tokens"])
	}
	messages := payload["messages"].([]map[string]any)
	if len(messages) != 5 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0]["role"] != "system" || messages[0]["content"] != "You are helpful." {
		t.Fatalf("system message = %#v", messages[0])
	}
	toolCalls, _ := messages[2]["tool_calls"].([]map[string]any)
	if len(toolCalls) != 1 || toolCalls[0]["id"] != "toolu_1" {
		t.Fatalf("assistant tool_calls = %#v", messages[2])
	}
	if messages[3]["role"] != "tool" || messages[3]["tool_call_id"] != "toolu_1" || messages[3]["content"] != "sunny" {
		t.Fatalf("tool result message = %#v", messages[3])
	}
	if messages[4]["role"] != "user" {
		t.Fatalf("trailing user message = %#v", messages[4])
	}
	tools := payload["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["type"] != "function" {
		t.Fatalf("tools = %#v", tools)
	}
	function := tools[0]["function"].(map[string]any)
	parameters := function["parameters"].(json.RawMessage)
	if !strings.Contains(string(parameters), `"city"`) || !strings.Contains(string(parameters), `"string"`) {
		t.Fatalf("tool parameters = %s", parameters)
	}
}

func TestAnthropicStreamTranslationEmitsAnthropicEvents(t *testing.T) {
	upstream := newOpenAISSEUpstream()
	defer upstream.Close()
	resp, err := upstream.Client().Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	recorder := httptest.NewRecorder()
	proxy := &Proxy{}
	firstTokenMarks := 0
	usage, errorText := proxy.streamAnthropicResponse(recorder, resp, "glm5.3-flash", func() {
		firstTokenMarks++
	})
	if errorText != "" {
		t.Fatalf("errorText = %q", errorText)
	}
	if usage.Input != 12 || usage.Output != 34 {
		t.Fatalf("usage = %#v", usage)
	}
	if firstTokenMarks == 0 {
		t.Fatal("markFirstToken was never called for a stream with output deltas")
	}

	var events []string
	var dataLines []string
	for _, block := range strings.Split(strings.TrimSpace(recorder.Body.String()), "\n\n") {
		lines := strings.Split(block, "\n")
		events = append(events, strings.TrimPrefix(lines[0], "event: "))
		dataLines = append(dataLines, strings.TrimPrefix(lines[1], "data: "))
	}
	expectOrder := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // thinking
		"content_block_start", "content_block_delta", "content_block_stop", // text
		"content_block_start", "content_block_delta", "content_block_stop", // tool_use
		"message_delta", "message_stop",
	}
	if strings.Join(events, "|") != strings.Join(expectOrder, "|") {
		t.Fatalf("event order = %v", events)
	}
	joined := strings.Join(dataLines, "\n")
	for _, want := range []string{`"type":"thinking"`, `"type":"thinking_delta"`, `"text_delta"`, `"tool_use"`, `"id":"call_1"`, `"name":"get_weather"`, `"input_json_delta"`, `"stop_reason":"tool_use"`, `"output_tokens":34`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("stream missing %s in:\n%s", want, joined)
		}
	}
}

func TestAnthropicStreamTranslationKeepsInterleavedToolArgumentsSeparate(t *testing.T) {
	recorder := httptest.NewRecorder()
	e := &anthropicEmitter{w: recorder, flusher: recorder, tools: map[int]*anthropicStreamTool{}}
	for _, chunk := range []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_0","function":{"name":"first","arguments":"{\"a\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_1","function":{"name":"second","arguments":"{\"b\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
	} {
		e.translateChunk([]byte(chunk))
	}
	e.flushTools()
	e.closeBlock()
	body := recorder.Body.String()
	if !strings.Contains(body, `"id":"call_0"`) || !strings.Contains(body, `"partial_json":"{\"a\":1}"`) {
		t.Fatalf("first tool stream = %s", body)
	}
	if !strings.Contains(body, `"id":"call_1"`) || !strings.Contains(body, `"partial_json":"{\"b\":2}"`) {
		t.Fatalf("second tool stream = %s", body)
	}
}

func TestAnthropicBufferedResponseMapsToolArguments(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		payload := `{"choices":[{"message":{"content":"","tool_calls":[` +
			`{"id":"call_a","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls -la\"}"}},` +
			`{"id":"call_b","type":"function","function":{"name":"noargs","arguments":""}},` +
			`{"id":"call_c","type":"function","function":{"name":"broken","arguments":"{\"command\":"}}` +
			`]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":9}}`
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()
	resp, err := upstream.Client().Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	recorder := httptest.NewRecorder()
	proxy := &Proxy{}
	usage, errorText := proxy.bufferedAnthropicResponse(recorder, resp, "glm5.3-flash")
	if errorText != "" {
		t.Fatalf("errorText = %q", errorText)
	}
	if usage.Input != 3 || usage.Output != 9 {
		t.Fatalf("usage = %#v", usage)
	}
	var message struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	if message.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", message.StopReason)
	}
	if len(message.Content) != 3 {
		t.Fatalf("content blocks = %#v", message.Content)
	}
	wantInput := map[string]string{
		"call_a": `{"command":"ls -la"}`,
		"call_b": `{}`,
		"call_c": `{"_raw_arguments":"{\"command\":"}`,
	}
	for _, block := range message.Content {
		if block.Type != "tool_use" {
			t.Fatalf("block type = %q, want tool_use", block.Type)
		}
		want, ok := wantInput[block.ID]
		if !ok {
			t.Fatalf("unexpected tool_use id %q", block.ID)
		}
		if string(block.Input) != want {
			t.Fatalf("tool %s input = %s, want %s", block.ID, block.Input, want)
		}
	}
}

func TestAnthropicMessagesRecordsStreamingLatency(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"Hello"}}]}` + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(60 * time.Millisecond)
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{},"finish_reason":"end_turn"}],"usage":{"prompt_tokens":5,"completion_tokens":7}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	config, store := newTestStore(t)
	addTestAccount(t, store, upstream.URL)
	_, secret, err := store.CreateAPIKey("latency", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy := NewProxy(config, store, NewModelRouter(store), nil)
	api := NewAPI(store, nil, nil, proxy.router, proxy)

	requestBody := `{"model":"upstream-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
	request.Header.Set("X-API-Key", secret)
	response := httptest.NewRecorder()
	api.RequireAPIKey(proxy.HandleAnthropicMessages).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	records := store.UsageRecords()
	if len(records) != 1 {
		t.Fatalf("usage records = %#v", records)
	}
	record := records[0]
	if !record.Streaming {
		t.Fatal("record.Streaming = false, want true")
	}
	if record.CompletionLatencyMS < 30 {
		t.Fatalf("CompletionLatencyMS = %d, want >= 30 (first-token timing not recorded)", record.CompletionLatencyMS)
	}
	if record.LatencyMS < record.CompletionLatencyMS {
		t.Fatalf("LatencyMS = %d < CompletionLatencyMS = %d", record.LatencyMS, record.CompletionLatencyMS)
	}
	if record.InputTokens != 5 || record.OutputTokens != 7 {
		t.Fatalf("tokens = %d/%d, want 5/7", record.InputTokens, record.OutputTokens)
	}
}
