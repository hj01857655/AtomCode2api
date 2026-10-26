// Package lanes 的 OAuthLogin：acs.atomgit.com 平台代持流，无需 client_id/PKCE。
//
// 流程（源码坐标 crates/atomcode-auth/src/oauth.rs）：
//  1. GET  /auth/login?provider=atomgit → {login_url, state}
//  2. 用户浏览器打开 login_url 授权（可剥掉 force_login=true 免二次确认）
//  3. GET  /auth/check?state=<s> → {valid: true} 表示已授权
//  4. GET  /auth/token?state=<s> → {access_token, refresh_token, expires_in, user{...}}
//
// 一次登录会话：发起 → 轮询 → 完成。进程内同时只保留一个活动会话（handler 层持有单例）。
package lanes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	acsBase    = "https://acs.atomgit.com"
	oauthTTL   = 600 * time.Second // 会话超时（10 分钟），与 poll() 判定一致
	oauthState = "atomgit"
)

// OAuthUser /auth/token 返回的用户信息。id 上游有时给数字有时给字符串，
// 用 json.RawMessage 收着，落盘时原样去引号（对齐 Python str(user["id"])）。
type OAuthUser struct {
	ID        json.RawMessage `json:"id"`
	Username  string          `json:"username"`
	Name      string          `json:"name"`
	Email     string          `json:"email"`
	AvatarURL string          `json:"avatar_url"`
}

// IDStr 用户 id 的字符串形态：JSON 数字去引号、带引号字符串剥引号。
func (u OAuthUser) IDStr() string {
	raw := strings.TrimSpace(string(u.ID))
	if raw == "null" {
		return ""
	}
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		var s string
		if json.Unmarshal(u.ID, &s) == nil {
			return s
		}
	}
	return raw
}

// OAuthCreds /auth/token 交换出的完整凭据（对应 _exchange 返回的 dict）。
type OAuthCreds struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresIn    int64     `json:"expires_in"`
	CreatedAt    int64     `json:"created_at"`
	User         OAuthUser `json:"user"`
}

// OAuthLogin 一次登录会话。零值不可用，经 NewOAuthLogin 构造。
type OAuthLogin struct {
	HTTP *http.Client
	// Base 非空时把 acs.atomgit.com 的绝对 URL 主机替换成它（httptest 注入用）。
	Base string

	mu        sync.Mutex
	state     string
	loginURL  string
	createdAt time.Time
}

func NewOAuthLogin() *OAuthLogin {
	return &OAuthLogin{HTTP: &http.Client{Timeout: reqTimeout}}
}

// LoginURL 当前授权 URL（start 后可读）。
func (o *OAuthLogin) LoginURL() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.loginURL
}

