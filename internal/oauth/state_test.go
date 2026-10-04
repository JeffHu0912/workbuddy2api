package oauth

import (
	"errors"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// okStart 返回固定 state/url 的 StartFunc。
func okStart() StartFunc {
	return func(r upstream.Region) (string, string, error) {
		return "state-" + string(r), "https://example.com/authorize", nil
	}
}

// TestPollWaitingToSuccess waiting → success：pollFn 返回账号则转成功终态并记忆 auth。
func TestPollWaitingToSuccess(t *testing.T) {
	var pollCalls int
	m := New(okStart(), func(r upstream.Region, s string) (*auth.Auth, error) {
		pollCalls++
		return &auth.Auth{AccessToken: "at", UID: "user123456", Nickname: "n1"}, nil
	})
	id, url, err := m.Start("cn")
	if err != nil || id == "" || url == "" {
		t.Fatalf("start: id=%q url=%q err=%v", id, url, err)
	}
	flow, err := m.Poll(id)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if flow.Status != StatusSuccess {
		t.Fatalf("status=%q want success", flow.Status)
	}
	if flow.Auth == nil || flow.Auth.UID != "user123456" || flow.Auth.Nickname != "n1" {
		t.Fatalf("auth=%+v want uid/nickname", flow.Auth)
	}
	if pollCalls != 1 {
		t.Fatalf("pollCalls=%d want 1", pollCalls)
	}
}

// TestPollWaitingToError waiting → error：pollFn 返回 error 则转错误终态。
func TestPollWaitingToError(t *testing.T) {
	m := New(okStart(), func(r upstream.Region, s string) (*auth.Auth, error) {
		return nil, errors.New("上游 5xx")
	})
	id, _, err := m.Start("global")
	if err != nil {
		t.Fatal(err)
	}
	flow, err := m.Poll(id)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if flow.Status != StatusError {
		t.Fatalf("status=%q want error", flow.Status)
	}
	if flow.Message == "" {
		t.Fatalf("error flow should carry message")
	}
}

// TestPollWaitingToTimeout waiting → timeout：超 TTL 转 timeout 终态（不再调 pollFn）。
func TestPollWaitingToTimeout(t *testing.T) {
	var pollCalls int
	now := time.Unix(1000, 0)
	m := New(okStart(), func(r upstream.Region, s string) (*auth.Auth, error) {
		pollCalls++
		return nil, nil
	})
	m.SetNow(func() time.Time { return now })
	id, _, _ := m.Start("cn")
	now = now.Add(oauthFlowTTL + time.Second)
	flow, err := m.Poll(id)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if flow.Status != StatusTimeout {
		t.Fatalf("status=%q want timeout", flow.Status)
	}
	if pollCalls != 0 {
		t.Fatalf("timeout should not call upstream, calls=%d", pollCalls)
	}
}

// TestStartRegionRouting 区域路由：region 大小写归一（cn/global），非法值报错。
func TestStartRegionRouting(t *testing.T) {
	var got []upstream.Region
	m := New(func(r upstream.Region) (string, string, error) {
		got = append(got, r)
		return "s", "u", nil
	}, func(r upstream.Region, s string) (*auth.Auth, error) { return nil, nil })

	for _, in := range []string{"cn", "global", "CN", " Global "} {
		if _, _, err := m.Start(in); err != nil {
			t.Fatalf("Start(%q) err=%v", in, err)
		}
	}
	want := []upstream.Region{upstream.RegionCN, upstream.RegionGlobal, upstream.RegionCN, upstream.RegionGlobal}
	if len(got) != len(want) {
		t.Fatalf("routed regions=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("region[%d]=%q want %q", i, got[i], want[i])
		}
	}
	if _, _, err := m.Start("bogus"); err == nil {
		t.Fatalf("Start(bogus) want error")
	}
}

// TestPollSoftErrorStaysWaiting 11217 归一：upstream.PollLogin 把 *Error(Status∈(0,500))
// 归一为 (nil,nil)（含 HTTP 200 + code=11217 token not ready）；状态机对 (nil,nil)
// 保持 waiting 且下次继续轮询（非终态）。
func TestPollSoftErrorStaysWaiting(t *testing.T) {
	var pollCalls int
	m := New(okStart(), func(r upstream.Region, s string) (*auth.Auth, error) {
		pollCalls++
		return nil, nil // 模拟 11217 token not ready 归一结果
	})
	id, _, _ := m.Start("cn")
	flow, err := m.Poll(id)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if flow.Status != StatusWaiting {
		t.Fatalf("status=%q want waiting", flow.Status)
	}
	if _, err := m.Poll(id); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if pollCalls != 2 {
		t.Fatalf("waiting should re-poll, calls=%d", pollCalls)
	}
}

// TestPollReadOnlyGuard 只读守卫：上游成功但只读模式开启时返回 ErrReadOnly，流程保持
// waiting（未消费）；关闭只读后可再 poll 成功。
func TestPollReadOnlyGuard(t *testing.T) {
	m := New(okStart(), func(r upstream.Region, s string) (*auth.Auth, error) {
		return &auth.Auth{AccessToken: "at", UID: "user123456"}, nil
	})
	m.SetReadOnly(true)
	id, _, _ := m.Start("cn")
	if _, err := m.Poll(id); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("poll err=%v want ErrReadOnly", err)
	}
	// 关闭只读 → 流程仍 waiting，可成功。
	m.SetReadOnly(false)
	flow, err := m.Poll(id)
	if err != nil {
		t.Fatalf("poll after unreadonly: %v", err)
	}
	if flow.Status != StatusSuccess {
		t.Fatalf("status=%q want success", flow.Status)
	}
}

