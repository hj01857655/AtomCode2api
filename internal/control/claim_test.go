package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubCred 最小 Cred 实现（只提供 Headers，Refresh 不参与 claim 测试）。
type stubCred struct{}

func (stubCred) Headers() http.Header { return http.Header{} }
func (stubCred) Refresh() error       { return nil }

// ClaimBest 级联顺序：Max 拒绝 → Pro 拒绝 → Lite 成功，且请求体带正确 plan_type。
func TestClaimBestCascades(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/coding-plan/status-v2" {
			_, _ = w.Write([]byte(`{"codingplan_free":{}}`))
			return
		}
		if r.URL.Path != "/coding-plan/claim-v2" {
			t.Errorf("路径不符: %s", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		tier := body["plan_type"]
		got = append(got, tier)
		switch tier {
		case "Lite":
			_, _ = w.Write([]byte(`{"success":true,"plan_name":"CodingPlan Lite-体验版","plan_type":"Lite-beta"}`))
		default:
			_, _ = w.Write([]byte(`{"success":false,"message":"不在活动范围"}`))
		}
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL + "/coding-plan", HTTP: srv.Client()}
	res := c.ClaimBest(stubCred{})
	if !res.OK || res.Tier != "Lite" {
		t.Fatalf("应级联到 Lite 成功，得到 %+v", res)
	}
	want := []string{"Max", "Pro", "Lite"}
	if len(got) != len(want) {
		t.Fatalf("请求档位序列 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("请求档位序列 %v，期望 %v", got, want)
		}
	}
}

// 上游 duplicate 为 true 时视为已持有：立即停止级联，不再尝试更低档。
func TestClaimBestStopsOnDuplicate(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/coding-plan/status-v2" {
			_, _ = w.Write([]byte(`{"codingplan_free":{}}`))
			return
		}
		calls++
		_, _ = w.Write([]byte(`{"duplicate":true,"plan_name":"CodingPlan Max"}`))
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL + "/coding-plan", HTTP: srv.Client()}
	res := c.ClaimBest(stubCred{})
	if !res.OK || !res.Held || res.Tier != "Max" {
		t.Fatalf("duplicate 应判为已持有并停在 Max，得到 %+v", res)
	}
	if calls != 1 {
		t.Fatalf("duplicate 后应停止级联，实际请求 %d 次", calls)
	}
}

// 回归：上游对「已持有再领」返回 success=true + duplicate=false + 相同 plan_type
// （2026-10-27 实测）。必须按 plan_type 比对判为已持有，否则每日自动领取会重复刷。
func TestClaimBestDetectsHeldByPlanType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/coding-plan/status-v2" {
			_, _ = w.Write([]byte(`{"codingplan_free":{"plan_type":"Lite-beta","plan_name":"CodingPlan Lite-体验版"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"duplicate":false,"message":"领取成功","plan_type":"Lite-beta","plan_name":"CodingPlan Lite-体验版"}`))
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL + "/coding-plan", HTTP: srv.Client()}
	res := c.ClaimBest(stubCred{})
	if !res.OK {
		t.Fatalf("应判成功，得到 %+v", res)
	}
	if !res.Held {
		t.Fatalf("plan_type 未变化应判为已持有，得到 %+v", res)
	}
}

// 三档全拒绝：返回最后一次的结果，不 panic。
func TestClaimBestAllRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/coding-plan/status-v2" {
			_, _ = w.Write([]byte(`{"codingplan_free":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":false,"message":"活动已结束"}`))
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL + "/coding-plan", HTTP: srv.Client()}
	res := c.ClaimBest(stubCred{})
	if res.OK || res.Held {
		t.Fatalf("全拒绝不应判成功，得到 %+v", res)
	}
	if res.Tier != "Lite" {
		t.Fatalf("应返回最后一档 Lite，得到 %s", res.Tier)
	}
}
