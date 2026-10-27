// Package adapters 对应 Python 版 adapters/：协议桥（Anthropic Messages /
// OpenAI Responses ⇄ Chat Completions）。本文件是 Anthropic 侧。
//
// Claude Code / Kiro 这类客户端原生只说 Anthropic Messages 协议（POST
// /v1/messages，SSE 每帧 `event: <type>\ndata: <json>\n\n`）。上游免费层只出
// Chat Completions，本地把协议翻掉，上游照旧走 chat 线。
package adapters

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"time"
)

// ── 公共辅助 ─────────────────────────────────────────────────────────────────

// hexID 生成 n 个 hex 字符的随机 id（对应 Python uuid4().hex[:n]）。
func hexID(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(b)[:n]
}

// marshalCompact JSON 紧凑序列化，不转义 HTML 字符（对齐 Python
// json.dumps(..., ensure_ascii=False, separators=(",",":"))）。
func marshalCompact(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// SSELines 行流 + 读错误访问器：流消费完后调用 Err()，返回底层读错误
// （nil = 正常 EOF），adapter 据此区分上游断流（Responses 终局帧选 incomplete/failed）。
type SSELines struct {
	Seq iter.Seq[string]
	Err func() error
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

// ════════════════════════════════════════════════════════════════════════════
// 一、请求侧：Anthropic Messages -> Chat Completions
// ════════════════════════════════════════════════════════════════════════════

// imageBlockToChat Anthropic image block -> Chat image_url block；转不了返回 nil。
func imageBlockToChat(b map[string]any) map[string]any {
	src := asMap(b["source"])
	if src == nil {
		return nil
	}
	if asString(src["type"]) == "base64" {
		mt := asString(src["media_type"])
		if mt == "" {
			mt = "image/png"
		}
		if data := asString(src["data"]); data != "" {
			return map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:" + mt + ";base64," + data},
			}
		}
	}
	if asString(src["type"]) == "url" {
		if url := asString(src["url"]); url != "" {
			return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
		}
	}
	return nil
}

// plainContentToChat Anthropic content（纯文本/图片块，无 tool_use/tool_result）
// -> Chat content（string 或块数组，单文本块折叠成字符串）。
func plainContentToChat(content any) any {
	if s, ok := content.(string); ok {
		return s
	}
	blocks, ok := content.([]any)
	if !ok {
		if content == nil {
			return ""
		}
		return fmt.Sprint(content)
	}
	parts := []any{}
	for _, b := range blocks {
		if s, ok := b.(string); ok {
			parts = append(parts, map[string]any{"type": "text", "text": s})
			continue
		}
		m := asMap(b)
		if m == nil {
			continue
		}
		switch asString(m["type"]) {
		case "text":
			text := asString(m["text"])
			parts = append(parts, map[string]any{"type": "text", "text": text})
		case "image":
			if img := imageBlockToChat(m); img != nil {
				parts = append(parts, img)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		if t, ok := parts[0].(map[string]any); ok && asString(t["type"]) == "text" {
			return asString(t["text"])
		}
	}
	return parts
}

// prependThinking 把 thinking 文本并入 Chat content（string 或块数组）。
//
// 上游不读 reasoning_content，thinking 只能走正文才不丢。thinking 在生成序
// 上早于正文，所以前置而不是后置。content 是块数组时插一个 text 块，保住
// 图片等非文本块的位置。
func prependThinking(content any, thinkTxt string) any {
	if thinkTxt == "" {
		return content
	}
	if s, ok := content.(string); ok {
		if s == "" {
			return thinkTxt
		}
		return thinkTxt + "\n\n" + s
	}
	parts, ok := content.([]any)
	if !ok {
		return thinkTxt
	}
	out := make([]any, 0, len(parts)+1)
	out = append(out, map[string]any{"type": "text", "text": thinkTxt})
	out = append(out, parts...)
	return out
}

// toolResultContentToStr tool_result.content（str 或块数组）-> 单个字符串。
//
// 块之间用换行连接而非空串：块边界就是语义边界，空串拼接会让
// [{text:"The answer is"},{text:"42"}] 变成 "The answer is42"，
// 上游把相邻块当成一个词。换行在纯文本里不会引入额外语义。
func toolResultContentToStr(out any) string {
	if s, ok := out.(string); ok {
		return s
	}
	if blocks, ok := out.([]any); ok {
		segs := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if s, ok := b.(string); ok {
				segs = append(segs, s)
			} else if m := asMap(b); m != nil && asString(m["type"]) == "text" {
				segs = append(segs, asString(m["text"]))
			}
		}
		return strings.Join(segs, "\n")
	}
	if out == nil {
		return ""
	}
	return string(marshalCompact(out))
}

// toolResultToChat user.tool_result 块 -> Chat role=tool 消息。
//
// is_error 在 Chat 协议里没有对应字段，但它是模型判断「工具失败」的
// 结构化信号，直接丢弃会让模型把失败结果当正常数据继续推理（错误
// 内容里没有 error 字样时尤其危险）。这里编码进 content 前缀。
func toolResultToChat(tr map[string]any) map[string]any {
	content := toolResultContentToStr(tr["content"])
	if b, ok := tr["is_error"].(bool); ok && b {
		content = "[tool error] " + content
	}
	return map[string]any{
		"role":         "tool",
		"tool_call_id": asString(tr["tool_use_id"]),
		"content":      content,
	}
}

// anthropicMessagesToChat Anthropic system + messages -> Chat messages。
//
// system（str 或文本块数组）-> 首条 system；assistant 的 tool_use 块 ->
// assistant.tool_calls（同条文本并入 content）；user 的 tool_result 块 ->
// role=tool（每块一条），剩余文本另起 user；纯文本/图片 -> 普通 user/assistant。
func anthropicMessagesToChat(body map[string]any) []any {
	messages := []any{}
	switch sysc := body["system"].(type) {
	case string:
		if sysc != "" {
			messages = append(messages, map[string]any{"role": "system", "content": sysc})
		}
	case []any:
		segs := []string{}
		for _, b := range sysc {
			if m := asMap(b); m != nil && asString(m["type"]) == "text" {
				segs = append(segs, asString(m["text"]))
			}
		}
		if txt := strings.Join(segs, " "); txt != "" {
			messages = append(messages, map[string]any{"role": "system", "content": txt})
		}
	}

	for _, mi := range asList(body["messages"]) {
		m := asMap(mi)
		if m == nil {
			continue
		}
		role := asString(m["role"])
		if role == "" {
			role = "user"
		}
		if role != "user" && role != "assistant" {
			role = "user"
		}
		content, isList := m["content"].([]any)
		if !isList {
			if s, ok := m["content"].(string); ok {
				messages = append(messages, map[string]any{"role": role, "content": s})
			} else if m["content"] == nil {
				messages = append(messages, map[string]any{"role": role, "content": ""})
			} else {
				messages = append(messages, map[string]any{"role": role, "content": fmt.Sprint(m["content"])})
			}
			continue
		}

		var toolUses, toolResults []map[string]any
		textSegs := []string{}
		thinkSegs := []string{}
		for _, bi := range content {
			if s, ok := bi.(string); ok {
				textSegs = append(textSegs, s)
				continue
			}
			b := asMap(bi)
			if b == nil {
				continue
			}
			switch asString(b["type"]) {
			case "text":
				textSegs = append(textSegs, asString(b["text"]))
			case "thinking":
				// thinking -> reasoning_content：Chat 侧语义对等的推理通道。
				// signature 不参与翻译——它是 Anthropic 的加密推理副本，
				// 上游只认文本，带上反而是无意义字段。
				if t := asString(b["thinking"]); t != "" {
					thinkSegs = append(thinkSegs, t)
				}
			case "tool_use":
				toolUses = append(toolUses, b)
			case "tool_result":
				toolResults = append(toolResults, b)
			}
		}
		textTxt := strings.Join(textSegs, " ")
		// thinking 在上游没有对等通道：实测 glm5.3-flash 与 qwen3.8-27b 都
		// 不读 assistant.reasoning_content（随机码回传 4 次全未命中，另试
		// reasoning / thinking / provider_specific_fields 三种字段名同样不认）。
		// 并入正文是唯一能保住推理连续性的做法——工具调用轮次丢了思考，
		// 模型会重复调用或接不上自己的推理。thinking 发生在输出之前，前置。
		if thinkTxt := strings.Join(thinkSegs, "\n\n"); thinkTxt != "" {
			if textTxt != "" {
				textTxt = thinkTxt + "\n\n" + textTxt
			} else {
				textTxt = thinkTxt
			}
		}

		if len(toolResults) > 0 {
			// user 回工具结果：每条 tool_result -> role=tool；余下文本另起 user
			for _, tr := range toolResults {
				messages = append(messages, toolResultToChat(tr))
			}
			if textTxt != "" {
				messages = append(messages, map[string]any{"role": "user", "content": textTxt})
			}
		} else if len(toolUses) > 0 {
			tcList := make([]any, 0, len(toolUses))
			for _, tu := range toolUses {
				id := asString(tu["id"])
				if id == "" {
					id = "call_" + hexID(24)
				}
				input := tu["input"]
				if input == nil {
					input = map[string]any{}
				}
				tcList = append(tcList, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      asString(tu["name"]),
						"arguments": string(marshalCompact(input)),
					},
				})
			}
			var contentVal any
			if textTxt != "" {
				contentVal = textTxt
			}
			messages = append(messages, map[string]any{
				"role":       "assistant",
				"content":    contentVal,
				"tool_calls": tcList,
			})
		} else {
			messages = append(messages, map[string]any{
				"role":    role,
				"content": prependThinking(plainContentToChat(content), strings.Join(thinkSegs, "\n\n")),
			})
		}
	}
	return messages
}

