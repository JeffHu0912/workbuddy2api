package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestLogBufferWriteRead 写读：按序返回全部已写行。
func TestLogBufferWriteRead(t *testing.T) {
	lb := NewLogBuffer()
	lb.Write("hello")
	lb.Write("world")
	got := lb.snapshot(0)
	if len(got) != 2 || got[0] != "hello" || got[1] != "world" {
		t.Fatalf("snapshot=%v want [hello world]", got)
	}
}

// TestLogBufferRingOverwrite 环形覆盖：写满后覆盖最旧行，容量恒 500。
func TestLogBufferRingOverwrite(t *testing.T) {
	lb := NewLogBuffer()
	for i := 0; i < logBufferCap+10; i++ {
		lb.Write(fmt.Sprintf("line-%d", i))
	}
	got := lb.snapshot(0)
	if len(got) != logBufferCap {
		t.Fatalf("len=%d want %d", len(got), logBufferCap)
	}
	if got[0] != fmt.Sprintf("line-%d", 10) {
		t.Fatalf("oldest=%q want line-10", got[0])
	}
	if got[len(got)-1] != fmt.Sprintf("line-%d", logBufferCap+9) {
		t.Fatalf("newest=%q want line-%d", got[len(got)-1], logBufferCap+9)
	}
}

// TestLogBufferLimit limit：snapshot(limit) 返回最近 limit 行（旧→新）。
func TestLogBufferLimit(t *testing.T) {
	lb := NewLogBuffer()
	for i := 0; i < 10; i++ {
		lb.Write(fmt.Sprintf("l%d", i))
	}
	got := lb.snapshot(3)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	if got[0] != "l7" || got[1] != "l8" || got[2] != "l9" {
		t.Fatalf("limit snapshot=%v want [l7 l8 l9]", got)
	}
}

// TestLogBufferConcurrent 并发写安全（-race 下无数据竞争），满后恒 500。
func TestLogBufferConcurrent(t *testing.T) {
	lb := NewLogBuffer()
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				lb.Write(fmt.Sprintf("g%d-%d", g, i))
			}
		}(g)
	}
	wg.Wait()
	got := lb.snapshot(0)
	if len(got) != logBufferCap {
		t.Fatalf("len=%d want %d", len(got), logBufferCap)
	}
}

// TestAdminLogsContract 契约形状：/admin/api/logs 返回 {"lines":[...]}。
func TestAdminLogsContract(t *testing.T) {
	lb := NewLogBuffer()
	lb.Write("a")
	lb.Write("b")
	h := NewHandler(Config{Log: lb, APIKey: "secret"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/logs?limit=1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	var body struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Lines) != 1 || body.Lines[0] != "b" {
		t.Fatalf("lines=%v want [b]", body.Lines)
	}
}

// TestAdminLogsRequiresAuth 401：未带 Authorization 拒绝。
func TestAdminLogsRequiresAuth(t *testing.T) {
	h := NewHandler(Config{Log: NewLogBuffer(), APIKey: "secret"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/logs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}

// TestRedactLog 脱敏：token/cookie/password/secret/authorization 及 Bearer 凭证值被遮。
func TestRedactLog(t *testing.T) {
	cases := []struct{ in, secret string }{
		{"token=abc123", "abc123"},
		{"password=secret", "secret"},
		{"cookie=session123", "session123"},
		{"secret=top", "top"},
		{"Authorization: Bearer xyz", "xyz"},
		{`accessToken="at-token"`, "at-token"},
		{`refresh_token='rt-token'`, "rt-token"},
	}
	for _, c := range cases {
		out := redactLog(c.in)
		if strings.Contains(out, c.secret) {
			t.Errorf("redactLog(%q)=%q leaked %q", c.in, out, c.secret)
		}
	}
	// Write 内部脱敏：敏感值不进入缓冲。
	lb := NewLogBuffer()
	lb.Write("oauth start token=leakme")
	got := lb.snapshot(0)
	if strings.Contains(got[0], "leakme") {
		t.Fatalf("buffer leaked sensitive value: %q", got[0])
	}
}
