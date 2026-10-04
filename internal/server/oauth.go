// oauth.go 管理面 OAuth 设备授权端点 + 安全中间件（限速/同源/安全头）。
//
// 三个端点（走 withAuth）：
//   - POST /admin/api/oauth/start      发起登录，返回 {id,url}
//   - POST /admin/api/oauth/{id}/poll  轮询状态机（waiting/success/error/timeout）
//   - GET  /admin/api/logs?limit=N     读环形日志缓冲
//
// 热加载闭环：poll 成功分支 SaveAtomic 落盘 + Pool.Add 立即入池，0 重启 0 延迟。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/oauth"
)

// oauthRateLimit 10 次 / 15min，按来源 IP 隔离。
const (
	oauthRateLimitCount  = 10
	oauthRateLimitWindow = 15 * time.Minute
)

// ---------------------------------------------------------------------------
// 安全头
// ---------------------------------------------------------------------------

// setAdminSecurityHeaders 管理面安全头（挂在所有 /admin/* 响应上，见 Handler.ServeHTTP）。
func setAdminSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'")
}

// isAdminPath 判定路径是否落在管理面（"/admin" 或 "/admin/..."）。
func isAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/")
}

// ---------------------------------------------------------------------------
// 同源校验
// ---------------------------------------------------------------------------

// sameOrigin 判定 Origin 与 Host 的 scheme+host+port 全等（大小写不敏感）。
// origin 须为完整 http(s)://host[:port]；host 为请求的 Host（含端口）。
func sameOrigin(origin, host string) bool {
	origin = strings.TrimSuffix(origin, "/")
	return strings.EqualFold(origin, "http://"+host) || strings.EqualFold(origin, "https://"+host)
}

// sameOriginGuard 非 GET/HEAD 请求校验 Origin 同源：Origin 存在且不同源 → 403。
// Origin 缺失（非浏览器客户端，如 curl/脚本）放行——浏览器跨站 POST 必带 Origin，
// CSRF 场景仍被拦截；放行缺失 Origin 以兼容管理面 curl 实测（验收 §5）。
func sameOriginGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "跨站请求被拒绝"})
				return
			}
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// 限速
// ---------------------------------------------------------------------------

type rateWindow struct {
	count int
	first time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]*rateWindow
	limit   int
	window  time.Duration
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{windows: map[string]*rateWindow{}, limit: limit, window: window}
}

// allow 报告 key 是否放行；不放行时返回建议的 Retry-After（窗口剩余时长）。
func (r *rateLimiter) allow(key string) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	w := r.windows[key]
	if w == nil || now.Sub(w.first) >= r.window {
		r.windows[key] = &rateWindow{count: 1, first: now}
		return true, 0
	}
	if w.count >= r.limit {
		return false, r.window - now.Sub(w.first)
	}
	w.count++
	return true, 0
}

// clear 成功时清零来源 IP 的计数。
func (r *rateLimiter) clear(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.windows, key)
}

// source 提取来源 IP（RemoteAddr 去端口）。
func source(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// oauthRateLimit 限速中间件：超限 429 + Retry-After。
func (h *Handler) oauthRateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := source(r)
		if ok, retry := h.oauthRL.allow(key); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "请求过于频繁，请稍后再试"})
			return
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// 端点
// ---------------------------------------------------------------------------

// decodeOAuthBody 解请求体（上限 64KB）。
func decodeOAuthBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少请求体"})
		return false
	}
	defer r.Body.Close()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return false
	}
	return true
}

// logf 写管理面环形日志（脱敏在 LogBuffer.Write 内完成）。
func (h *Handler) logf(format string, args ...any) {
	if h.cfg.Log != nil {
		h.cfg.Log.Write(fmt.Sprintf(format, args...))
	}
}

// adminOAuthStart 发起 OAuth 登录（body: {"region":"cn"|"global"}）。
func (h *Handler) adminOAuthStart(w http.ResponseWriter, r *http.Request) {
	if h.oauth == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "oauth 未初始化"})
		return
	}
	var in struct {
		Region string `json:"region"`
	}
	if !decodeOAuthBody(w, r, &in) {
		return
	}
	id, url, err := h.oauth.Start(in.Region)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	h.logf("oauth start region=%s id=%s", in.Region, id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "url": url})
}

// adminOAuthPoll 轮询授权状态机。
func (h *Handler) adminOAuthPoll(w http.ResponseWriter, r *http.Request) {
	if h.oauth == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "oauth 未初始化"})
		return
	}
	id := r.PathValue("id")
	flow, err := h.oauth.Poll(id)
	if err != nil {
		switch {
		case errors.Is(err, oauth.ErrUnknownID):
			// 未知 id 与 timeout 归一，防枚举。
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": "流程已失效，请重新发起"})
		case errors.Is(err, oauth.ErrReadOnly):
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "服务端已开启只读模式"})
		default:
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		}
		return
	}
	switch flow.Status {
	case oauth.StatusWaiting:
		writeJSON(w, http.StatusOK, map[string]any{"status": "waiting"})
	case oauth.StatusTimeout:
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": "流程已失效，请重新发起"})
	case oauth.StatusError:
		h.logf("oauth poll id=%s status=error", id)
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": "登录失败，请重试"})
	case oauth.StatusSuccess:
		a := flow.Auth
		// uid 校验：写盘前必过 ^[A-Za-z0-9._-]{6,128}$，失败 500。
		if !auth.ValidUID(a.UID) {
			h.logf("oauth poll id=%s 非法 uid 拒绝落盘", id)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "非法 uid"})
			return
		}
		a.FilePath = filepath.Join(h.cfg.AuthDir, "workbuddy-"+a.UID+".json")
		a.BackfillRealm() // 按 domain 补 realm 标识，保证落盘带 realm 键
		if err := a.SaveAtomic(); err != nil {
			h.logf("oauth poll id=%s save 失败: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "凭证落盘失败"})
			return
		}
		h.cfg.Pool.Add(a)
		h.oauthRL.clear(source(r)) // 成功清零限速
		h.logf("oauth poll id=%s uid=%s 成功入池（0 重启）", id, a.UID)
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "uid": a.UID, "nickname": a.Nickname})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": "waiting"})
	}
}

// adminLogs 读环形日志缓冲（?limit=N，缺省全量，最多 500 行）。
func (h *Handler) adminLogs(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	lines := []string{}
	if h.cfg.Log != nil {
		lines = h.cfg.Log.snapshot(limit)
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}
