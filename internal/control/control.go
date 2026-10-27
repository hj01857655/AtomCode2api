// Package control 对应 Python 版 server.py 的控制面函数：
// claim_plan / get_status / fetch_models（api.gitcode.com/api/v5/coding-plan）。
//
// 控制面仅需 Bearer（cred.Headers()），免签名。
//
// 重要（docs/atomcode-02）：status-v2 只描述官方客户端路径的窗口状态，
// 反代路径（api-ai.gitcode.com + 中性 UA）不被窗口硬闸拦，勿以 status 判断可用性。
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"atomcode2api/internal/upstream"
)

const reqTimeout = 30 * time.Second

// Client 控制面。Base 可注入 httptest 假上游。
type Client struct {
	Base string // https://api.gitcode.com/api/v5/coding-plan
	HTTP *http.Client
}

func New() *Client {
	return &Client{Base: controlBase, HTTP: &http.Client{Timeout: reqTimeout}}
}

const controlBase = "https://api.gitcode.com/api/v5/coding-plan"

// claimTiers 领取优先级：高到低级联，命中即停。
// 社区脚本 L0NE-6/AtomCode-Daily 口径（Max → Pro → Lite）。
var claimTiers = []string{"Max", "Pro", "Lite"}

// ClaimResult 一次领取的结果（面板逐档展示）。
type ClaimResult struct {
	Tier    string `json:"tier"`
	OK      bool   `json:"ok"`
	Held    bool   `json:"held"` // 已持有该档（无变化），停止级联
	Plan    string `json:"plan,omitempty"`
	PlanType string `json:"plan_type,omitempty"`
	Message string `json:"message"`
}

// ClaimBest 按 Max → Pro → Lite 级联领取，命中即停。
//
// 幂等判据是 plan_type 而非 duplicate：实测（2026-10-27）已持有套餐时上游
// 返回 {"success":true,"duplicate":false,"message":"领取成功","plan_type":"Lite-beta"}，
// duplicate 恒为 false，不能用它判断「已领过」。改为先查 status-v2 拿到当前
// plan_type，领取后比对——一致即视为已持有，避免每天重复刷。
func (c *Client) ClaimBest(cred upstream.Cred) ClaimResult {
	before := currentPlanType(c.Status(cred))
	var last ClaimResult
	for _, tier := range claimTiers {
		res := c.ClaimTier(cred, tier)
		if res.OK {
			if before != "" && res.PlanType == before {
				res.Held = true
				res.Message = "已持有 " + res.Plan + "（无变化），无需重复领取"
			}
			return res
		}
		last = res
	}
	if last.Message == "" {
		last.Message = "三档均未领取（上游未说明原因）"
	}
	return last
}

// currentPlanType 从 status-v2 里取当前持有的 plan_type（取不到返回空串）。
func currentPlanType(status map[string]any) string {
	if status == nil {
		return ""
	}
	free := asMap(status["codingplan_free"])
	return asStr(free["plan_type"])
}

// ClaimTier 领取指定档位。返回结构化结果，不写日志（由调用方决定）。
func (c *Client) ClaimTier(cred upstream.Cred, tier string) ClaimResult {
	res := ClaimResult{Tier: tier}
	body, _ := json.Marshal(map[string]string{"plan_type": tier})
	resp, err := c.do(http.MethodPost, c.Base+"/claim-v2", cred.Headers(), body)
	if err != nil {
		res.Message = "请求失败: " + err.Error()
		return res
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	res.Plan = asStr(data["plan_name"])
	res.PlanType = asStr(data["plan_type"])
	res.Message = firstNonEmptyStr(asStr(data["message"]), asStr(data["msg"]))
	switch {
	case truthy(data["duplicate"]):
		// 兼容上游未来真的返回 duplicate 的情况。
		res.OK, res.Held = true, true
		if res.Message == "" {
			res.Message = "已持有 " + tier + " 档套餐，无需重复领取"
		}
	case truthy(data["success"]):
		res.OK = true
		if res.Message == "" {
			res.Message = "已领取 " + tier + " 档免费套餐"
		}
	default:
		if res.Message == "" {
			res.Message = "未领取（HTTP " + itoa(resp.StatusCode) + "）"
		}
	}
	return res
}

// ClaimPlan 启动时的自动领取：级联领一次并记日志。失败不致命。
func (c *Client) ClaimPlan(cred upstream.Cred) {
	res := c.ClaimBest(cred)
	if res.OK {
		slog.Info("[claim] 领取完成", slog.String("tier", res.Tier),
			slog.String("plan", res.Plan), slog.String("msg", res.Message))
		return
	}
	slog.Warn("[claim] 未领取", slog.String("tier", res.Tier), slog.String("msg", res.Message))
}

// Status GET /status-v2?plan_type=Lite；失败返回 nil（对应 Python 的 except → None）。
func (c *Client) Status(cred upstream.Cred) map[string]any {
	resp, err := c.do(http.MethodGet, c.Base+"/status-v2?plan_type=Lite", cred.Headers(), nil)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var data map[string]any
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	return data
}

// Models GET /models-v2?plan_type=Lite 模型目录；非 200 或解析失败返回 error。
func (c *Client) Models(cred upstream.Cred) ([]map[string]any, error) {
	resp, err := c.do(http.MethodGet, c.Base+"/models-v2?plan_type=Lite", cred.Headers(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("控制面 HTTP %d: %s", resp.StatusCode, trunc(string(raw), 300))
	}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("控制面响应解析失败：%w", err)
	}
	list, ok := data.([]any)
	if !ok {
		// models-v2 直接返回数组；万一包了对象，取 data/models 字段兜底
		if m := asMap(data); m != nil {
			if inner, ok := m["data"].([]any); ok {
				list = inner
			} else if inner, ok := m["models"].([]any); ok {
				list = inner
			}
		}
		if list == nil {
			return nil, fmt.Errorf("控制面 models-v2 响应形状不符预期")
		}
	}
	out := make([]map[string]any, 0, len(list))
	for _, mi := range list {
		if m := asMap(mi); m != nil {
			out = append(out, m)
		}
	}
	return out, nil
}

func (c *Client) do(method, url string, h http.Header, body []byte) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), reqTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	if h != nil {
		req.Header = h
	} else {
		req.Header = http.Header{}
	}
	req.Header.Set("User-Agent", "ac2api/0.1.0")
	return c.HTTP.Do(req)
}

func truthy(v any) bool {
	b, _ := v.(bool)
	return b
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
