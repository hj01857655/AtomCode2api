// 三协议路由，对应 Python 版 routes/chat.py、routes/responses.py、routes/anthropic.py。
//
// 共同约定（与 Python 一致）：
//   - 逐端点显式过 _guard / _cred_guard
//   - 上游两条通道按模型前缀分流（openrouter/* → OpenRouter，其余 → CodingPlan）
//   - _send_* 失败时把 (jsonify, status) 原样回给客户端
//   - 流式必带 stream_options.include_usage（客户端已带则尊重客户端）
//   - 上游把额度耗尽/过载伪装成 HTTP 200 的正常回复文本，由 DetectPseudoReply
//     识别：非流式转 429 + Retry-After，流式在首个内容段替换为 SSE error 帧
//
// 已知缺陷修复（不照抄 Python）：非流式上游响应间歇性多包一层 {"data":{...}}，
// Python 版 chat 路由原样透传、responses/messages 路由直接解析失败；Go 版在
// 所有 CodingPlan 非流式路径上先 UnwrapDataLayer 再处理。
package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"atomcode2api/internal/adapters"
	"atomcode2api/internal/config"
	"atomcode2api/internal/upstream"
)

// ── /v1/chat/completions ─────────────────────────────────────────────────────

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || !s.credGuard(w) {
		return
	}
	body, bodyLen, ok := readJSONBody(r)
	if !ok || len(asList(body["messages"])) == 0 {
		writeJSON(w, http.StatusBadRequest, openAIErrorBody(
			"invalid request: 'messages' is required", "invalid_request_error"))
		return
	}

	modelID := asString(body["model"])
	rid := s.nextRID()
	tIn := time.Now()
	s.logIn(rid, r, body, bodyLen)
	wantStream := truthy(body["stream"])

	// ── OpenRouter 分支（免费模型，不吃 CodingPlan 额度）──
	if config.IsOpenRouterModel(modelID) {
		if s.OrKey == "" {
			writeJSON(w, http.StatusUnauthorized, openAIErrorBody(
				"OpenRouter API key 未配置（在 ~/.atomcode/config.toml 或 OPENROUTER_API_KEY 环境变量）",
				"auth_error"))
			return
		}
		orBody := mergeBody(body, map[string]any{"model": config.OpenRouterUpstreamModel(modelID)})
		resp, serr := s.Up.SendOpenRouter(r.Context(), marshalCompact(orBody), rid)
		if serr != nil {
			writeJSON(w, serr.Status, serr.Body)
			return
		}
		defer resp.Body.Close()
		t0 := time.Now()
		if !wantStream {
			content, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			s.doneLog(rid, "openrouter", len(content), t0)
			writeRaw(w, http.StatusOK, "application/json; charset=utf-8", content)
			return
		}
		// OpenRouter 分支：纯透传，不做伪回复拦截（Python 同）
		s.passthroughStream(w, r, rid, tIn, t0, resp.Body, "openrouter", nil)
		return
	}

	// ── CodingPlan 分支 ──
	// 对齐官方 CLI（openai_compat.rs:1335）：必带 include_usage，上游才会在流末尾
	// 发 usage 块——这是额度观察与记账的依据。客户端已带则尊重客户端。
	if wantStream && body["stream_options"] == nil {
		body = mergeBody(body, map[string]any{"stream_options": map[string]any{"include_usage": true}})
	}
	resp, serr := s.Up.SendCodingPlan(r.Context(), s, marshalCompact(body), rid, modelID)
	if serr != nil {
		writeJSON(w, serr.Status, serr.Body)
		return
	}
	defer resp.Body.Close()
	t0 := time.Now()

	if !wantStream {
		content, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		content = upstream.UnwrapDataLayer(content)
		_, txt := decodeChatJSON(content)
		if p := DetectPseudoReply(txt); p != nil {
			s.logQuota(rid, p, modelID, "200 伪回复转 429")
			s.pseudoOpenAIHTTP(w, p, txt)
			return
		}
		s.doneLog(rid, "chat", len(content), t0)
		writeRaw(w, http.StatusOK, "application/json; charset=utf-8", content)
		return
	}

	// SSE 透传（上游已是 OpenAI SSE 格式，含 reasoning_content 字段）。
	// 流内首段 content 命中伪回复 → 替换为 error 帧 + [DONE]；
	// 否则全部原样透传，流末再做一次后置检测（已发内容无法撤回，只记日志）。
	tail := &pseudoTail{}
	s.passthroughStream(w, r, rid, tIn, t0, resp.Body, "chat", tail)
	// 流末后置伪回复检测
	if p := DetectPseudoReply(tail.text()); p != nil {
		s.Log.Warn("done:", slog.String("rid", rid),
			slog.String("line", fmt.Sprintf("quota(%s) reset=%s —— 上游 200 伪回复（已透传）",
				p.Kind, orQ(p.Reset))), slog.String("model", modelID))
	}
}