// anthropicToolsToChat Anthropic tools（input_schema）-> Chat tools（function.parameters）。
func anthropicToolsToChat(tools any) []any {
	list, ok := tools.([]any)
	if !ok {
		return nil
	}
	out := []any{}
	for _, ti := range list {
		t := asMap(ti)
		if t == nil {
			continue
		}
		params := t["input_schema"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        asString(t["name"]),
				"description": asString(t["description"]),
				"parameters":  params,
			},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// anthropicToolChoiceToChat Anthropic tool_choice -> Chat tool_choice。
func anthropicToolChoiceToChat(tc any) any {
	m := asMap(tc)
	if m == nil {
		return nil
	}
	switch asString(m["type"]) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": asString(m["name"])},
		}
	}
	return nil
}

// AnthropicToChat Anthropic Messages 请求体 -> Chat Completions 请求体。
// 只搬 Chat 能表达的字段；thinking / metadata / top_k 等无对应项丢弃（v1）。
func AnthropicToChat(body map[string]any) map[string]any {
	stream := false
	if b, ok := body["stream"].(bool); ok {
		stream = b
	}
	chat := map[string]any{
		"model":    asString(body["model"]),
		"messages": anthropicMessagesToChat(body),
		"stream":   stream,
	}
	if tools := anthropicToolsToChat(body["tools"]); tools != nil {
		chat["tools"] = tools
	}
	if tc := anthropicToolChoiceToChat(body["tool_choice"]); tc != nil {
		chat["tool_choice"] = tc
	}
	if v, ok := body["max_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	if v, ok := body["temperature"]; ok && v != nil {
		chat["temperature"] = v
	}
	if v, ok := body["top_p"]; ok && v != nil {
		chat["top_p"] = v
	}
	if ss, ok := body["stop_sequences"].([]any); ok && len(ss) > 0 {
		chat["stop"] = ss
	}
	return chat
}

