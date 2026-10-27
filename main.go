// atomcode2api — AtomCode CodingPlan 本地 OpenAI 兼容网关（Go 版）。
//
// 把 AtomCode 的 CodingPlan 免费额度包装成标准 OpenAI Chat Completions API，
// 外加 /v1/responses 与 /v1/messages 两种协议面。任意支持自定义 Base URL 的
// 客户端指向 http://127.0.0.1:8788/v1 即可使用。
//
// 启动:
//
//	go build -o atomcode2api.exe .
//	./atomcode2api.exe
//
// 环境变量：HOST / PORT / API_KEY（设置后要求 Bearer 本地鉴权，或写在 .env）。
//
// 关键约束：数据面 UA 绝不能以 atomcode 开头 —— 那是 UA 门的开关
// （docs/atomcode-03-数据面与UA门.md：必须同时换主机和换 UA）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"atomcode2api/internal/config"
	"atomcode2api/internal/handler"
	"atomcode2api/internal/lanes"
)

func main() {
	cfg := config.Load()

	var credPath string
	flag.StringVar(&credPath, "auth", "", "凭据文件路径（默认 ~/.atomcode/auth.toml）")
	flag.Parse()

	logger := handler.NewLogger(cfg.StateDir)
	slog.SetDefault(logger)

	s := handler.New(cfg, nil, logger)
	s.AuthTOML = credPath                                 // 空串时 lanes 内部回落到默认路径
	s.AuthJSON = filepath.Join(cfg.StateDir, "auth.json") // 网关独立凭据文件（刷新/登录产物）

	// 对应 Python __main__ 的凭据装载分支
	cred, err := lanes.NewCredential(s.AuthTOML, s.AuthJSON, logger)
	if err != nil {
		if errors.Is(err, lanes.ErrNotLoggedIn) {
			logger.Warn("尚未登录，CodingPlan 路由不可用——GET /login 开始 OAuth 登录（OpenRouter 路由不受影响）")
		} else {
			logger.Error("凭据装载失败", slog.String("err", err.Error()))
		}
	} else {
		username := cred.Username()
		if username == "" {
			username = "未命名"
		}
		logger.Info("凭证账号", slog.String("username", username), slog.String("uid", cred.UserID()))
		if cred.IsExpired() {
			logger.Info("token 已过期，尝试刷新...")
			if err := cred.Refresh(); err != nil {
				logger.Error("token 刷新失败", slog.String("err", err.Error()))
			}
		}
		s.SetCred(cred)
		s.Con.ClaimPlan(s)
		printStatus(s)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		// 不设 ReadTimeout/WriteTimeout：长流式回答不能被整体时限掐掉
		// （对齐 requests stream=True + timeout=120 的空闲超时语义，
		//  上游侧空闲上限在 upstream.Transport 层实现）
		IdleTimeout: 120 * time.Second,
	}

	fmt.Printf("atomcode2api 已启动: http://%s:%d\n", cfg.Host, cfg.Port)
	fmt.Printf("OpenAI Base URL: http://%s:%d/v1\n", cfg.Host, cfg.Port)
	fmt.Println("CodingPlan 模型: qwen3.8-27b, glm5.3-flash")
	if cfg.OpenRouterAPIKey != "" {
		fmt.Printf("OpenRouter 模型: %d 个免费模型已接入\n", len(config.OpenRouterModels))
	} else {
		fmt.Println("OpenRouter: 未配置 API key（跳过免费模型）")
	}
	if cfg.APIKey != "" {
		fmt.Println("本地鉴权: 已开启（API_KEY）")
	} else {
		fmt.Println("本地鉴权: 未开启（仅回环）")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("监听失败", slog.String("addr", addr), slog.String("err", err.Error()))
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
}

// printStatus 启动时打套餐/窗口两行（对应 Python 的 print，缺字段给 None）。
func printStatus(s *handler.Server) {
	st := s.Con.Status(s)
	if len(st) == 0 {
		return
	}
	free, usage := statusSections(st)
	fmt.Printf("套餐: %s | 到期: %s | 剩余天数: %s\n",
		pyStr(free["plan_name"]), pyStr(free["expires_at"]), pyStr(free["remaining_days"]))
	fmt.Printf("窗口: %s/%s | 重置: %s\n",
		pyStr(usage["window_tokens_used"]), pyStr(usage["window_token_limit"]), pyStr(usage["reset_at"]))
}

// statusSections 拆 codingplan_free / current_usage 两段（缺失给空 map）。
func statusSections(st map[string]any) (map[string]any, map[string]any) {
	free, _ := st["codingplan_free"].(map[string]any)
	usage, _ := st["current_usage"].(map[string]any)
	if free == nil {
		free = map[string]any{}
	}
	if usage == nil {
		usage = map[string]any{}
	}
	return free, usage
}

// pyStr JSON 值的 Python str() 形态（None / 整数 / 浮点）。
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%v", x)
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return fmt.Sprintf("%v", v)
}
