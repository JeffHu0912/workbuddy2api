// oneclick.go 一键任务面板端点（M3）：POST /admin/api/oneclick 聚合「签到 +
// 活跃 + 旅行巡检 + 成长任务（shell-out）」，逐账号串行执行并返回四态结果表。
//
// 设计要点（对齐 WBCenter 批量操作）：
//   - 整批恒 200，单账号失败下沉到 results[].error（不禁用不中断整批）；
//   - 禁用/缺失账号 skipped=true&ok=true；
//   - 12153 会话失效标记 session_dead=true（ok=false，error 固定文案）；
//   - 账号间 150ms 节流，避免上游风控；
//   - growth 动作走独立 python3 shell-out（120s 超时，stderr 不 panic）。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// oneclickAccountDelay 账号间节流（对齐 WBCenter 批量操作 150ms）。测试可置 0。
var oneclickAccountDelay = 150 * time.Millisecond

// growthTaskTimeout 成长任务 shell-out 超时（对齐 WBCenter 长任务上限）。
const growthTaskTimeout = 120 * time.Second

// oneclickAllowedActions actions 白名单。
var oneclickAllowedActions = map[string]bool{
	"checkin":  true,
	"activity": true,
	"travel":   true,
	"growth":   true,
}

// oneclickRequest POST /admin/api/oneclick 请求体。
type oneclickRequest struct {
	Actions []string `json:"actions"`
	UIDs    []string `json:"uids"`
}

// stepResult 单动作结果（status ∈ ok/already/fail/skipped，detail 人类可读）。
type stepResult struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// oneclickAccountResult 单账号聚合结果。
type oneclickAccountResult struct {
	UID         string                `json:"uid"`
	Nickname    string                `json:"nickname"`
	OK          bool                  `json:"ok"`
	Message     string                `json:"message"`
	Steps       map[string]stepResult `json:"steps"`
	Skipped     bool                  `json:"skipped"`
	SessionDead bool                  `json:"session_dead"`
	Error       string                `json:"error"`
}