// ════════════════════════════════════════════════════════════════════════════
// 二、响应侧公共
// ════════════════════════════════════════════════════════════════════════════

// thinkingSigMarker 网关自签的 thinking signature。
//
// Anthropic 协议要求 thinking 块带 signature 才能回传，而上游只给
// reasoning_content、不给签名。这里签发一个固定标记（base64，解码后是
// "atomcode-reasoning-v1"）：只有本网关会读它，回传时认出来就还原成
// reasoning_content，不需要通过任何签名校验。
//
// 不用空串：空 signature + 有文本的 thinking 块在不少客户端里会被当成
// 占位块丢弃（判据是「签名空且文本空才算占位」，有文本反而落进「签名
// 无效」分支），显式标记能保证往返稳定。
const thinkingSigMarker = "EYXRvbWNvZGUtcmVhc29uaW5nLXYx"

// reasoningThinkingBlock Chat message.reasoning_content -> Anthropic thinking 块。
// 无 reasoning 时返回 nil。
func reasoningThinkingBlock(msg map[string]any) map[string]any {
	txt := asString(msg["reasoning_content"])
	if txt == "" {
		return nil
	}
	return map[string]any{
		"type":      "thinking",
		"thinking":  txt,
		"signature": thinkingSigMarker,
	}
}

