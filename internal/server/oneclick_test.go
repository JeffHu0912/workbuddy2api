// oneclick_test.go POST /admin/api/oneclick 的契约测试（M3）。
//
// 覆盖：禁用账号 skipped、单账号失败不中断整批、12153 会话失效标记、uids 子集
// 过滤、非法 action 400、空池空结果、summary 计数。测试用 fake pool + fake
// upstream（httptest），不打真上游。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// fastOneclick 关闭账号间 150ms 节流，避免测试白等。
func fastOneclick(t *testing.T) {
	t.Helper()
	old := oneclickAccountDelay
	oneclickAccountDelay = 0
	t.Cleanup(func() { oneclickAccountDelay = old })
}

// ocUpResp 单 token 的签到响应（status 0 = 200）。
type ocUpResp struct {
	status int
	body   string
}

// ocUp fake 签到上游：按 access token 返回差异化响应。
type ocUp struct {
	respond        map[string]ocUpResp // token → 签到响应
	defaultResp    ocUpResp            // 未命中 token 的兜底
	resourceRemain int64
}

func (u *ocUp) serve() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			resp := u.defaultResp
			if u.respond != nil {
				if r2, ok := u.respond[tok]; ok {
					resp = r2
				}
			}
			if resp.status != 0 {
				w.WriteHeader(resp.status)
			}
			w.Write([]byte(resp.body))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			fmt.Fprintf(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":%d,"CycleCapacityUsed":0}]}}}}`, u.resourceRemain)
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
}

// newOneclickEnv 构造共享同一 Pool 的 fake 调度器（checkin 走 fake upstream）。
func newOneclickEnv(t *testing.T, u *ocUp, accounts ...*auth.Auth) (*pool.Pool, *scheduler.Scheduler) {
	t.Helper()
	srv := httptest.NewServer(u.serve())
	t.Cleanup(srv.Close)
	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return p, scheduler.New(scheduler.Config{Pool: p, Upstream: up})
}

// ocAcct 构造带有效 token（far-future 过期，不触发预刷新）的测试账号。
func ocAcct(uid, tok string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: tok, RefreshToken: "rt-" + uid, ExpiresAt: 9999999999}
}

// oneclickResp 响应体解码（独立声明，不跟实现结构体耦合字段名）。
type oneclickResp struct {
	Summary struct {
		Total   int `json:"total"`
		OK      int `json:"ok"`
		Failed  int `json:"failed"`
		Skipped int `json:"skipped"`
	} `json:"summary"`
	Results []struct {
		UID         string `json:"uid"`
		Nickname    string `json:"nickname"`
		OK          bool   `json:"ok"`
		Message     string `json:"message"`
		Skipped     bool   `json:"skipped"`
		SessionDead bool   `json:"session_dead"`
		Error       string `json:"error"`
	} `json:"results"`
}

func doOneclick(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/api/oneclick", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

func decodeOneclick(t *testing.T, rec *httptest.ResponseRecorder) oneclickResp {
	t.Helper()
	var out oneclickResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode oneclick response: %v body=%s", err, rec.Body)
	}
	return out
}

var ocOK = ocUpResp{body: `{"code":0,"msg":"ok","data":{}}`}

