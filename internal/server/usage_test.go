package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// readUsageLines 读取 usage.jsonl 并逐行解析为 []usageRecord（任一行非法 JSON 即失败）。
func readUsageLines(t *testing.T, path string) []usageRecord {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []usageRecord
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		var rec usageRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("usage line %d not valid JSON: %q (%v)", len(out)+1, sc.Text(), err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

// TestUsageLogFormat 验证单条落盘的 JSON 行格式与字段对齐（验收：落盘格式单测）。
func TestUsageLogFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	SetUsageLogPath(path)
	t.Cleanup(CloseUsageLog)

	recordUsageLog(&chatStat{
		model:  "cn:deep-model",
		mode:   "stream",
		uid:    "1e911f3a-dead-beef-cafe",
		ttfb:   7399 * time.Millisecond,
		toks:   56,
		status: 200,

		hasUsage:  true,
		prompt:    1234,
		cacheHit:  1000,
		cacheMiss: 234,
		cacheWr:   0,

		credit:    5.25,
		hasCredit: true,
	}, 15700*time.Millisecond)

	CloseUsageLog() // 排空队列 → flush → 关文件（同步落盘后读文件）

	recs := readUsageLines(t, path)
	if len(recs) != 1 {
		t.Fatalf("lines = %d, want 1", len(recs))
	}
	rec := recs[0]
	want := usageRecord{
		Model:      "cn:deep-model",
		UID8:       "1e911f3a",
		Credit:     5.25,
		Prompt:     1234,
		Completion: 56,
		CacheHit:   1000,
		CacheMiss:  234,
		CacheWrite: 0,
		TTFBMs:     7399,
		LatencyMs:  15700,
		Streaming:  true,
		Success:    true,
	}
	if rec.TS == "" {
		t.Error("ts empty")
	}
	if _, err := time.Parse(time.RFC3339, rec.TS); err != nil {
		t.Errorf("ts %q not RFC3339: %v", rec.TS, err)
	}
	want.TS = rec.TS
	if rec != want {
		t.Errorf("record = %+v, want %+v", rec, want)
	}
	// 供验收展示的真实样例行（含本机时区偏移）。
	if raw, err := os.ReadFile(path); err == nil {
		t.Logf("sample line: %s", bytes.TrimSpace(raw))
	}
}

// TestUsageLogDefaults 失败/无 usage/无 uid 请求：哨兵归零、uid8 留空、success=false。
func TestUsageLogDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	SetUsageLogPath(path)
	t.Cleanup(CloseUsageLog)

	recordUsageLog(&chatStat{model: "m1", mode: "sync", uid: "", status: 429, toks: -1}, 1*time.Second)
	CloseUsageLog()

	recs := readUsageLines(t, path)
	if len(recs) != 1 {
		t.Fatalf("lines = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.UID8 != "" {
		t.Errorf("uid8 = %q, want empty（无 uid 留空）", rec.UID8)
	}
	if rec.Success || rec.Streaming {
		t.Errorf("success=%v streaming=%v, want false/false（429 非流式）", rec.Success, rec.Streaming)
	}
	if rec.Completion != 0 || rec.Prompt != 0 || rec.Credit != 0 {
		t.Errorf("无 usage 观测字段应为 0，got prompt=%d completion=%d credit=%g",
			rec.Prompt, rec.Completion, rec.Credit)
	}
	if rec.LatencyMs != 1000 {
		t.Errorf("latency_ms = %d, want 1000", rec.LatencyMs)
	}
}

// TestUsageLogConcurrent 多 goroutine 并发写同一文件：行数不丢不串（channel 序列化）。
func TestUsageLogConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	SetUsageLogPath(path)
	t.Cleanup(CloseUsageLog)

	const goroutines, perG = 20, 50 // 1000 条 < channel 容量 1024，无丢弃干扰
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				recordUsageLog(&chatStat{
					model: "cn:concurrent", mode: "stream", uid: "abcdef12-3456",
					status: 200, hasUsage: true, prompt: 1, toks: 1,
					hasCredit: true, credit: 0.01,
				}, time.Second)
			}
		}()
	}
	wg.Wait()
	CloseUsageLog()

	recs := readUsageLines(t, path)
	if len(recs) != goroutines*perG {
		t.Errorf("lines = %d, want %d（并发下不得丢行/串行）", len(recs), goroutines*perG)
	}
	for i, rec := range recs {
		if rec.Model != "cn:concurrent" || rec.UID8 != "abcdef12" || !rec.Success {
			t.Fatalf("line %d 内容异常: %+v", i, rec)
		}
	}
}

// TestUsageLogDisabledByDefault 未接线时 recordUsageLog 是 no-op（测试/纯内存形态零污染）。
func TestUsageLogDisabledByDefault(t *testing.T) {
	CloseUsageLog() // 隔离其他用例可能残留的接线
	recordUsageLog(&chatStat{model: "m", mode: "sync", status: 200}, time.Second)
	// 无 panic 即通过：未接线时写路径整体短路。
}

// TestUsageLogOpenFailureTolerated 目录不存在时 open 失败仅 WARN，请求路径不受影响。
func TestUsageLogOpenFailureTolerated(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-dir", "usage.jsonl")
	SetUsageLogPath(missing)
	t.Cleanup(CloseUsageLog)

	recordUsageLog(&chatStat{model: "m", mode: "sync", status: 200}, time.Second)
	CloseUsageLog() // 排空 + flush 也不得 panic

	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("open 失败路径不应产出文件，stat err = %v", err)
	}
}