// pseudoTail 累积透传流的正文（上限 4KB）与 usage，供流末后置检测与 tokens 汇报。
type pseudoTail struct {
	parts   []string
	size    int
	usage   map[string]any
	usageOK bool
}

func (t *pseudoTail) add(contents []string) {
	if t.size < 4096 {
		t.parts = append(t.parts, contents...)
		for _, c := range contents {
			t.size += len(c)
		}
	}
}

func (t *pseudoTail) text() string { return joinAll(t.parts) }

// noteLine 从原始 SSE 行记录 usage（流末 tokens= 汇报用）。
func (t *pseudoTail) noteLine(line string) {
	if t.usageOK {
		return
	}
	if u := usageFromSSELine(line); u != nil {
		t.usage = u
		t.usageOK = true
	}
}

// passthroughStream 裸字节转发上游流（chat 路由专用，保留上游帧格式不动）。
//
// tail 非 nil（CodingPlan 分支）时同时按行切出内容做伪回复拦截：首个内容段命中
// 就吐 error + [DONE] 并停止转发；流末 done 行带 ttft 与 tokens。
// tail 为 nil（OpenRouter 分支）时只转发与计测。
//
// 对应 routes/chat.py 的 generate() / or_generate()。
func (s *Server) passthroughStream(
	w http.ResponseWriter, r *http.Request,
	rid string, tIn, t0 time.Time,
	upstreamBody io.Reader, kind string, tail *pseudoTail,
) {
	sseHeaders(w)
	flusher, _ := w.(http.Flusher)

	total := 0
	var ttft time.Duration
	ttftOK := false
	intercept := tail != nil // 上游伪回复必以首个 content delta 整段出现，只拦那一段
	tap := &lineTap{}

	buf := make([]byte, 4096) // Python iter_content(chunk_size=4096)
	for {
		if err := r.Context().Err(); err != nil {
			s.logDisconnect(rid, kind, total, t0)
			return
		}
		n, rerr := upstreamBody.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if !ttftOK {
				ttft = time.Since(tIn)
				ttftOK = true
			}
			total += n

			var contents []string
			if intercept || tail != nil {
				for _, line := range tap.feed(chunk) {
					if c := FirstContentLine(line); c != "" {
						contents = append(contents, c)
					}
					if tail != nil {
						tail.noteLine(line)
					}
				}
			}
			if len(contents) > 0 {
				if intercept {
					joined := joinAll(contents)
					if p := DetectPseudoReply(joined); p != nil {
						s.logQuota(rid, p, "", "流内伪回复替换为 SSE error")
						// Python: f"data: {json.dumps(err, ensure_ascii=False)}\n\n"（默认分隔符，带空格）
						frame := marshalSpaced(map[string]any{"error": ssePseudoErrFields(p, joined)})
						_, _ = w.Write(append(append([]byte("data: "), frame...), '\n', '\n'))
						_, _ = w.Write([]byte("data: [DONE]\n\n"))
						if flusher != nil {
							flusher.Flush()
						}
						return
					}
					intercept = false
				}
				if tail != nil {
					tail.add(contents)
				}
			}

			if _, werr := w.Write(chunk); werr != nil {
				s.logDisconnect(rid, kind, total, t0)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			s.Log.Error("流中断", slog.String("rid", rid), slog.String("err", rerr.Error()),
				slog.String("line", fmt.Sprintf("%s %dB %.1fs", kind, total, time.Since(t0).Seconds())))
			return
		}
	}

	info := fmt.Sprintf("%dB %.1fs ttft=%s", total, time.Since(t0).Seconds(), fmtTTFT(ttft, ttftOK))
	if tail != nil && tail.usageOK {
		info += fmt.Sprintf(" tokens=%s/%s", pyVal(tail.usage["prompt_tokens"]), pyVal(tail.usage["completion_tokens"]))
	}
	if kind == "chat" {
		// Python CodingPlan 分支 done 行不带协议标签（"done: %dB %.1fs ttft=..."）
		s.Log.Info("done:", slog.String("rid", rid), slog.String("line", info))
		return
	}
	s.Log.Info("done:", slog.String("rid", rid), slog.String("line", kind+" "+info))
}