// Start GET /auth/login 发起会话，取 {login_url, state}，剥掉 force_login=true。
// 对应 Python start()（含 raise_for_status 与 data["state"]/data["login_url"] 的
// KeyError：缺任一字段都算发起失败，路由层回 502）。
func (o *OAuthLogin) Start() error {
	raw, status, err := o.fetch(acsBase+"/auth/login", url.Values{"provider": {oauthState}})
	if err != nil {
		return fmt.Errorf("OAuth 发起失败：%w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("OAuth 发起失败：HTTP %d: %s", status, trunc(string(raw), 200))
	}
	var data struct {
		State    string `json:"state"`
		LoginURL string `json:"login_url"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("OAuth 发起失败：%w", err)
	}
	if data.State == "" || data.LoginURL == "" {
		return fmt.Errorf("OAuth 发起失败：响应缺少 state/login_url")
	}
	loginURL := stripForceLogin(data.LoginURL)
	o.mu.Lock()
	o.state = data.State
	o.loginURL = loginURL
	o.createdAt = time.Now()
	o.mu.Unlock()
	slog.Info("OAuth 会话已发起", slog.String("state", truncate(data.State, 8)))
	return nil
}

// Poll 返回 (凭据|nil, 状态, 错误)。已授权时完成交换并返回凭据，否则 (nil, 原因, nil)。
// 对应 Python poll()：未发起 / 超时 / waiting / done。超时后清空 state（需重新 /login）。
//
// err 非 nil 对应 Python 里 poll() 冒泡出的异常（网络错、JSON 解析失败），
// 路由层因此回 502 {"status":"error"}；"waiting" 只在 check 请求本身成功但
// valid 为假时出现（Python: `if not (resp.ok and resp.json().get("valid"))`）。
func (o *OAuthLogin) Poll() (*OAuthCreds, string, error) {
	o.mu.Lock()
	state := o.state
	created := o.createdAt
	o.mu.Unlock()

	if state == "" {
		return nil, "login 未发起，先 GET /login", nil
	}
	if time.Since(created) > oauthTTL {
		o.mu.Lock()
		o.state = ""
		o.mu.Unlock()
		return nil, "会话已超时（10 分钟），请重新 /login", nil
	}

	raw, status, err := o.fetch(acsBase+"/auth/check", url.Values{"state": {state}})
	if err != nil {
		return nil, "", err
	}
	if status == http.StatusOK {
		var check struct {
			Valid bool `json:"valid"`
		}
		if uerr := json.Unmarshal(raw, &check); uerr != nil {
			return nil, "", uerr
		}
		if check.Valid {
			creds, eerr := o.exchange(state)
			if eerr != nil {
				return nil, "", eerr
			}
			return creds, "done", nil
		}
	}
	return nil, "waiting", nil
}

// exchange GET /auth/token?state=<s> → 凭据；state 一次性，用完即弃。
// 对应 Python _exchange()（raise_for_status + data["access_token"] KeyError）。
func (o *OAuthLogin) exchange(state string) (*OAuthCreds, error) {
	raw, status, err := o.fetch(acsBase+"/auth/token", url.Values{"state": {state}})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", status, trunc(string(raw), 200))
	}
	var data struct {
		AccessToken  string    `json:"access_token"`
		TokenType    *string   `json:"token_type"`
		RefreshToken string    `json:"refresh_token"`
		ExpiresIn    *int64    `json:"expires_in"`
		User         OAuthUser `json:"user"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data.AccessToken == "" {
		return nil, errors.New("token 交换响应缺少 access_token")
	}
	o.mu.Lock()
	o.state = "" // state 一次性
	o.mu.Unlock()

	tokenType := "Bearer"
	if data.TokenType != nil && *data.TokenType != "" {
		tokenType = *data.TokenType
	}
	expiresIn := int64(defaultExpiresIn)
	if data.ExpiresIn != nil {
		expiresIn = *data.ExpiresIn
	}
	return &OAuthCreds{
		AccessToken:  data.AccessToken,
		TokenType:    tokenType,
		RefreshToken: data.RefreshToken,
		ExpiresIn:    expiresIn,
		CreatedAt:    time.Now().Unix(),
		User:         data.User,
	}, nil
}

// ToSaveCreds 把 OAuth 交换结果转成落盘凭据（user.id 存在才带 [user] 表）。
func (cr *OAuthCreds) ToSaveCreds() SaveCreds {
	sc := SaveCreds{
		AccessToken:  cr.AccessToken,
		TokenType:    orDefault(cr.TokenType, "Bearer"),
		RefreshToken: cr.RefreshToken,
		ExpiresIn:    cr.ExpiresIn,
		CreatedAt:    cr.CreatedAt,
		User: User{
			ID:        cr.User.IDStr(),
			Username:  cr.User.Username,
			Name:      cr.User.Name,
			Email:     cr.User.Email,
			AvatarURL: cr.User.AvatarURL,
		},
	}
	sc.HasUser = sc.User.ID != ""
	return sc
}

// fetch 发一次 GET，返回 (body, status, err)。URL 由 o.base(endpoint) 拼出——
// Base 被测试指向 httptest 时，acs.atomgit.com 的常量基址不参与拼接。
// 请求错误（网络失败）冒泡成 error；HTTP 状态码本身交给调用方判定
// （对齐 Python：start/_exchange 走 raise_for_status，poll 的 check 用 resp.ok）。
func (o *OAuthLogin) fetch(u string, q url.Values) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), reqTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base(u)+"?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", UpstreamUA)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// base 把绝对 URL 的 scheme://host 换成 o.Base（为空则原样返回）。
// 只替换主机部分、保留路径，因此测试注入时端点路径仍与真实上游一致。
func (o *OAuthLogin) base(u string) string {
	if o.Base == "" {
		return u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return o.Base + u
	}
	b, err := url.Parse(o.Base)
	if err != nil {
		return o.Base + u
	}
	parsed.Scheme, parsed.Host = b.Scheme, b.Host
	return parsed.String()
}

// stripForceLogin 剥掉 force_login=true：已登录 atomgit.com 的浏览器可免确认直接授权。
// 复刻 Python 三连 replace 的顺序语义。
func stripForceLogin(u string) string {
	u = strings.ReplaceAll(u, "&force_login=true", "")
	u = strings.ReplaceAll(u, "?force_login=true&", "?")
	u = strings.ReplaceAll(u, "?force_login=true", "")
	return u
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