// oneclickSummary 整批计数（total = ok + failed + skipped）。
type oneclickSummary struct {
	Total   int `json:"total"`
	OK      int `json:"ok"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// oneclickResponse 整批响应。
type oneclickResponse struct {
	Summary oneclickSummary         `json:"summary"`
	Results []oneclickAccountResult `json:"results"`
}

// adminOneclick 一键任务：校验 actions 白名单 → 按 uids 过滤 → 逐账号串行跑
// actions → 聚合 summary。整批恒 200。
func (h *Handler) adminOneclick(w http.ResponseWriter, r *http.Request) {
	var req oneclickRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		// 空体 / 非法 JSON → actions 回落缺省（与缺省语义一致，不因体格式拒绝）。
		req.Actions = nil
	}

	actions := req.Actions
	if len(actions) == 0 {
		actions = []string{"checkin", "activity"}
	}
	for _, act := range actions {
		if !oneclickAllowedActions[act] {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid action: "+act)
			return
		}
	}

	// 目标账号序列：uids 空 = 全量；非空 = 只对请求子集（未命中池的 uid 记缺失）。
	all := h.cfg.Pool.List()
	byUID := make(map[string]pool.Status, len(all))
	for _, st := range all {
		byUID[st.UID] = st
	}
	type ocTarget struct {
		uid   string
		st    pool.Status
		found bool
	}
	var targets []ocTarget
	if len(req.UIDs) == 0 {
		for _, st := range all {
			targets = append(targets, ocTarget{uid: st.UID, st: st, found: true})
		}
	} else {
		for _, uid := range req.UIDs {
			if st, ok := byUID[uid]; ok {
				targets = append(targets, ocTarget{uid: uid, st: st, found: true})
			} else {
				targets = append(targets, ocTarget{uid: uid, found: false})
			}
		}
	}

	resp := oneclickResponse{Results: make([]oneclickAccountResult, 0, len(targets))}

	first := true
	for _, tg := range targets {
		if !first && oneclickAccountDelay > 0 {
			time.Sleep(oneclickAccountDelay)
		}
		first = false

		res := oneclickAccountResult{UID: tg.uid, Steps: map[string]stepResult{}}

		if !tg.found {
			res.OK = true
			res.Skipped = true
			res.Message = "账号不存在，已跳过"
			resp.Results = append(resp.Results, res)
			resp.Summary.Total++
			resp.Summary.Skipped++
			h.logf("oneclick %s: skipped (not found)", tg.uid)
			continue
		}
		res.Nickname = tg.st.Nickname

		if tg.st.Disabled {
			res.OK = true
			res.Skipped = true
			res.Message = "账号已禁用，已跳过"
			resp.Results = append(resp.Results, res)
			resp.Summary.Total++
			resp.Summary.Skipped++
			h.logf("oneclick %s: skipped (disabled)", res.Nickname)
			continue
		}

		a := h.cfg.Pool.AuthByUID(tg.uid)
		if a == nil {
			res.Error = "no auth file found"
			res.Message = "无凭证文件"
			resp.Results = append(resp.Results, res)
			resp.Summary.Total++
			resp.Summary.Failed++
			h.logf("oneclick %s: failed (no auth file)", tg.uid)
			continue
		}

		// 逐 action 执行，聚合单账号结果。
		res.OK = true
		var parts []string
		for _, act := range actions {
			sr := h.runOneAction(act, tg.uid, a)
			res.Steps[act] = sr
			if sr.Status == "fail" {
				res.OK = false
				if res.Error == "" {
					res.Error = sr.Detail
				}
				if isSessionDeadText(sr.Detail) {
					res.SessionDead = true
				}
			}
			parts = append(parts, actionShort(act, sr))
		}
		if res.SessionDead {
			res.OK = false
			res.Error = "上游会话失效（12153）"
			res.Message = res.Error
		} else {
			res.Message = strings.Join(parts, "+")
		}

		if res.OK {
			resp.Summary.OK++
		} else {
			resp.Summary.Failed++
		}
		resp.Summary.Total++
		resp.Results = append(resp.Results, res)
		h.logf("oneclick %s: ok=%v msg=%q", res.Nickname, res.OK, res.Message)
	}

	writeJSON(w, http.StatusOK, resp)
}

// runOneAction 执行单个动作，返回结构化结果。growth 走 shell-out（不依赖
// 调度器）；checkin/activity/travel 复用调度器（Scheduler 为 nil 时记 fail）。
func (h *Handler) runOneAction(act, uid string, a *auth.Auth) stepResult {
	if act == "growth" {
		status, detail := h.runGrowthTask(uid)
		return stepResult{Status: status, Detail: detail}
	}
	if h.cfg.Scheduler == nil {
		return stepResult{Status: "fail", Detail: "scheduler not configured"}
	}
	switch act {
	case "checkin":
		oc := h.cfg.Scheduler.CheckinOne(uid)
		return stepResult{Status: string(oc.Status), Detail: oc.Detail}
	case "activity":
		status, detail := h.cfg.Scheduler.ActivityOnceResult(a)
		return stepResult{Status: status, Detail: detail}
	case "travel":
		status, detail := h.cfg.Scheduler.TravelOnceResult(a)
		return stepResult{Status: status, Detail: detail}
	}
	return stepResult{Status: "fail", Detail: "unknown action: " + act}
}

// runGrowthTask 成长任务 shell-out：python3 scripts/task_runner.py <uid> --yes。
// 120s 超时；非零退出/超时捕获 stderr（不 panic），stdout 末行作摘要回填 detail。
func (h *Handler) runGrowthTask(uid string) (status, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), growthTaskTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "scripts/task_runner.py", uid, "--yes")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		d := strings.TrimSpace(stderr.String())
		if d == "" {
			d = err.Error()
		}
		return "fail", d
	}
	summary := lastNonEmptyLine(stdout.String())
	if summary == "" {
		summary = "growth task completed"
	}
	return "ok", summary
}

// lastNonEmptyLine 返回 s 的最后一条非空行（task_runner 汇总行
// "task_runner done: ..." 通常在最末）。
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// isSessionDeadText 判定结果详情是否指向 12153 会话失效（字符串层面，覆盖
// 上游 Error 原文与分类措辞）。
func isSessionDeadText(s string) bool {
	ls := strings.ToLower(s)
	return strings.Contains(ls, "12153") ||
		strings.Contains(ls, "session dead") ||
		strings.Contains(ls, "session not found") ||
		strings.Contains(ls, "session expired")
}

// actionShort 单动作结果 → 消息短摘要（用于 res.Message）。
func actionShort(act string, sr stepResult) string {
	switch act {
	case "checkin":
		switch sr.Status {
		case "ok":
			return "签到成功"
		case "already":
			return "签到已签"
		case "skipped":
			return "签到跳过"
		default:
			return "签到失败"
		}
	case "activity":
		if sr.Status == "ok" {
			return "活跃" + sr.Detail
		}
		return "活跃失败"
	case "travel":
		if sr.Status == "ok" {
			return "旅行" + sr.Detail
		}
		return "旅行失败"
	case "growth":
		if sr.Status == "ok" {
			return "成长" + sr.Detail
		}
		return "成长失败"
	default:
		return act + ":" + sr.Status
	}
}