// StopReasonFor Chat finish_reason -> Anthropic stop_reason。
func StopReasonFor(finish string) string {
	switch finish {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	}
	return "end_turn"
}

// AnthropicUsage Chat usage -> Anthropic usage（四个字段齐全）。
func AnthropicUsage(u map[string]any) map[string]any {
	if u == nil {
		u = map[string]any{}
	}
	return map[string]any{
		"input_tokens":                int(asFloat(u["prompt_tokens"])),
		"output_tokens":               int(asFloat(u["completion_tokens"])),
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
	}
}

// AnthropicErrTypeForHTTP HTTP 状态码 -> Anthropic error type。
func AnthropicErrTypeForHTTP(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	return "api_error"
}

// AnthropicErrorBody Anthropic 错误响应体。
func AnthropicErrorBody(errType, message string) map[string]any {
	if message == "" {
		message = "error"
	}
	return map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	}
}

// ════════════════════════════════════════════════════════════════════════════
// 三、非流式：Chat JSON -> Anthropic Message
// ════════════════════════════════════════════════════════════════════════════

// ChatJSONToAnthropic 非流式 Chat Completions JSON -> Anthropic Message 对象。
func ChatJSONToAnthropic(chat map[string]any, model string) map[string]any {
	if model == "" {
		model = asString(chat["model"])
	}
	msg := map[string]any{}
	finish := "stop"
	if choices := asList(chat["choices"]); len(choices) > 0 {
		if c := asMap(choices[0]); c != nil {
			if m := asMap(c["message"]); m != nil {
				msg = m
			}
			if fr := asString(c["finish_reason"]); fr != "" {
				finish = fr
			}
		}
	}

	contentBlocks := []any{}
	// thinking 块必须排在 text 之前：Anthropic 的块序就是生成序，
	// 客户端（Claude Code 等）按位置渲染思考面板。
	if th := reasoningThinkingBlock(msg); th != nil {
		contentBlocks = append(contentBlocks, th)
	}
	if text := asString(msg["content"]); text != "" {
		contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
	}
	for _, tci := range asList(msg["tool_calls"]) {
		tc := asMap(tci)
		if tc == nil {
			continue
		}
		fn := asMap(tc["function"])
		if fn == nil {
			fn = map[string]any{}
		}
		inp := map[string]any{}
		if args := asString(fn["arguments"]); args != "" {
			var parsed map[string]any
			if json.Unmarshal([]byte(args), &parsed) == nil && parsed != nil {
				inp = parsed
			}
		}
		id := asString(tc["id"])
		if id == "" {
			id = "toolu_" + hexID(24)
		}
		contentBlocks = append(contentBlocks, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  asString(fn["name"]),
			"input": inp,
		})
	}

	return map[string]any{
		"id":            "msg_" + hexID(24),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       contentBlocks,
		"stop_reason":   StopReasonFor(finish),
		"stop_sequence": nil,
		"usage":         AnthropicUsage(asMap(chat["usage"])),
	}
}

