// oneclick_result.go 一键任务面板（M3）的结构化结果出口：把定时调度里
// 「只打日志」的单账号执行逻辑包装成 (status, detail) 结构化返回，供
// /admin/api/oneclick 逐账号聚合。status ∈ {ok, already, fail, skipped}
// 复用 CheckinStatus 语义；detail 为人类可读摘要（失败时含上游错误原文）。
//
// 约束：不破坏 RunActivityNow / RunTravelNow 现有签名——它们仍在定时排程里
// 用，本文件只新增并行的 *Result / *One 版本。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// CheckinOne 单账号立即签到（供 oneclick 面板聚合）。复用 CheckinAll 的单账号
// 逻辑（checkinOne），阻塞等待正在进行的全量签到结束（checkinMu 串行化）。
// 未在池中命中的 uid 返回 skipped(not found)。
func (s *Scheduler) CheckinOne(uid string) CheckinOutcome {
	s.checkinMu.Lock()
	defer s.checkinMu.Unlock()
	for _, st := range s.cfg.Pool.List() {
		if st.UID == uid {
			return s.checkinOne(st)
		}
	}
	return CheckinOutcome{UID: uid, Status: CheckinSkipped, Detail: "not found"}
}

// checkinOne 单账号签到逻辑（从 CheckinAll 循环体提取，语义逐字一致）：
// 禁用/无凭证/global 跳过 → 按需刷新 → daily-checkin → 查余额解冻。
func (s *Scheduler) checkinOne(st pool.Status) CheckinOutcome {
	oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
	if st.Disabled {
		oc.Status, oc.Detail = CheckinSkipped, "disabled"
		return oc
	}
	a := s.cfg.Pool.AuthByUID(st.UID)
	if a == nil || a.RefreshTokenValue() == "" {
		oc.Status, oc.Detail = CheckinSkipped, "no credentials"
		return oc
	}
	if a.IsGlobal() {
		oc.Status, oc.Detail = CheckinSkipped, "global"
		return oc
	}
	// 停机跨过 token 有效期（关机过夜/容器长期停跑）时先补一次刷新。
	if a.NeedsRefresh(checkinRefreshSkew) {
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("checkin %s refresh: %v", logfmt.Label(st.UID, st.Nickname), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: checkin %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			if a.NeedsRefresh(0) {
				oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
				return oc
			}
		} else {
			a.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
			if err := a.SaveAtomic(); err != nil {
				log.Printf("checkin %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
			}
		}
	}
	// 签到返回错误（含"今天已签到"）也继续查余额：余额恢复即可解冻账号。
	if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
		if upstream.IsAlreadyCheckin(err) {
			oc.Status = CheckinAlready
		} else {
			oc.Status = CheckinFail
			oc.Detail = err.Error()
			log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	} else {
		oc.Status = CheckinOK
	}
	remain, buckets, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
	if err != nil {
		log.Printf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
		oc.Status = CheckinFail
		oc.Detail = joinDetail(oc.Detail, "resource: "+err.Error())
		return oc
	}
	s.cfg.Pool.ReenableIfCredits(st.UID, remain)
	s.cfg.Pool.SetCreditsDetailed(st.UID, remain, buckets.Expiring)
	oc.Credits = &remain
	return oc
}

// ActivityOnceResult 单账号活跃上报（外部入口：oneclick 面板）。取背景 ctx。
// 返回 (status, detail)：status=ok 表示 N 条全发满；detail 含 "report n/N" 摘要。
func (s *Scheduler) ActivityOnceResult(a *auth.Auth) (status, detail string) {
	if a == nil || a.AccessTokenValue() == "" {
		return "skipped", "no access token"
	}
	return s.activityOne(context.Background(), a)
}

// activityOne 单账号活跃上报 + 连登自检 + 领猫 + 领奖闭环（从 runActivity
// 循环体提取，语义逐字一致），返回结构化结果。
func (s *Scheduler) activityOne(ctx context.Context, a *auth.Auth) (status, detail string) {
	count := s.cfg.ActivityReportCount
	cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
	ok := 0
	var lastErr error
	for i := 1; i <= count; i++ {
		rid := fmt.Sprintf("%s-r%d", cid, i)
		if err := s.cfg.Upstream.ReportChatActivity(a, cid, rid); err != nil {
			log.Printf("activity %s: report %d/%d: %v", logfmt.Label(a.UID, a.Nickname), i, count, err)
			lastErr = err
			break
		}
		log.Printf("activity %s: report %d/%d ok", logfmt.Label(a.UID, a.Nickname), i, count)
		ok++
		if i < count {
			if !sleepCtx(ctx, activityReportGap) {
				return "fail", fmt.Sprintf("report %d/%d (cancelled)", ok, count)
			}
		}
	}
	if ok < count {
		d := fmt.Sprintf("report %d/%d", ok, count)
		if lastErr != nil {
			d += ": " + lastErr.Error()
		}
		return "fail", d
	}
	suspicious := s.checkActivityStreak(a) // N 条全发满 → 回读 streak 自检
	s.travelAdoptForce(a)                  // 无猫账号对话量刚补满 → 立即重试领养
	s.claimGrowthRewards(a)                // 连登奖励 + 抽奖（finally 语义）
	d := fmt.Sprintf("report %d/%d", ok, count)
	if suspicious {
		d += ", streak check failed"
	}
	return "ok", d
}

// TravelOnceResult 单账号单趟旅行巡检（外部入口：oneclick 面板）。
// 返回 (status, detail)：status=ok 表示状态机推进成功（领养/派出/领奖/在途跳过）。
func (s *Scheduler) TravelOnceResult(a *auth.Auth) (status, detail string) {
	if a == nil || a.RefreshTokenValue() == "" {
		return "skipped", "no credentials"
	}
	if a.IsGlobal() {
		return "skipped", "global"
	}
	return s.travelOne(a)
}