func (s *Server) logDisconnect(rid, kind string, total int, t0 time.Time) {
	s.Log.Warn("client 断开", slog.String("rid", rid),
		slog.String("line", fmt.Sprintf("%s %dB %.1fs", kind, total, time.Since(t0).Seconds())))
}

// ── /v1/responses ────────────────────────────────────────────────────────────

func (s *Server) responsesAPI(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || !s.credGuard(w) {
		return
	}
	body, _, ok := readJSONBody(r)
	if !ok || len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, openAIErrorBody(
			"invalid request: JSON object body required", "invalid_request_error"))
		return
	}
	chatBody := adapters.ResponsesToChat(body)
	if len(asList(chatBody["messages"])) == 0 {
		writeJSON(w, http.StatusBadRequest, openAIErrorBody(
			"invalid request: 'input' is required", "invalid_request_error"))
		return
	}

	modelID := asString(body["model"])
	rid := s.nextRID()
	wantStream := truthy(chatBody["stream"])
	// 与 chat 同款：流式必带 include_usage，上游才会在流末发 usage 块
	if wantStream && chatBody["stream_options"] == nil {
		chatBody = mergeBody(chatBody, map[string]any{"stream_options": map[string]any{"include_usage": true}})
	}

	// OpenRouter 分支：未配 key 时 Python 版回 OpenAI 形状 401
	if denied, msg := s.guardOpenRouter(modelID); denied {
		writeJSON(w, http.StatusUnauthorized, openAIErrorBody(msg, "auth_error"))
		return
	}

	resp, serr := s.dispatch(r, chatBody, rid, modelID)
	if serr != nil {
		writeJSON(w, serr.Status, serr.Body)
		return
	}
	defer resp.Body.Close()
	t0 := time.Now()

	if wantStream {
		s.streamAdapter(w, r, rid, t0, resp.Body, "responses", modelID, func(lines adapters.SSELines) iter_seq_bytes {
			return adapters.ChatSSEToResponses(lines, modelID)
		})
		return
	}

	content, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	content = upstream.UnwrapDataLayer(content)
	chatJSON, txt := decodeChatJSON(content)
	if p := DetectPseudoReply(txt); p != nil {
		s.logQuota(rid, p, modelID, "200 伪回复转 429")
		s.pseudoOpenAIHTTP(w, p, txt)
		return
	}
	if chatJSON == nil {
		writeJSON(w, http.StatusBadGateway, openAIErrorBody(
			"upstream returned non-JSON body", "upstream_error"))
		return
	}
	out := marshalCompact(adapters.ChatJSONToResponses(chatJSON, modelID))
	s.doneLog(rid, "responses", len(out), t0)
	writeRaw(w, http.StatusOK, "application/json; charset=utf-8", out)
}

// ── /v1/messages（Anthropic）─────────────────────────────────────────────────

func (s *Server) messagesAPI(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) || !s.credGuard(w) {
		return
	}
	body, _, ok := readJSONBody(r)
	if !ok || len(asList(body["messages"])) == 0 {
		writeJSON(w, http.StatusBadRequest, adapters.AnthropicErrorBody(
			"invalid_request_error", "invalid request: 'messages' is required"))
		return
	}
	chatBody := adapters.AnthropicToChat(body)
	if len(asList(chatBody["messages"])) == 0 {
		writeJSON(w, http.StatusBadRequest, adapters.AnthropicErrorBody(
			"invalid_request_error", "invalid request: 'messages' is required"))
		return
	}

	modelID := asString(body["model"])
	rid := s.nextRID()
	wantStream := truthy(chatBody["stream"])
	if wantStream && chatBody["stream_options"] == nil {
		chatBody = mergeBody(chatBody, map[string]any{"stream_options": map[string]any{"include_usage": true}})
	}

	// OpenRouter 分支：未配 key 时 Python 版回 Anthropic 形状 401（authentication_error）
	if denied, _ := s.guardOpenRouter(modelID); denied {
		writeJSON(w, http.StatusUnauthorized, adapters.AnthropicErrorBody(
			"authentication_error", "OpenRouter API key 未配置"))
		return
	}

	resp, serr := s.dispatch(r, chatBody, rid, modelID)
	if serr != nil {
		writeJSON(w, serr.Status, serr.Body)
		return
	}
	defer resp.Body.Close()
	t0 := time.Now()

	if wantStream {
		s.streamAdapter(w, r, rid, t0, resp.Body, "messages", modelID, func(lines adapters.SSELines) iter_seq_bytes {
			return adapters.ChatSSEToAnthropic(lines.Seq, modelID)
		})
		return
	}

	content, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	content = upstream.UnwrapDataLayer(content)
	chatJSON, txt := decodeChatJSON(content)
	if p := DetectPseudoReply(txt); p != nil {
		s.logQuota(rid, p, modelID, "200 伪回复转 429")
		s.pseudoAnthropicHTTP(w, p, txt)
		return
	}
	if chatJSON == nil {
		writeJSON(w, http.StatusBadGateway, adapters.AnthropicErrorBody(
			"api_error", "upstream returned non-JSON body"))
		return
	}
	out := marshalCompact(adapters.ChatJSONToAnthropic(chatJSON, modelID))
	s.doneLog(rid, "messages", len(out), t0)
	writeRaw(w, http.StatusOK, "application/json; charset=utf-8", out)
}

