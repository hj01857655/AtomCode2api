// thinking 块双向映射：Anthropic thinking ↔ Chat reasoning_content。
//
// 上游（glm5.3-flash / qwen3.8-27b）产出真实 reasoning_content 但不给
// signature。Anthropic 协议里 thinking 块必须带 signature 才能回传，
// 所以网关自己签发一个固定标记充当 signature —— 只有本网关会读它，
// 不需要通过 Anthropic 的签名校验。
package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 上游：只给 reasoning_content，正文为空。
// 真实形态——模型想完就停，或者思考占满了 max_tokens。
var reasoningOnlyStream = concat(
	chunk(map[string]any{"role": "assistant", "reasoning_content": "Let me "}, nil, nil, "t1"),
	chunk(map[string]any{"reasoning_content": "think."}, nil, nil, "t1"),
	chunk(map[string]any{}, "stop", nil, "t1"),
	chunk(map[string]any{}, nil, sseUsage, "t1"),
	[]byte("data: [DONE]\n\n"),
)

// 上游：thinking 之后有正文。
var reasoningThenTextStream = concat(
	chunk(map[string]any{"role": "assistant", "reasoning_content": "hmm"}, nil, nil, "t2"),
	chunk(map[string]any{"content": "Hi!"}, nil, nil, "t2"),
	chunk(map[string]any{}, "stop", nil, "t2"),
	chunk(map[string]any{}, nil, sseUsage, "t2"),
	[]byte("data: [DONE]\n\n"),
)

var reasoningOnlyJSON = marshalCompactJSON(map[string]any{
	"id": "t3", "object": "chat.completion", "created": 1, "model": "m",
	"choices": []any{map[string]any{
		"index": 0, "finish_reason": "stop",
		"message": map[string]any{"role": "assistant", "content": "",
			"reasoning_content": "deep thought"},
	}},
	"usage": sseUsage,
})

var reasoningThenTextJSON = marshalCompactJSON(map[string]any{
	"id": "t4", "object": "chat.completion", "created": 1, "model": "m",
	"choices": []any{map[string]any{
		"index": 0, "finish_reason": "stop",
		"message": map[string]any{"role": "assistant", "content": "Hi!",
			"reasoning_content": "deep thought"},
	}},
	"usage": sseUsage,
})

// 请求侧：assistant 的 thinking 块并入正文（上游不读 reasoning_content）。
func TestAnthropicThinkingFoldedIntoContent(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = reasoningThenTextJSON
	r := fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "glm5.3-flash", "max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "I should greet back",
					"signature": "EYXRvbWNvZGUtcmVhc29uaW5nLXYx"},
				map[string]any{"type": "text", "text": "Hello!"},
			}},
			map[string]any{"role": "user", "content": "again"},
		},
	}, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	msgs := asList(fx.fake.last(t).payload["messages"])
	asst := asMap(msgs[1])
	got := asString(asst["content"])
	if !strings.Contains(got, "I should greet back") {
		t.Fatalf("thinking 内容丢了: %q", got)
	}
	if !strings.Contains(got, "Hello!") {
		t.Fatalf("正文丢了: %q", got)
	}
	if strings.Index(got, "I should greet back") > strings.Index(got, "Hello!") {
		t.Errorf("thinking 应排在正文之前: %q", got)
	}
}

// 纯 thinking 的 assistant 消息：content 必须存在（可为空串），
// 且不能因为缺正文就被整条丢掉——assistant 轮次消失会破坏对话结构。
func TestAnthropicPureThinkingMessage(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = reasoningThenTextJSON
	fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "glm5.3-flash", "max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "just thinking",
					"signature": "EYXRvbWNvZGUtcmVhc29uaW5nLXYx"},
			}},
		},
	}, nil)
	msgs := asList(fx.fake.last(t).payload["messages"])
	if len(msgs) != 2 {
		t.Fatalf("assistant 轮次被丢了: %v", msgs)
	}
	asst := asMap(msgs[1])
	if got := asString(asst["content"]); got != "just thinking" {
		t.Fatalf("content=%q，纯 thinking 消息应只剩思考文本", got)
	}
}

// 响应侧非流式：reasoning_content -> thinking 块，且带可回传的 signature。
func TestAnthropicResponseThinkingBlock(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = reasoningThenTextJSON
	r := fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "glm5.3-flash", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应非 JSON: %v\n%s", err, r.Body.String())
	}
	blocks := asList(out["content"])
	if len(blocks) != 2 {
		t.Fatalf("content 块数=%d，期望 thinking+text 两块: %v", len(blocks), out["content"])
	}
	th := asMap(blocks[0])
	if th["type"] != "thinking" {
		t.Fatalf("首块不是 thinking: %v", th)
	}
	if got := asString(th["thinking"]); got != "deep thought" {
		t.Errorf("thinking 文本=%q", got)
	}
	if sig := asString(th["signature"]); sig == "" {
		t.Errorf("signature 为空，回传时会被当成占位块丢弃")
	}
	if asMap(blocks[1])["type"] != "text" {
		t.Errorf("次块不是 text: %v", blocks[1])
	}
}