// ════════════════════════════════════════════════════════════════════════════
// 四、流式：Chat SSE -> Anthropic SSE
// ════════════════════════════════════════════════════════════════════════════

// antFrame 组装一帧 `event: <type>\ndata: <json>\n\n`（type 同时写进 data）。
func antFrame(eventType string, data map[string]any) []byte {
	d := make(map[string]any, len(data)+1)
	for k, v := range data {
		d[k] = v
	}
	d["type"] = eventType
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(eventType)
	buf.WriteString("\ndata: ")
	buf.Write(marshalCompact(d))
	buf.WriteString("\n\n")
	return buf.Bytes()
}

// ChatSSEToAnthropic Chat SSE 行流 -> Anthropic SSE 帧流。
// 收尾: content_block_stop * N -> message_delta -> message_stop；
// 上游错误帧 -> error 事件后结束。
func ChatSSEToAnthropic(lines iter.Seq[string], model string) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		msgID := "msg_" + hexID(24)
		baseUsage := map[string]any{
			"input_tokens": 0, "output_tokens": 0,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		}
		blocks := 0                 // 已开出的 content block 数
		thinkIdx := -1              // 当前打开的 thinking 块下标（-1 = 没有打开的）
		textIdx := -1               // 文本块下标（-1 = 还没开）
		toolBlocks := map[int]int{} // chat tool_calls.index -> blocks 下标
		closed := map[int]bool{}    // 已发过 content_block_stop 的下标
		finish := ""
		var usage map[string]any

		// closeThinking 给当前 thinking 块发 signature_delta + content_block_stop。
		//
		// Anthropic 的块序要求签名落在块停止之前，且正文/工具块不能插在
		// 思考块中间——所以 thinking 必须在下一个块开出前封口。
		closeThinking := func() bool {
			if thinkIdx < 0 {
				return true
			}
			if !yield(antFrame("content_block_delta", map[string]any{
				"index": thinkIdx,
				"delta": map[string]any{"type": "signature_delta", "signature": thinkingSigMarker},
			})) {
				return false
			}
			if !yield(antFrame("content_block_stop", map[string]any{"index": thinkIdx})) {
				return false
			}
			closed[thinkIdx] = true
			thinkIdx = -1
			return true
		}

		yield(antFrame("message_start", map[string]any{"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "content": []any{},
			"model": model, "stop_reason": nil, "stop_sequence": nil,
			"usage": baseUsage,
		}}))

		for chunk := range chatChunks(lines) {
			if chunk.upstreamError != nil {
				yield(antFrame("error", map[string]any{
					"error": map[string]any{"type": "api_error", "message": chunk.upstreamError.msg},
				}))
				return
			}
			if chunk.usage != nil {
				usage = chunk.usage
			}
			finish = pickFinish(chunk, finish)

			// 0) reasoning delta -> thinking 块
			//
			// 上游通常先吐 reasoning 再吐正文，但没有协议保证。若正文已
			// 开块后又来 reasoning（少见），这里新开一个 thinking 块排在
			// 后面——Anthropic 的块序即生成序，靠后但不丢内容。
			if rc := chunk.reasoningContent; rc != "" {
				if thinkIdx < 0 {
					thinkIdx = blocks
					blocks++
					if !yield(antFrame("content_block_start", map[string]any{
						"index": thinkIdx,
						"content_block": map[string]any{
							"type": "thinking", "thinking": "", "signature": "",
						},
					})) {
						return
					}
				}
				if !yield(antFrame("content_block_delta", map[string]any{
					"index": thinkIdx,
					"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
				})) {
					return
				}
			}

			// 1) 文本 delta
			if ct := chunk.content; ct != "" {
				if textIdx < 0 {
					if !closeThinking() {
						return
					}
					textIdx = blocks
					blocks++
					if !yield(antFrame("content_block_start", map[string]any{
						"index":         textIdx,
						"content_block": map[string]any{"type": "text", "text": ""},
					})) {
						return
					}
				}
				if !yield(antFrame("content_block_delta", map[string]any{
					"index": textIdx,
					"delta": map[string]any{"type": "text_delta", "text": ct},
				})) {
					return
				}
			}

			// 2) tool_calls delta
			for _, tc := range chunk.toolCalls {
				bidx, ok := toolBlocks[tc.index]
				if !ok {
					if !closeThinking() {
						return
					}
					bidx = blocks
					blocks++
					callID := tc.id
					if callID == "" {
						callID = "toolu_" + hexID(24)
					}
					toolBlocks[tc.index] = bidx
					if !yield(antFrame("content_block_start", map[string]any{
						"index": bidx,
						"content_block": map[string]any{
							"type": "tool_use", "id": callID, "name": tc.name, "input": map[string]any{},
						},
					})) {
						return
					}
				}
				if tc.arguments != "" {
					if !yield(antFrame("content_block_delta", map[string]any{
						"index": bidx,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.arguments},
					})) {
						return
					}
				}
			}
		}

		// 收尾：还没封口的 thinking（整条回复只有思考）在这里封。
		if !closeThinking() {
			return
		}
		for idx := 0; idx < blocks; idx++ {
			if closed[idx] {
				continue
			}
			if !yield(antFrame("content_block_stop", map[string]any{"index": idx})) {
				return
			}
		}
		outTokens := 0
		if usage != nil {
			outTokens = int(asFloat(usage["completion_tokens"]))
		}
		yield(antFrame("message_delta", map[string]any{
			"delta": map[string]any{"stop_reason": StopReasonFor(finish), "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": outTokens},
		}))
		yield(antFrame("message_stop", map[string]any{}))
	}
}

