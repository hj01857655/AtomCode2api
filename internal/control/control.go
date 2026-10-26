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

// ClaimPlan 领取 CodingPlan Lite-体验版（幂等，启动时调一次）。失败不致命。
func (c *Client) ClaimPlan(cred upstream.Cred) {
	body, _ := json.Marshal(map[string]string{"plan_type": "Lite"})
	resp, err := c.do(http.MethodPost, c.Base+"/claim-v2", cred.Headers(), body)
	if err != nil {
		slog.Warn("[claim] 失败", slog.String("err", err.Error()))
		return
	}
	defer resp.Body.Close()
	var data map[string]any
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(raw, &data)
	switch {
	case truthy(data["success"]):
		slog.Info("[claim] 领取成功", slog.String("plan_name", asStr(data["plan_name"])),
			slog.String("有效期", "7 天后"))
	case truthy(data["duplicate"]):
		slog.Info("[claim] 已领取", slog.String("plan_name", asStr(data["plan_name"])))
	default:
		slog.Info("[claim] 返回", slog.String("data", string(raw)))
	}
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
