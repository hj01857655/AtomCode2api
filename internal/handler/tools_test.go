// 工具调用往返：三协议 × 请求翻译 / 响应解析 / 多轮回传。
//
// 上游样本刻意把 arguments 拆成多片——整块下发时拼接逻辑根本没被执行到，
// 分片才是真实上游的形态。
package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 上游：一次完整的工具调用（参数分三片），收尾 finish_reason=tool_calls。
var toolStream = concat(
	chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil, "c4"),
	chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "call_abc", "type": "function",
		"function": map[string]any{"name": "get_weather", "arguments": ""},
	}}}, nil, nil, "c4"),
	chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "function": map[string]any{"arguments": `{"city": `},
	}}}, nil, nil, "c4"),
	chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "function": map[string]any{"arguments": `"Beijing"}`},
	}}}, nil, nil, "c4"),
	chunk(map[string]any{}, "tool_calls", nil, "c4"),
	chunk(map[string]any{}, nil, sseUsage, "c4"),
	[]byte("data: [DONE]\n\n"),
)

// 上游：工具调用完成的非流式响应。
var toolJSONBytes = marshalCompactJSON(map[string]any{
	"id": "c5", "object": "chat.completion", "created": 1, "model": "m",
	"choices": []any{map[string]any{
		"index": 0, "finish_reason": "tool_calls",
		"message": map[string]any{"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{
				"id": "call_abc", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"city": "Beijing"}`},
			}}},
	}},
	"usage": sseUsage,
})

var anthropicToolsReq = []any{map[string]any{
	"name": "get_weather", "description": "Get weather for a city",
	"input_schema": map[string]any{"type": "object",
		"properties": map[string]any{"city": map[string]any{"type": "string"}},
		"required":   []any{"city"}},
}}

// parseAntFrames 解析 Anthropic SSE 帧（event: / data: 两行一组）。
func parseAntFrames(t *testing.T, raw []byte) []struct {
	event string
	data  map[string]any
} {
	t.Helper()
	var out []struct {
		event string
		data  map[string]any
	}
	for _, block := range strings.Split(string(raw), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev, payload string
		for _, ln := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(ln, "event:"); ok {
				ev = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(ln, "data:"); ok {
				payload = strings.TrimSpace(v)
			}
		}
		if payload == "" {
			continue
		}
		var o map[string]any
		if err := json.Unmarshal([]byte(payload), &o); err != nil {
			t.Fatalf("Anthropic SSE 帧解析失败: %v\n%s", err, payload)
		}
		out = append(out, struct {
			event string
			data  map[string]any
		}{ev, o})
	}
	return out
}

// Anthropic 请求侧：tools(input_schema) -> Chat tools(function.parameters)，
// tool_choice{type:tool} -> {type:function}，assistant.tool_use -> tool_calls，
// user.tool_result -> role=tool。
func TestAnthropicRequestToolTranslation(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = toolJSONBytes
	r := fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "qwen3.8-27b", "max_tokens": 512,
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "weather?"}}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "call_prev",
					"name": "get_weather", "input": map[string]any{"city": "Beijing"}}}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_prev",
					"content": "Beijing: 18C"}}},
		},
		"tools":       anthropicToolsReq,
		"tool_choice": map[string]any{"type": "tool", "name": "get_weather"},
	}, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	cb := fx.fake.last(t).payload

	tools := asList(cb["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools=%v", cb["tools"])
	}
	fn := asMap(asMap(tools[0])["function"])
	if fn == nil || fn["name"] != "get_weather" {
		t.Fatalf("tools[0].function=%v", asMap(tools[0])["function"])
	}
	if _, ok := asMap(fn["parameters"])["properties"]; !ok {
		t.Errorf("input_schema 未搬到 parameters: %v", fn["parameters"])
	}

	tc := asMap(cb["tool_choice"])
	if tc == nil || tc["type"] != "function" {
		t.Fatalf("tool_choice=%v", cb["tool_choice"])
	}
	if got := asString(asMap(tc["function"])["name"]); got != "get_weather" {
		t.Errorf("tool_choice.function.name=%q", got)
	}

	msgs := asList(cb["messages"])
	if len(msgs) != 3 {
		t.Fatalf("messages 条数=%d，期望 3（user/assistant/tool）：%v", len(msgs), cb["messages"])
	}
	asst := asMap(msgs[1])
	calls := asList(asst["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("assistant.tool_calls=%v", asst["tool_calls"])
	}
	call := asMap(calls[0])
	if call["id"] != "call_prev" || asString(asMap(call["function"])["name"]) != "get_weather" {
		t.Errorf("tool_calls[0]=%v", call)
	}
	if args := asString(asMap(call["function"])["arguments"]); !strings.Contains(args, "Beijing") {
		t.Errorf("tool_use.input 未序列化成 arguments: %q", args)
	}
	toolMsg := asMap(msgs[2])
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_prev" {
		t.Errorf("tool_result 未翻成 role=tool: %v", toolMsg)
	}
	if c := asString(toolMsg["content"]); c != "Beijing: 18C" {
		t.Errorf("tool 消息 content=%q", c)
	}
}

// Anthropic 非流式响应：Chat tool_calls -> content[].tool_use + stop_reason=tool_use。
func TestAnthropicNonStreamToolUse(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = toolJSONBytes
	r := fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "qwen3.8-27b", "max_tokens": 512,
		"messages": []any{map[string]any{"role": "user", "content": "weather?"}},
		"tools":    anthropicToolsReq,
	}, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	var msg map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v，期望 tool_use", msg["stop_reason"])
	}
	blocks := asList(msg["content"])
	if len(blocks) != 1 {
		t.Fatalf("content=%v", msg["content"])
	}
	b := asMap(blocks[0])
	if b["type"] != "tool_use" || b["name"] != "get_weather" {
		t.Fatalf("content[0]=%v", b)
	}
	// id 必须沿用上游 call id：客户端靠它把 tool_result 配回本轮调用
	if b["id"] != "call_abc" {
		t.Errorf("tool_use.id=%v，应透传上游 call id（否则 tool_result 对不上）", b["id"])
	}
	// arguments 必须被解析成结构化 input，不是原样字符串
	inp := asMap(b["input"])
	if inp == nil || asString(inp["city"]) != "Beijing" {
		t.Errorf("tool_use.input=%v（arguments 未解析成对象）", b["input"])
	}
}

// Anthropic 流式响应：tool_calls 分片 -> tool_use 块 + input_json_delta 拼接。
func TestAnthropicStreamToolUse(t *testing.T) {
	fx := newFixture(t, "stream")
	fx.fake.streamBody = toolStream
	r := fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "qwen3.8-27b", "max_tokens": 512, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "weather?"}},
		"tools":    anthropicToolsReq,
	}, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	frames := parseAntFrames(t, r.Body.Bytes())

	var blockType, blockName, blockID, partial, stopReason string
	var sawStop bool
	for _, f := range frames {
		switch f.event {
		case "content_block_start":
			cb := asMap(f.data["content_block"])
			blockType, blockName = asString(cb["type"]), asString(cb["name"])
			blockID = asString(cb["id"])
		case "content_block_delta":
			d := asMap(f.data["delta"])
			if d["type"] == "input_json_delta" {
				partial += asString(d["partial_json"])
			}
		case "content_block_stop":
			sawStop = true
		case "message_delta":
			stopReason = asString(asMap(f.data["delta"])["stop_reason"])
		}
	}
	if blockType != "tool_use" {
		t.Errorf("content_block_start.type=%q，期望 tool_use", blockType)
	}
	if blockName != "get_weather" {
		t.Errorf("tool_use.name=%q", blockName)
	}
	if blockID != "call_abc" {
		t.Errorf("tool_use.id=%q，应透传上游 call id", blockID)
	}
	if !sawStop {
		t.Error("缺 content_block_stop")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(partial), &got); err != nil {
		t.Fatalf("拼接后的 partial_json 不是合法 JSON: %q", partial)
	}
	if asString(got["city"]) != "Beijing" {
		t.Errorf("拼接参数=%v", got)
	}
	if stopReason != "tool_use" {
		t.Errorf("message_delta.stop_reason=%q，期望 tool_use", stopReason)
	}
}

// Chat 请求侧：tools/tool_choice 原样透传；响应侧 tool_calls 分片原样出现。
func TestChatToolPassthrough(t *testing.T) {
	fx := newFixture(t, "stream")
	fx.fake.streamBody = toolStream
	r := fx.do(t, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "qwen3.8-27b", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "weather?"}},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "get_weather", "description": "w",
			"parameters": map[string]any{"type": "object"}}}},
		"tool_choice": "auto",
	}, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	cb := fx.fake.last(t).payload
	if len(asList(cb["tools"])) != 1 || cb["tool_choice"] != "auto" {
		t.Errorf("Chat tools/tool_choice 未透传: tools=%v choice=%v", cb["tools"], cb["tool_choice"])
	}
	body := r.Body.String()
	if !strings.Contains(body, `"tool_calls"`) {
		t.Errorf("Chat 流式响应缺 tool_calls:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("Chat 流式响应缺 finish_reason=tool_calls")
	}
}

// Responses 请求侧：tools[] -> Chat tools；function_call -> assistant.tool_calls；
// function_call_output -> role=tool。
func TestResponsesRequestToolTranslation(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = toolJSONBytes
	r := fx.do(t, http.MethodPost, "/v1/responses", map[string]any{
		"model": "qwen3.8-27b", "stream": false,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "weather?"}}},
			map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_prev",
				"name": "get_weather", "arguments": `{"city": "Beijing"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_prev",
				"output": "Beijing: 18C"},
		},
		"tools": []any{map[string]any{"type": "function", "name": "get_weather",
			"description": "w", "parameters": map[string]any{"type": "object"}}},
	}, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	cb := fx.fake.last(t).payload

	tools := asList(cb["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools=%v", cb["tools"])
	}
	if got := asString(asMap(asMap(tools[0])["function"])["name"]); got != "get_weather" {
		t.Errorf("tools[0].function.name=%q", got)
	}

	msgs := asList(cb["messages"])
	if len(msgs) != 3 {
		t.Fatalf("messages 条数=%d，期望 3：%v", len(msgs), cb["messages"])
	}
	asst := asMap(msgs[1])
	calls := asList(asst["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("function_call 未合并成 assistant.tool_calls: %v", asst)
	}
	if asMap(calls[0])["id"] != "call_prev" {
		t.Errorf("tool_calls[0].id=%v，期望沿用 call_id", asMap(calls[0])["id"])
	}
	toolMsg := asMap(msgs[2])
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_prev" {
		t.Errorf("function_call_output 未翻成 role=tool: %v", toolMsg)
	}
}

// 并行工具调用：两个 tool_calls 各自独立成块，id 互不串。
// 上游样本让 index 0/1 交错分片——单工具时拼接逻辑过了，交错才是真实形态。
func TestAnthropicParallelToolCalls(t *testing.T) {
	stream := concat(
		chunk(map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_a", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"city": `}},
			map[string]any{"index": 1, "id": "call_b", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"city": `}},
		}}, nil, nil, "c6"),
		chunk(map[string]any{"tool_calls": []any{
			map[string]any{"index": 1, "function": map[string]any{"arguments": `"Tokyo"}`}},
			map[string]any{"index": 0, "function": map[string]any{"arguments": `"Beijing"}`}},
		}}, nil, nil, "c6"),
		chunk(map[string]any{}, "tool_calls", nil, "c6"),
		[]byte("data: [DONE]\n\n"),
	)
	fx := newFixture(t, "stream")
	fx.fake.streamBody = stream
	r := fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "qwen3.8-27b", "max_tokens": 256, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "both cities?"}},
		"tools":    anthropicToolsReq,
	}, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	argsByID := map[string]string{}
	idByBlock := map[int]string{}
	for _, f := range parseAntFrames(t, r.Body.Bytes()) {
		switch f.event {
		case "content_block_start":
			cb := asMap(f.data["content_block"])
			if asString(cb["type"]) == "tool_use" {
				idByBlock[idxOf(f.data["index"])] = asString(cb["id"])
			}
		case "content_block_delta":
			d := asMap(f.data["delta"])
			if d["type"] == "input_json_delta" {
				idx := idxOf(f.data["index"])
				argsByID[idByBlock[idx]] += asString(d["partial_json"])
			}
		}
	}
	if len(idByBlock) != 2 {
		t.Fatalf("tool_use 块数=%d，期望 2：%v", len(idByBlock), idByBlock)
	}
	want := map[string]string{"call_a": "Beijing", "call_b": "Tokyo"}
	for id, city := range want {
		var got map[string]any
		if err := json.Unmarshal([]byte(argsByID[id]), &got); err != nil {
			t.Fatalf("%s 参数不是合法 JSON: %q", id, argsByID[id])
		}
		if asString(got["city"]) != city {
			t.Errorf("%s 的 city=%v，期望 %s（参数串块了）", id, got["city"], city)
		}
	}
}

// tool_choice 三态映射：none 抑制、any->required、{type:tool}->{type:function}。
func TestAnthropicToolChoiceMapping(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"auto", map[string]any{"type": "auto"}, "auto"},
		{"none", map[string]any{"type": "none"}, "none"},
		{"any", map[string]any{"type": "any"}, "required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t, "json")
			fx.fake.jsonBody = toolJSONBytes
			r := fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
				"model": "qwen3.8-27b", "max_tokens": 64,
				"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
				"tools":       anthropicToolsReq,
				"tool_choice": c.in,
			}, nil)
			if r.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
			}
			if got := fx.fake.last(t).payload["tool_choice"]; got != c.want {
				t.Errorf("tool_choice=%v，期望 %v", got, c.want)
			}
		})
	}
}

// tool_result.content 既可能是字符串，也可能是块数组——两种都必须能取到文本。
func TestAnthropicToolResultContentShapes(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = toolJSONBytes
	fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "qwen3.8-27b", "max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "user", "content": "both?"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "call_a", "name": "get_weather",
					"input": map[string]any{"city": "Beijing"}},
				map[string]any{"type": "tool_use", "id": "call_b", "name": "get_weather",
					"input": map[string]any{"city": "Tokyo"}}}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_a",
					"content": []any{map[string]any{"type": "text", "text": "Beijing: 18C"}}},
				map[string]any{"type": "tool_result", "tool_use_id": "call_b",
					"content": "Tokyo: 22C"}}},
		},
	}, nil)
	msgs := asList(fx.fake.last(t).payload["messages"])
	var got []string
	for _, mi := range msgs {
		if m := asMap(mi); m["role"] == "tool" {
			got = append(got, asString(m["tool_call_id"])+"="+asString(m["content"]))
		}
	}
	want := []string{"call_a=Beijing: 18C", "call_b=Tokyo: 22C"}
	if len(got) != len(want) {
		t.Fatalf("tool 消息=%v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tool[%d]=%q，期望 %q", i, got[i], want[i])
		}
	}
}

// 无 input_schema 的 tool 必须补空 schema，不能整条丢弃。
func TestAnthropicToolWithoutInputSchema(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = toolJSONBytes
	fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "qwen3.8-27b", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    []any{map[string]any{"name": "noargs", "description": "no params"}},
	}, nil)
	tools := asList(fx.fake.last(t).payload["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools=%v，无 schema 的 tool 不应被丢弃", fx.fake.last(t).payload["tools"])
	}
	fn := asMap(asMap(tools[0])["function"])
	if fn["name"] != "noargs" {
		t.Errorf("tools[0].function.name=%v", fn["name"])
	}
	if p := asMap(fn["parameters"]); p == nil || p["type"] != "object" {
		t.Errorf("无 input_schema 时应补空 object schema，得到 %v", fn["parameters"])
	}
}

// max_tokens 截断：Chat finish_reason=length -> Anthropic stop_reason=max_tokens。
func TestAnthropicStopReasonMaxTokens(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = marshalCompactJSON(map[string]any{
		"id": "c7", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "length",
			"message": map[string]any{"role": "assistant", "content": "1 2 3"}}},
		"usage": sseUsage,
	})
	r := fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "qwen3.8-27b", "max_tokens": 16,
		"messages": []any{map[string]any{"role": "user", "content": "count"}},
	}, nil)
	var msg map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason=%v，期望 max_tokens", msg["stop_reason"])
	}
}

// Responses 非流式响应：Chat tool_calls -> output[].function_call。
func TestResponsesNonStreamFunctionCall(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = toolJSONBytes
	r := fx.do(t, http.MethodPost, "/v1/responses", map[string]any{
		"model": "qwen3.8-27b", "stream": false,
		"input": "weather?",
		"tools": []any{map[string]any{"type": "function", "name": "get_weather",
			"parameters": map[string]any{"type": "object"}}},
	}, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	items := asList(out["output"])
	if len(items) != 1 {
		t.Fatalf("output=%v", out["output"])
	}
	it := asMap(items[0])
	if it["type"] != "function_call" {
		t.Fatalf("output[0].type=%v，期望 function_call", it["type"])
	}
	if it["call_id"] != "call_abc" || it["name"] != "get_weather" {
		t.Errorf("function_call=%v", it)
	}
	if args := asString(it["arguments"]); !strings.Contains(args, "Beijing") {
		t.Errorf("arguments=%q", args)
	}
}

// idxOf SSE 帧里的 index 是 JSON 数字（float64），转成 int 供 map 键用。
func idxOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// 回归：tool_result.content 多 text 块必须有块边界，不能粘连成 "is42"。
// Anthropic 允许 content 是块数组，块边界就是语义边界；用空串拼接会让
// 上游模型把相邻块当成一个词。
func TestAnthropicToolResultMultiBlockSeparated(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = toolJSONBytes
	fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "qwen3.8-27b", "max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "user", "content": "weather?"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "call_a", "name": "get_weather",
					"input": map[string]any{"city": "Beijing"}}}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_a", "content": []any{
					map[string]any{"type": "text", "text": "The answer is"},
					map[string]any{"type": "text", "text": "42 degrees"},
				}}}},
		},
	}, nil)
	var got string
	for _, mi := range asList(fx.fake.last(t).payload["messages"]) {
		if m := asMap(mi); m["role"] == "tool" {
			got = asString(m["content"])
		}
	}
	if strings.Contains(got, "is42") {
		t.Fatalf("多块 tool_result 粘连成 %q（块边界丢失）", got)
	}
	if !strings.Contains(got, "The answer is") || !strings.Contains(got, "42 degrees") {
		t.Fatalf("多块 tool_result 内容缺失: %q", got)
	}
}

// 回归：tool_result.is_error 必须让模型看到失败信号。
// OpenAI 的 role=tool 消息没有 is_error 字段，只能编码进 content；
// 直接丢弃会让模型把失败结果当正常数据继续推理。
func TestAnthropicToolResultIsError(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = toolJSONBytes
	fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "qwen3.8-27b", "max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "user", "content": "weather in Atlantis?"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "call_a", "name": "get_weather",
					"input": map[string]any{"city": "Atlantis"}}}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_a",
					"is_error": true, "content": `{"code": 404}`}}},
		},
	}, nil)
	var got string
	for _, mi := range asList(fx.fake.last(t).payload["messages"]) {
		if m := asMap(mi); m["role"] == "tool" {
			got = asString(m["content"])
		}
	}
	if !strings.Contains(strings.ToLower(got), "error") {
		t.Fatalf("is_error=true 的 tool_result 丢了失败信号: %q", got)
	}
	if !strings.Contains(got, "404") {
		t.Fatalf("原始内容被吞: %q", got)
	}
}