// ════════════════════════════════════════════════════════════════════════════
// 五、非流式聚合：Chat SSE -> 单个 Chat JSON
// ════════════════════════════════════════════════════════════════════════════

// AggregateChatSSE Chat SSE 行流 -> 单个 chat.completion JSON。
// 供非流式 Anthropic 路径用：上游恒流式，先聚合成 Chat JSON，再交给
// ChatJSONToAnthropic 翻成 Anthropic Message。
func AggregateChatSSE(lines iter.Seq[string], model string) map[string]any {
	content := []string{}
	reasoning := []string{}
	type callState struct {
		id, name, arguments string
	}
	toolCalls := map[int]*callState{}
	callOrder := []int{}
	finish := ""
	var usage map[string]any

	for chunk := range chatChunks(lines) {
		if chunk.upstreamError != nil {
			// 非流式聚合遇到上游错误：记录到 _upstream_error，交给路由层定夺
			msg := map[string]any{
				"id": "chatcmpl-" + hexID(24), "object": "chat.completion",
				"created": time.Now().Unix(), "model": model,
				"choices": []any{map[string]any{
					"index": 0,
					"message": map[string]any{
						"role": "assistant", "content": strings.Join(content, ""),
					},
					"finish_reason": orDefault(finish, "stop"),
				}},
				"usage": usageOrZero(usage),
			}
			msg["_upstream_error"] = []string{chunk.upstreamError.code, chunk.upstreamError.msg}
			return msg
		}
		if chunk.usage != nil {
			usage = chunk.usage
		}
		finish = pickFinish(chunk, finish)
		if chunk.content != "" {
			content = append(content, chunk.content)
		}
		if chunk.reasoningContent != "" {
			reasoning = append(reasoning, chunk.reasoningContent)
		}
		for _, tc := range chunk.toolCalls {
			st, ok := toolCalls[tc.index]
			if !ok {
				id := tc.id
				if id == "" {
					id = "call_" + hexID(24)
				}
				st = &callState{id: id, name: tc.name}
				toolCalls[tc.index] = st
				callOrder = append(callOrder, tc.index)
			}
			if tc.name != "" {
				st.name = tc.name
			}
			st.arguments += tc.arguments
		}
	}

	var contentVal any
	if joined := strings.Join(content, ""); joined != "" {
		contentVal = joined
	}
	msg := map[string]any{"role": "assistant", "content": contentVal}
	if r := strings.Join(reasoning, ""); r != "" {
		msg["reasoning_content"] = r
	}
	if len(callOrder) > 0 {
		tcs := make([]any, 0, len(callOrder))
		for _, i := range callOrder {
			st := toolCalls[i]
			tcs = append(tcs, map[string]any{
				"id": st.id, "type": "function",
				"function": map[string]any{"name": st.name, "arguments": st.arguments},
			})
		}
		msg["tool_calls"] = tcs
	}
	return map[string]any{
		"id": "chatcmpl-" + hexID(24), "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": orDefault(finish, "stop"),
		}},
		"usage": usageOrZero(usage),
	}
}

