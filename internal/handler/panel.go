// /admin 控制面：内置面板页 + overview / refresh-token 两个面板后端。
// panel.html 内嵌一份（//go:embed），状态目录里有同名文件时优先用用户的——
// 内置版只是让 /admin 开箱即用，不依赖手动安装步骤。
package handler

import (
	_ "embed"
	"net/http"
	"os"

	"atomcode2api/internal/config"
)

//go:embed panel.html
var panelBuiltin []byte

// servePanel 输出面板 HTML：状态目录 panel.html > 内置版。
func (s *Server) servePanel(w http.ResponseWriter, _ *http.Request) {
	raw, err := os.ReadFile(s.Cfg.StateFilePath("panel.html"))
	if err != nil || len(raw) == 0 {
		raw = panelBuiltin
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(raw)
}

// overview GET /admin/api/overview：面板概览数据。
// 形状 {credential:{valid,.../status 同形的套餐字段}, models, local_auth}；
// 凭据无效时仍返回（valid=false），模型拉取失败时置空——面板要能展示"未登录"，
// 所以这里不走 credGuard。
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	credential := map[string]any{"valid": false}
	if c := s.credOrNil(); c != nil {
		credential["valid"] = true
		credential["username"] = c.Username()
		credential["uid"] = c.UserID()
		// token 本体到期时间（凭据自动续期后跟着变），与套餐到期区分
		if e := c.ExpiresAt(); e > 0 {
			credential["token_expires_at"] = e
		}
		// /status 同形的套餐与 5h 窗口字段；拉不到不影响其余展示
		if st := s.quotaStatus(); st != nil {
			for k, v := range st {
				credential[k] = v
			}
		}
	}
	models := []any{}
	if up, err := s.Con.Models(s); err == nil {
		for _, m := range up {
			// plan_available 缺省 true（与 /v1/models 一致）
			if v, ok := m["plan_available"].(bool); ok && !v {
				continue
			}
			models = append(models, map[string]any{
				"id":       asString(m["display_model_name"]),
				"owned_by": "atomcode",
			})
		}
	}
	if s.OrKey != "" {
		for _, m := range config.OpenRouterModels {
			models = append(models, map[string]any{"id": m.ID, "owned_by": "openrouter"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"credential": credential,
		"models":     models,
		"local_auth": s.Cfg.APIKey != "",
	})
}

// adminRefresh POST /admin/api/refresh-token：强制刷新当前凭据（诊断页用）。
// Refresh 产出的新凭据落网关独立凭据文件 auth.json（不再回写 auth.toml）。
func (s *Server) adminRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	if err := s.Refresh(); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已刷新（auth.json）"})
}
