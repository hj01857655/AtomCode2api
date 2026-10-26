// 伪回复识别的离线回归（不联网），移植 Python tests/test_pseudo_detect.py：
// 真伪回复必须命中、撞词正常回答必须放行。
package handler

import (
	"fmt"
	"testing"
	"time"
)

// 真实上游模板（wire-dump 与 CLI 会话文件取证的原样，含未取证时段的同族变体）
var pseudoPositive = []struct {
	text  string
	kind  string
	reset string // "" = None
}{
	{"模型「glm5.3-flash」在当前 CodingPlan 用量窗口内已达上限。当前滚动窗口约在当日 03:13 结束并重置额度，请稍后重试。",
		"quota_exhausted", "03:13"},
	{"模型「qwen3.8-27b」在当前 CodingPlan 用量窗口内已达上限。当前滚动窗口约在当日 13:13 结束并重置额度，请稍后重试。",
		"quota_exhausted", "13:13"},
	{"模型「glm5.3-flash」的请求负载过高，请稍后再试。", "overloaded", ""},
	{"模型「qwen3.8-27b」的请求负载过高，请稍后再试。", "overloaded", ""},
	// 模板嵌在更长的正文里也必须命中（search 语义）
	{"抱歉，模型「glm5.3-flash」的请求负载过高，请稍后再试。", "overloaded", ""},
	// 无重置时刻的退化为 None reset，仍须识别为 quota
	{"模型「glm5.3-flash」在当前 CodingPlan 用量窗口内已达上限，请稍后重试。",
		"quota_exhausted", ""},
}

// 撞词的正常回答 / 转述场景——绝不能拦截
var pseudoNegative = []string{
	"「请求负载过高」指服务器或系统在单位时间内收到的请求数量超出了其处理能力",
	"HTTP 429 通常配合 Retry-After 头使用，表示请求负载过高时的标准限流响应",
	"用量窗口内已达上限这个说法来自 CodingPlan 的计费文案，含义是滚动配额用尽",
	"这句话可以这样写：用量窗口内已达上限。当前滚动窗口约在当日 03:13 结束并重置额度",
	"请实现一个处理请求负载过高的限流中间件",
	"当系统提示「请求负载过高，请稍后再试」时通常意味着服务过载",
}

func TestDetectPseudoReply(t *testing.T) {
	for _, tc := range pseudoPositive {
		got := DetectPseudoReply(tc.text)
		if got == nil {
			t.Errorf("漏判: %q → nil，期望 (%s, %q)", clip(tc.text, 44), tc.kind, tc.reset)
			continue
		}
		if got.Kind != tc.kind || got.Reset != tc.reset {
			t.Errorf("误判: %q → (%s, %q)，期望 (%s, %q)",
				clip(tc.text, 44), got.Kind, got.Reset, tc.kind, tc.reset)
		}
	}
	for _, text := range pseudoNegative {
		if got := DetectPseudoReply(text); got != nil {
			t.Errorf("误伤: %q → (%s, %q)", clip(text, 44), got.Kind, got.Reset)
		}
	}
	if got := DetectPseudoReply(""); got != nil {
		t.Errorf("空文本应返回 nil，得到 %+v", got)
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	// Python: wait 必须落在 (0, 86400]
	wait := RetryAfterSeconds("03:13")
	if wait <= 0 || wait > 86400 {
		t.Errorf("retry_after 异常: %d", wait)
	}
	if got := RetryAfterSeconds(""); got != 0 {
		t.Errorf("retry_after(\"\") 应为 0（= Python None），得到 %d", got)
	}
	if got := RetryAfterSeconds("03"); got != 0 {
		t.Errorf("retry_after(\"03\") 应为 0（不可解析），得到 %d", got)
	}
	if got := RetryAfterSeconds("ab:cd"); got != 0 {
		t.Errorf("retry_after(\"ab:cd\") 应为 0，得到 %d", got)
	}

	// 对齐 Python 的算术：目标时刻在今日已过时 +86400（跨天）
	lt := time.Now()
	nowSec := lt.Hour()*3600 + lt.Minute()*60 + lt.Second()
	for _, reset := range []string{"03:13", "13:13", "23:59", "00:00"} {
		hh, mm := splitHM(t, reset)
		delta := hh*3600 + mm*60 - nowSec
		if delta <= 0 {
			delta += 86400
		}
		if got := RetryAfterSeconds(reset); got != delta {
			t.Errorf("retry_after(%q) = %d，期望 %d（此刻 %s）", reset, got, delta, lt.Format("15:04:05"))
		}
	}
}

func splitHM(t *testing.T, s string) (int, int) {
	t.Helper()
	var hh, mm int
	if _, err := fmt.Sscanf(s, "%d:%d", &hh, &mm); err != nil {
		t.Fatalf("用例时刻格式错: %q", s)
	}
	return hh, mm
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