func usageOrZero(u map[string]any) map[string]any {
	if u != nil {
		return u
	}
	return map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
}

func orDefault(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

// ════════════════════════════════════════════════════════════════════════════
// 六、辅助：完整 Chat JSON -> Anthropic SSE 序列；单帧错误事件
// ════════════════════════════════════════════════════════════════════════════

// ChatJSONToAnthropicSSE 完整 Chat JSON -> Anthropic SSE 帧序列。
// 用于客户端要流式、但上游只给了非流式 JSON（或错误）的场景。
func ChatJSONToAnthropicSSE(chatJSON map[string]any, model string) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		obj := ChatJSONToAnthropic(chatJSON, model)
		usage := asMap(obj["usage"])
		yield(antFrame("message_start", map[string]any{"message": map[string]any{
			"id": obj["id"], "type": "message", "role": "assistant", "content": []any{},
			"model": obj["model"], "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens": usage["input_tokens"], "output_tokens": 0,
				"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
			},
		}}))
		for idx, bi := range asList(obj["content"]) {
			block := asMap(bi)
			if block == nil {
				continue
			}
			switch asString(block["type"]) {
			case "thinking":
				yield(antFrame("content_block_start", map[string]any{
					"index": idx,
					"content_block": map[string]any{
						"type": "thinking", "thinking": "", "signature": "",
					},
				}))
				yield(antFrame("content_block_delta", map[string]any{
					"index": idx,
					"delta": map[string]any{"type": "thinking_delta", "thinking": block["thinking"]},
				}))
				yield(antFrame("content_block_delta", map[string]any{
					"index": idx,
					"delta": map[string]any{"type": "signature_delta", "signature": thinkingSigMarker},
				}))
				yield(antFrame("content_block_stop", map[string]any{"index": idx}))
			case "text":
				yield(antFrame("content_block_start", map[string]any{
					"index": idx, "content_block": map[string]any{"type": "text", "text": ""},
				}))
				yield(antFrame("content_block_delta", map[string]any{
					"index": idx,
					"delta": map[string]any{"type": "text_delta", "text": block["text"]},
				}))
				yield(antFrame("content_block_stop", map[string]any{"index": idx}))
			case "tool_use":
				yield(antFrame("content_block_start", map[string]any{
					"index": idx,
					"content_block": map[string]any{
						"type": "tool_use", "id": block["id"], "name": block["name"],
						"input": map[string]any{},
					},
				}))
				yield(antFrame("content_block_delta", map[string]any{
					"index": idx,
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": string(marshalCompact(block["input"])),
					},
				}))
				yield(antFrame("content_block_stop", map[string]any{"index": idx}))
			}
		}
		yield(antFrame("message_delta", map[string]any{
			"delta": map[string]any{"stop_reason": obj["stop_reason"], "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": usage["output_tokens"]},
		}))
		yield(antFrame("message_stop", map[string]any{}))
	}
}

// AnthropicErrorSSE 单个 Anthropic error 事件帧。
func AnthropicErrorSSE(errType, message string) []byte {
	return antFrame("error", map[string]any{
		"error": map[string]any{"type": errType, "message": message},
	})
}
