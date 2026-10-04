// logbuf.go 管理面环形日志缓冲：容量 500 行，写满覆盖最旧行；token/cookie/password/
// secret/authorization 在写入前脱敏，/admin/api/logs 经 snapshot 读取最近 N 行。
package server

import (
	"regexp"
	"sync"
)

// logBufferCap 环形缓冲容量。
const logBufferCap = 500

// LogBuffer 环形日志缓冲，线程安全。
type LogBuffer struct {
	mu     sync.Mutex
	buf    []string
	head   int // 下一个写入位置
	filled bool
}

// NewLogBuffer 构建容量 500 的环形日志缓冲。
func NewLogBuffer() *LogBuffer {
	return &LogBuffer{buf: make([]string, logBufferCap)}
}

// Write 追加一行（写前脱敏），写满覆盖最旧行。
func (l *LogBuffer) Write(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf[l.head] = redactLog(line)
	l.head++
	if l.head == len(l.buf) {
		l.head = 0
		l.filled = true
	}
}

// snapshot 返回最近 limit 行（旧→新）。limit<=0 或超界返回全部。
func (l *LogBuffer) snapshot(limit int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.snapshotLocked()
	if limit > 0 && limit < len(out) {
		out = out[len(out)-limit:]
	}
	return out
}

// snapshotLocked 返回按时间顺序（旧→新）排列的全部已写行；调用方须持有 l.mu。
func (l *LogBuffer) snapshotLocked() []string {
	if !l.filled {
		return append([]string(nil), l.buf[:l.head]...)
	}
	out := make([]string, 0, len(l.buf))
	for i := 0; i < len(l.buf); i++ {
		out = append(out, l.buf[(l.head+i)%len(l.buf)])
	}
	return out
}

// bearerRe 匹配 "Bearer <token>"（Authorization 头里的凭证形态）。
var bearerRe = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;]+`)

// secretKeyRe 匹配敏感键后跟 `:`/`=` 和值（token/cookie/password/secret/authorization
// 及 access_token/refresh_token 等变体）。
var secretKeyRe = regexp.MustCompile(`(?i)\b([A-Za-z0-9_-]*(?:token|cookie|password|secret|authorization)[A-Za-z0-9_-]*)\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;]+)`)

// redactLog 脱敏日志行中的敏感值（token/cookie/password/secret/authorization）。
// 先遮 Bearer 后的 token，再遮敏感键后的值；返回值绝不含原始敏感串。
func redactLog(line string) string {
	line = bearerRe.ReplaceAllString(line, "Bearer [REDACTED]")
	line = secretKeyRe.ReplaceAllString(line, "${1}=[REDACTED]")
	return line
}
