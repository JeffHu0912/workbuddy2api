// admin.go 管理面板（/admin）：WorkBuddy 账号可视化与实时状态监控。
package server

import (
	_ "embed"
	"net/http"
	"sync"
	"time"

	"workbuddy2api/internal/pool"
)

//go:embed admin.html
var adminPageHTML []byte

// adminPage 返回内嵌 HTML 面板（深色简洁风，无外部依赖）。
func (h *Handler) adminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(adminPageHTML)
}

// adminCredits 查询全部账号的实时额度（并发拉取 CodeBuddy 上游）。
func (h *Handler) adminCredits(w http.ResponseWriter, r *http.Request) {
	type acct struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		Remain       int64  `json:"remain"`
		Cooling      bool   `json:"cooling"`
		Disabled     bool   `json:"disabled"`
		InFlight     int    `json:"in_flight"`
		SuccessCount int64  `json:"success_count"`
		ErrTotal     int64  `json:"err_total"`
		Reason       string `json:"reason,omitempty"`
		Error        string `json:"error,omitempty"`
	}

	st := h.cfg.Pool.List()
	out := make([]acct, len(st))
	var wg sync.WaitGroup
	for i, s := range st {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			a := h.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				out[i] = acct{UID: s.UID, Nickname: s.Nickname, Error: "no auth file found"}
				return
			}
			var ac acct
			ac.UID = s.UID
			ac.Nickname = s.Nickname
			ac.Cooling = s.Cooling
			ac.Disabled = s.Disabled
			ac.InFlight = s.InFlight
			ac.SuccessCount = s.SuccessCount
			ac.ErrTotal = s.ErrTotal
			ac.Reason = s.Reason

			remain, err := h.cfg.Upstream.UserResource(a)
			if err != nil {
				ac.Error = "billing: " + err.Error()
				ac.Remain = s.Credits // 回退内存旧额度
			} else {
				ac.Remain = remain
				h.cfg.Pool.SetCredits(s.UID, remain)
			}
			out[i] = ac
		}(i, s)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{
		"fetched_at": time.Now().Format("2006-01-02 15:04:05"),
		"accounts":   out,
	})
}

// adminAccounts 返回账号池运行态画像（脱敏）。
func (h *Handler) adminAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
	})
}

// adminResetCooldown 重置单个账号的冷却与熔断。
func (h *Handler) adminResetCooldown(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		uid = r.URL.Query().Get("uid")
	}
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid parameter is required")
		return
	}
	if !h.cfg.Pool.ResetCooldown(uid) {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found in pool")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"uid":     uid,
		"message": "cooldown and breaker reset successfully",
	})
}

// adminCheckin 手工批量触发每日签到并解冻有余额账号。
func (h *Handler) adminCheckin(w http.ResponseWriter, r *http.Request) {
	st := h.cfg.Pool.List()
	type resItem struct {
		UID     string `json:"uid"`
		Success bool   `json:"success"`
		Remain  int64  `json:"remain"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]resItem, len(st))
	var wg sync.WaitGroup
	for i, s := range st {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			a := h.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				results[i] = resItem{UID: s.UID, Error: "no auth"}
				return
			}
			_ = h.cfg.Upstream.DailyCheckin(a)
			remain, err := h.cfg.Upstream.UserResource(a)
			if err != nil {
				results[i] = resItem{UID: s.UID, Error: err.Error()}
				return
			}
			h.cfg.Pool.ReenableIfCredits(s.UID, remain)
			results[i] = resItem{UID: s.UID, Success: true, Remain: remain}
		}(i, s)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
	})
}
