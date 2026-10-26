// Package handler 对应 Python 版 server.py 的调度部分 + routes/ 三协议路由：
// 鉴权闸门、rid、in:/done: 日志、伪回复识别、控制面与登录端点。
//
// 本文件是 server.py 的公共设施：上游伪回复识别（额度耗尽/过载伪装成 200 的
// assistant 文本）与 Retry-After 换算。
package handler

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 上游把「额度耗尽 / 负载过高」伪装成正常 HTTP 200 的 assistant 文本回复，
// agent 客户端会当成模型真实回答。网关需要识别并转成正规错误。
// 实测无任何可区分的状态码/响应头，只能靠文案，但**不能按裸子串匹配**：
// 用户问「什么是「请求负载过高」」时模型正常解释也会含该短语，会被误判成过载。
// 因此锚定上游固定模板的共现特征（quota 三要素共现、overload 整句前后缀锚定）：
//
//	quota:    "模型「X」在当前 CodingPlan 用量窗口内已达上限。…约在当日 HH:MM 结束并重置额度…"
//	overload: "模型「X」的请求负载过高，请稍后再试。"
//
// Go 的 regexp 没有 re.S 开关语义差异问题：(?s) 内联开启 dotall，对齐 Python。
// .{0,80}? 非贪婪在 RE2 里等价（最左匹配 + 最短扩张语义由 RE2 保证）。
var (
	pseudoQuota    = regexp.MustCompile(`(?s)模型「[^」]{1,64}」在当前\s*CodingPlan\s*用量窗口内已达上限(?:.{0,80}?重置额度)?`)
	pseudoOverload = regexp.MustCompile(`(?s)模型「[^」]{1,64}」的请求负载过高，请稍后再试`)
	pseudoResetRE  = regexp.MustCompile(`约在当日\s*(\d{1,2}:\d{2})`)
)

// PseudoResult 命中结果：kind ∈ quota_exhausted / overloaded；Reset 形如 "03:13"（可空）。
type PseudoResult struct {
	Kind  string
	Reset string // "" = 未提取到重置时刻
}

// DetectPseudoReply 命中返回 *PseudoResult，否则 nil。对应 _detect_pseudo_reply。
func DetectPseudoReply(text string) *PseudoResult {
	if text == "" {
		return nil
	}
	kind := ""
	switch {
	case pseudoQuota.MatchString(text):
		kind = "quota_exhausted"
	case pseudoOverload.MatchString(text):
		kind = "overloaded"
	default:
		return nil
	}
	res := &PseudoResult{Kind: kind}
	if m := pseudoResetRE.FindStringSubmatch(text); m != nil {
		res.Reset = m[1]
	}
	return res
}

// ErrTypeForKind 伪回复种类 → OpenAI error type。
func ErrTypeForKind(kind string) string {
	if kind == "quota_exhausted" {
		return "rate_limit_error"
	}
	return "overloaded_error"
}

// RetryAfterSeconds 把 "03:13"（当日窗口重置时刻）换算成 Retry-After 秒数。
// 不可解析返回 0（调用方视为 None，不发 Retry-After 头）。
func RetryAfterSeconds(resetDisplay string) int {
	if resetDisplay == "" {
		return 0
	}
	parts := strings.Split(resetDisplay, ":")
	if len(parts) != 2 {
		return 0
	}
	hh, err1 := strconv.Atoi(parts[0])
	mm, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0
	}
	lt := time.Now()
	delta := hh*3600 + mm*60 - (lt.Hour()*3600 + lt.Minute()*60 + lt.Second())
	if delta <= 0 {
		delta += 86400
	}
	return delta
}

// FirstContentLine 从 Chat SSE 原始行提取 delta.content（非内容帧返回 ""）。
// 对应 _first_content_line。
func FirstContentLine(line string) string {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "data:") {
		return ""
	}
	payload := strings.TrimSpace(s[len("data:"):])
	if payload == "" || payload == "[DONE]" {
		return ""
	}
	var d map[string]any
	if json.Unmarshal([]byte(payload), &d) != nil {
		return ""
	}
	for _, chi := range asList(d["choices"]) {
		ch := asMap(chi)
		if ch == nil {
			continue
		}
		delta := asMap(ch["delta"])
		if delta == nil {
			continue
		}
		if c := asString(delta["content"]); c != "" {
			return c
		}
	}
	return ""
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