// TestPollUnknownID 未知 id：返回 ErrUnknownID（上层与 timeout 归一为同一文案）。
func TestPollUnknownID(t *testing.T) {
	m := New(okStart(), func(r upstream.Region, s string) (*auth.Auth, error) { return nil, nil })
	if _, err := m.Poll("nonexistent"); !errors.Is(err, ErrUnknownID) {
		t.Fatalf("err=%v want ErrUnknownID", err)
	}
}

// TestPollTerminalIdempotent 终态幂等：命中终态直接返回记忆值，不再调上游。
func TestPollTerminalIdempotent(t *testing.T) {
	var pollCalls int
	m := New(okStart(), func(r upstream.Region, s string) (*auth.Auth, error) {
		pollCalls++
		return &auth.Auth{AccessToken: "at", UID: "user123456"}, nil
	})
	id, _, _ := m.Start("cn")
	f1, err := m.Poll(id)
	if err != nil || f1.Status != StatusSuccess {
		t.Fatalf("first poll status=%q err=%v", f1.Status, err)
	}
	f2, err := m.Poll(id)
	if err != nil || f2.Status != StatusSuccess {
		t.Fatalf("second poll status=%q err=%v", f2.Status, err)
	}
	if f2.Auth == nil || f2.Auth.UID != "user123456" {
		t.Fatalf("terminal memory value lost: %+v", f2.Auth)
	}
	if pollCalls != 1 {
		t.Fatalf("terminal should not re-poll, calls=%d", pollCalls)
	}
}

// TestPollTimeoutBoundary 超时边界：now < Created+ttl 仍 waiting；now >= Created+ttl 转 timeout。
func TestPollTimeoutBoundary(t *testing.T) {
	var pollCalls int
	now := time.Unix(1000, 0)
	m := New(okStart(), func(r upstream.Region, s string) (*auth.Auth, error) {
		pollCalls++
		return nil, nil
	})
	m.SetNow(func() time.Time { return now })
	id, _, _ := m.Start("cn")

	now = now.Add(oauthFlowTTL - time.Nanosecond)
	flow, err := m.Poll(id)
	if err != nil || flow.Status != StatusWaiting {
		t.Fatalf("at ttl-1ns status=%q err=%v want waiting", flow.Status, err)
	}
	if pollCalls != 1 {
		t.Fatalf("pollCalls=%d want 1", pollCalls)
	}

	now = now.Add(time.Nanosecond) // now == Created + ttl
	flow, err = m.Poll(id)
	if err != nil || flow.Status != StatusTimeout {
		t.Fatalf("at ttl status=%q err=%v want timeout", flow.Status, err)
	}
	if pollCalls != 1 {
		t.Fatalf("timeout should not re-poll, calls=%d", pollCalls)
	}
}