// ── 共用：上游分流 / adapter 流转发 / 伪回复 ─────────────────────────────────

// dispatch 按模型前缀选上游通道（responses 与 messages 路由共用）。
//
// OpenRouter 未配 key 时，Python 版在 responses 路由回 OpenAI 形状 401、
// 在 messages 路由回 Anthropic 形状 401 —— 二者都从 dispatch 之前返回。
// 为保留这个形状差异，调用方先判 IsOpenRouterModel+OrKey；这里只兜住
// dispatch 内部不再重复判定的情形。
func (s *Server) dispatch(r *http.Request, chatBody map[string]any, rid, modelID string) (*http.Response, *upstream.SendError) {
	if config.IsOpenRouterModel(modelID) {
		orBody := mergeBody(chatBody, map[string]any{"model": config.OpenRouterUpstreamModel(modelID)})
		return s.Up.SendOpenRouter(r.Context(), marshalCompact(orBody), rid)
	}
	return s.Up.SendCodingPlan(r.Context(), s, marshalCompact(chatBody), rid, modelID)
}

// guardOpenRouter 两协议路由的 OpenRouter key 检查（错误体形状由调用方决定）。
// 返回 (denied, message)。
func (s *Server) guardOpenRouter(modelID string) (bool, string) {
	if !config.IsOpenRouterModel(modelID) || s.OrKey != "" {
		return false, ""
	}
	return true, "OpenRouter API key 未配置（在 ~/.atomcode/config.toml 或 OPENROUTER_API_KEY 环境变量）"
}

// iter_seq_bytes adapter 输出帧序列（iter.Seq[[]byte] 的具名别名，便于传参）。
type iter_seq_bytes = func(func([]byte) bool)