// 响应侧非流式：只有 reasoning、没有正文时，content 不能是空数组。
// Anthropic 客户端拿到 content:[] 会认为模型没输出任何东西。
func TestAnthropicResponseThinkingOnly(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = reasoningOnlyJSON
	r := fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "glm5.3-flash", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	var out map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应非 JSON: %v\n%s", err, r.Body.String())
	}
	blocks := asList(out["content"])
	if len(blocks) == 0 {
		t.Fatalf("content 是空数组，思考内容被完全吞掉: %s", r.Body.String())
	}
	th := asMap(blocks[0])
	if th["type"] != "thinking" || asString(th["thinking"]) != "deep thought" {
		t.Fatalf("thinking 块不对: %v", th)
	}
}

// 响应侧流式：reasoning_content delta -> thinking_delta + signature_delta，
// 且 thinking 块必须排在 text 块之前（Anthropic 要求块序与生成序一致）。
func TestAnthropicStreamThinkingDeltas(t *testing.T) {
	fx := newFixture(t, "stream")
	fx.fake.streamBody = reasoningThenTextStream
	r := fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "glm5.3-flash", "max_tokens": 64, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	frames := parseAntFrames(t, r.Body.Bytes())

	var thinkIdx, textIdx = -1, -1
	var thinkingText, sig string
	for _, f := range frames {
		switch f.event {
		case "content_block_start":
			cb := asMap(f.data["content_block"])
			switch cb["type"] {
			case "thinking":
				thinkIdx = idxOf(f.data["index"])
			case "text":
				textIdx = idxOf(f.data["index"])
			}
		case "content_block_delta":
			d := asMap(f.data["delta"])
			switch d["type"] {
			case "thinking_delta":
				thinkingText += asString(d["thinking"])
			case "signature_delta":
				sig = asString(d["signature"])
			}
		}
	}
	if thinkIdx < 0 {
		t.Fatalf("没有开 thinking 块:\n%s", r.Body.String())
	}
	if textIdx >= 0 && thinkIdx > textIdx {
		t.Errorf("thinking 块(index=%d) 排在 text(index=%d) 之后", thinkIdx, textIdx)
	}
	if thinkingText != "hmm" {
		t.Errorf("thinking_delta 拼出 %q，期望 \"hmm\"", thinkingText)
	}
	if sig == "" {
		t.Errorf("没有 signature_delta，客户端攒不齐 thinking 块")
	}
}

// 流式且只有 reasoning：仍要有 thinking 块，不能让客户端收到空消息。
func TestAnthropicStreamThinkingOnly(t *testing.T) {
	fx := newFixture(t, "stream")
	fx.fake.streamBody = reasoningOnlyStream
	r := fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "glm5.3-flash", "max_tokens": 64, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	frames := parseAntFrames(t, r.Body.Bytes())
	var started int
	var thinkingText string
	for _, f := range frames {
		if f.event == "content_block_start" {
			started++
		}
		if f.event == "content_block_delta" {
			if d := asMap(f.data["delta"]); d["type"] == "thinking_delta" {
				thinkingText += asString(d["thinking"])
			}
		}
	}
	if started == 0 {
		t.Fatalf("一个 content block 都没开:\n%s", r.Body.String())
	}
	if thinkingText != "Let me think." {
		t.Errorf("thinking 文本=%q", thinkingText)
	}
}

// 往返：网关签发的 thinking 块原样回传，必须还原成 reasoning_content。
func TestAnthropicThinkingRoundTrip(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = reasoningThenTextJSON

	// 第一轮：拿网关签发的 thinking 块
	r1 := fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "glm5.3-flash", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	var first map[string]any
	if err := json.Unmarshal(r1.Body.Bytes(), &first); err != nil {
		t.Fatalf("第一轮响应非 JSON: %v", err)
	}
	asstBlocks := asList(first["content"])

	// 第二轮：把 assistant 内容原样放回去
	fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "glm5.3-flash", "max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": asstBlocks},
			map[string]any{"role": "user", "content": "again"},
		},
	}, nil)

	asst := asMap(asList(fx.fake.last(t).payload["messages"])[1])
	got := asString(asst["content"])
	if !strings.Contains(got, "deep thought") {
		t.Fatalf("回传的 thinking 块丢了: %q", got)
	}
	if !strings.Contains(got, "Hi!") {
		t.Errorf("正文丢了: %q", got)
	}
}

// 无 signature 的 thinking 块（第三方客户端手搓的）也要认。
func TestAnthropicThinkingWithoutSignature(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = reasoningThenTextJSON
	fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "glm5.3-flash", "max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "no sig here"},
			}},
		},
	}, nil)
	asst := asMap(asList(fx.fake.last(t).payload["messages"])[1])
	if got := asString(asst["content"]); got != "no sig here" {
		t.Fatalf("无 signature 的 thinking 块被丢弃: %q", got)
	}
}

// redacted_thinking 块没有可读文本，只能整块丢弃——不能变成空正文。
func TestAnthropicRedactedThinking(t *testing.T) {
	fx := newFixture(t, "json")
	fx.fake.jsonBody = reasoningThenTextJSON
	fx.do(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "glm5.3-flash", "max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "redacted_thinking", "data": "opaque"},
				map[string]any{"type": "text", "text": "visible"},
			}},
		},
	}, nil)
	asst := asMap(asList(fx.fake.last(t).payload["messages"])[1])
	if got := asString(asst["content"]); got != "visible" {
		t.Errorf("正文=%q", got)
	}
	if rc, ok := asst["reasoning_content"]; ok {
		t.Errorf("上游不读 reasoning_content，不该发这个字段: %q", rc)
	}
}