// TestOneclickDisabledAccountSkipped 禁用账号 skipped=true&ok=true，不跑调度器。
func TestOneclickDisabledAccountSkipped(t *testing.T) {
	fastOneclick(t)
	p := testPoolWith(&auth.Auth{UID: "u1"})
	p.Disable("u1", "test")
	h := NewHandler(Config{Pool: p}) // Scheduler nil：disabled 短路不会触达

	rec := doOneclick(t, h, `{"actions":["checkin"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	out := decodeOneclick(t, rec)
	if len(out.Results) != 1 {
		t.Fatalf("results=%d want 1", len(out.Results))
	}
	r := out.Results[0]
	if !r.Skipped || !r.OK {
		t.Fatalf("disabled 账号应 skipped=true ok=true, got %+v", r)
	}
	if out.Summary.Skipped != 1 || out.Summary.OK != 0 || out.Summary.Failed != 0 {
		t.Errorf("summary=%+v want skipped=1", out.Summary)
	}
}

// TestOneclickSingleAccountFailureDoesNotAbort 单账号失败不中断整批（全部账号都被处理）。
func TestOneclickSingleAccountFailureDoesNotAbort(t *testing.T) {
	fastOneclick(t)
	u := &ocUp{
		defaultResp: ocUpResp{status: 500, body: `{"code":500,"msg":"boom"}`},
		respond: map[string]ocUpResp{
			"tok-u1": ocOK,
			"tok-u2": {status: 500, body: `{"code":500,"msg":"boom"}`},
		},
		resourceRemain: 100,
	}
	p, sch := newOneclickEnv(t, u, ocAcct("u1", "tok-u1"), ocAcct("u2", "tok-u2"))
	h := NewHandler(Config{Pool: p, Scheduler: sch})

	rec := doOneclick(t, h, `{"actions":["checkin"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("整批应恒 200, got %d body=%s", rec.Code, rec.Body)
	}
	out := decodeOneclick(t, rec)
	if len(out.Results) != 2 {
		t.Fatalf("results=%d want 2（失败不应中断整批）", len(out.Results))
	}
	byUID := map[string]bool{}
	var errUID string
	for _, r := range out.Results {
		if r.OK {
			byUID[r.UID] = true
		} else {
			errUID = r.UID
			if r.Error == "" {
				t.Errorf("%s 失败账号 error 应非空", r.UID)
			}
		}
	}
	if !byUID["u1"] {
		t.Errorf("u1 应 ok=true, got %+v", out.Results)
	}
	if errUID != "u2" {
		t.Errorf("u2 应 ok=false, got errUID=%q results=%+v", errUID, out.Results)
	}
	if out.Summary.OK != 1 || out.Summary.Failed != 1 {
		t.Errorf("summary=%+v want ok=1 failed=1", out.Summary)
	}
}

// TestOneclickSessionDead12153 上游 12153 → session_dead=true 且 ok=false。
func TestOneclickSessionDead12153(t *testing.T) {
	fastOneclick(t)
	u := &ocUp{
		defaultResp:    ocUpResp{status: 401, body: `{"code":12153,"msg":"Offline user session not found"}`},
		resourceRemain: 100,
	}
	p, sch := newOneclickEnv(t, u, ocAcct("u1", "tok-u1"))
	h := NewHandler(Config{Pool: p, Scheduler: sch})

	rec := doOneclick(t, h, `{"actions":["checkin"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	out := decodeOneclick(t, rec)
	if len(out.Results) != 1 {
		t.Fatalf("results=%d", len(out.Results))
	}
	r := out.Results[0]
	if !r.SessionDead || r.OK {
		t.Fatalf("12153 应 session_dead=true ok=false, got %+v", r)
	}
	if !strings.Contains(r.Error, "12153") {
		t.Errorf("error=%q 应含 12153", r.Error)
	}
	if out.Summary.Failed != 1 {
		t.Errorf("summary failed=%d want 1", out.Summary.Failed)
	}
}

// TestOneclickUIDsSubset 非空 uids 只跑子集。
func TestOneclickUIDsSubset(t *testing.T) {
	fastOneclick(t)
	u := &ocUp{defaultResp: ocOK, resourceRemain: 100}
	p, sch := newOneclickEnv(t, u, ocAcct("u1", "tok-u1"), ocAcct("u2", "tok-u2"), ocAcct("u3", "tok-u3"))
	h := NewHandler(Config{Pool: p, Scheduler: sch})

	rec := doOneclick(t, h, `{"actions":["checkin"],"uids":["u1","u2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	out := decodeOneclick(t, rec)
	if len(out.Results) != 2 {
		t.Fatalf("results=%d want 2（只跑 u1,u2）", len(out.Results))
	}
	seen := map[string]bool{}
	for _, r := range out.Results {
		seen[r.UID] = true
	}
	if !seen["u1"] || !seen["u2"] || seen["u3"] {
		t.Errorf("uids 过滤错: %v", seen)
	}
}

// TestOneclickMissingUIDSkipped 不存在的 uid → skipped（整批 200）。
func TestOneclickMissingUIDSkipped(t *testing.T) {
	fastOneclick(t)
	u := &ocUp{defaultResp: ocOK, resourceRemain: 100}
	p, sch := newOneclickEnv(t, u, ocAcct("u1", "tok-u1"))
	h := NewHandler(Config{Pool: p, Scheduler: sch})

	rec := doOneclick(t, h, `{"actions":["checkin"],"uids":["nonexistent"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	out := decodeOneclick(t, rec)
	if len(out.Results) != 1 {
		t.Fatalf("results=%d want 1（缺失 uid 记 skipped）", len(out.Results))
	}
	r := out.Results[0]
	if r.UID != "nonexistent" || !r.Skipped || !r.OK {
		t.Errorf("缺失 uid 应 skipped=true ok=true, got %+v", r)
	}
}

// TestOneclickInvalidAction400 非法 action → 400。
func TestOneclickInvalidAction400(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p})

	rec := doOneclick(t, h, `{"actions":["bogus"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "invalid_request")
}

// TestOneclickEmptyPool 空池空结果（results=[]，不报错）。
func TestOneclickEmptyPool(t *testing.T) {
	fastOneclick(t)
	p := testPoolWith() // 空池
	h := NewHandler(Config{Pool: p})

	rec := doOneclick(t, h, `{"actions":["checkin"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	out := decodeOneclick(t, rec)
	if out.Results == nil || len(out.Results) != 0 {
		t.Fatalf("results 应为空数组, got %#v", out.Results)
	}
	if out.Summary.Total != 0 || out.Summary.OK != 0 || out.Summary.Failed != 0 || out.Summary.Skipped != 0 {
		t.Errorf("summary 应全 0, got %+v", out.Summary)
	}
}

// TestOneclickSummaryCounts summary 计数正确（ok/failed/skipped 三类混排）。
func TestOneclickSummaryCounts(t *testing.T) {
	fastOneclick(t)
	u := &ocUp{
		defaultResp:    ocOK,
		respond:        map[string]ocUpResp{"tok-u3": {status: 500, body: `{"code":500,"msg":"boom"}`}},
		resourceRemain: 100,
	}
	// u1 ok、u2 ok、u3 fail、u4 disabled
	p, sch := newOneclickEnv(t, u,
		ocAcct("u1", "tok-u1"), ocAcct("u2", "tok-u2"),
		ocAcct("u3", "tok-u3"), ocAcct("u4", "tok-u4"))
	p.Disable("u4", "test")
	h := NewHandler(Config{Pool: p, Scheduler: sch})

	rec := doOneclick(t, h, `{"actions":["checkin"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	out := decodeOneclick(t, rec)
	if out.Summary.Total != 4 || out.Summary.OK != 2 || out.Summary.Failed != 1 || out.Summary.Skipped != 1 {
		t.Errorf("summary=%+v want total=4 ok=2 failed=1 skipped=1", out.Summary)
	}
}