// streamAdapter 通用 adapter 流转发：上游 body → 行流（带伪回复前置拦截）→
// adapter 帧流 → 客户端。对应 Python chat_lines() + generate()。
func (s *Server) streamAdapter(
	w http.ResponseWriter, r *http.Request, rid string, t0 time.Time,
	upstreamBody io.Reader, kind, modelID string,
	build func(adapters.SSELines) iter_seq_bytes,
) {
	sseHeaders(w)
	flusher, _ := w.(http.Flusher)

	lines, errRef := sseLineFeed(upstreamBody)
	// 行流前置拦截：首个内容行命中伪回复就吐一条 SSE error 行并终止行流。
	// Python 用紧凑分隔符 json.dumps(separators=(",",":"))，与 adapter 帧同形。
	filtered := func(yield func(string) bool) {
		intercept := true
		for line := range lines {
			if intercept {
				if c := FirstContentLine(line); c != "" {
					if p := DetectPseudoReply(c); p != nil {
						s.logQuota(rid, p, modelID, "流内伪回复替换为 SSE error")
						frame := marshalCompact(map[string]any{"error": ssePseudoErrFields(p, c)})
						if !yield("data: " + string(frame)) {
							return
						}
						return
					}
					intercept = false
				}
			}
			if !yield(line) {
				return
			}
		}
	}

	total := 0
	for frame := range build(adapters.SSELines{Seq: filtered, Err: errRef}) {
		if err := r.Context().Err(); err != nil {
			s.logDisconnect(rid, kind, total, t0)
			return
		}
		if _, werr := w.Write(frame); werr != nil {
			s.logDisconnect(rid, kind, total, t0)
			return
		}
		total += len(frame)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// 上游行流读取报错（非 EOF）：Python 版由 generate() 的 except Exception 记
	// 「流中断」；adapter 已尽力收尾，这里只补日志，不再改已发内容。
	if err := errRef(); err != nil {
		s.Log.Error("流中断", slog.String("rid", rid), slog.String("err", err.Error()),
			slog.String("line", fmt.Sprintf("%s %dB %.1fs", kind, total, time.Since(t0).Seconds())))
		return
	}
	s.doneLog(rid, kind, total, t0)
}

// ssePseudoErrFields 伪回复 → 流内 error 帧字段（含 retry_after，与 Python 一致）。
func ssePseudoErrFields(p *PseudoResult, message string) map[string]any {
	return map[string]any{
		"message":     message,
		"type":        ErrTypeForKind(p.Kind),
		"code":        p.Kind,
		"reset_at":    nullable(p.Reset),
		"retry_after": nullableSec(RetryAfterSeconds(p.Reset)),
	}
}

// pseudoOpenAIHTTP 非流式伪回复 → 429（chat/responses 的 OpenAI 错误体 + Retry-After）。
func (s *Server) pseudoOpenAIHTTP(w http.ResponseWriter, p *PseudoResult, message string) {
	if sec := RetryAfterSeconds(p.Reset); sec > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(sec))
	}
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
		"message":  message,
		"type":     ErrTypeForKind(p.Kind),
		"code":     p.Kind,
		"reset_at": nullable(p.Reset),
	}})
}

// pseudoAnthropicHTTP 非流式伪回复 → 429（Anthropic 错误体只有 type/message）。
func (s *Server) pseudoAnthropicHTTP(w http.ResponseWriter, p *PseudoResult, message string) {
	if sec := RetryAfterSeconds(p.Reset); sec > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(sec))
	}
	writeJSON(w, http.StatusTooManyRequests,
		adapters.AnthropicErrorBody(ErrTypeForKind(p.Kind), message))
}

// logQuota 伪回复命中日志（Python: "[%s] quota(%s) model=%s reset=%s —— %s"）。
func (s *Server) logQuota(rid string, p *PseudoResult, modelID, what string) {
	s.Log.Warn("quota", slog.String("rid", rid),
		slog.String("line", fmt.Sprintf("quota(%s) model=%s reset=%s —— %s",
			p.Kind, orQ(modelID), orQ(p.Reset), what)))
}

// ── 小工具 ───────────────────────────────────────────────────────────────────

// mergeBody 浅合并（对应 Python dict(body, **extra)：extra 覆盖同名键）。
func mergeBody(base map[string]any, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func joinAll(parts []string) string {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	b := make([]byte, 0, n)
	for _, p := range parts {
		b = append(b, p...)
	}
	return string(b)
}

func fmtTTFT(d time.Duration, ok bool) string {
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// nullable 空串 → JSON null（Python 的 reset=None 直接进错误体）。
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableSec 0 → JSON null（Python _retry_after_seconds(None) → None）。
func nullableSec(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// marshalSpaced Python json.dumps 默认分隔符（", " / ": "）形态，
// 用于 chat 路由流内 error 帧（那处 Python 未指定 separators）。
func marshalSpaced(v any) []byte {
	b := marshalCompact(v)
	var out []byte
	inString := false
	escaped := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			out = append(out, c)
		case ',':
			out = append(append(out, c), ' ')
		case ':':
			out = append(append(out, c), ' ')
		default:
			out = append(out, c)
		}
	}
	return out
}

// usageFromSSELine 取一行 SSE data 里的 usage（无则 nil）。
func usageFromSSELine(line string) map[string]any {
	d := ssePayload(line)
	if d == nil {
		return nil
	}
	return asMap(d["usage"])
}

// ssePayload 解析 "data: {...}" 行为对象；非 data 行 / [DONE] / 解析失败返回 nil。
func ssePayload(line string) map[string]any {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "data:") {
		return nil
	}
	payload := strings.TrimSpace(s[len("data:"):])
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	var d map[string]any
	if json.Unmarshal([]byte(payload), &d) != nil {
		return nil
	}
	return d
}
