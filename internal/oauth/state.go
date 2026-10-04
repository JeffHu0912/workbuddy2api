// Package oauth 实现管理面 OAuth 设备授权的内存状态机：
//
//	waiting → success | error | timeout（三终态）
//
// 终态幂等：命中终态直接返回记忆值，不再调上游。TTL 用可注入 now func() time.Time 时间源，
// 便于测试超时边界与超时归一。上游调用以 StartFunc/PollFunc 注入（生产接 upstream.StartLogin/
// PollLogin），保持本包零网络依赖、纯可测。
package oauth

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// Flow 状态常量。
const (
	StatusWaiting = "waiting"
	StatusSuccess = "success"
	StatusError   = "error"
	StatusTimeout = "timeout"
)

// oauthFlowTTL 授权流程总时长上限。
const oauthFlowTTL = 5 * time.Minute

// 哨兵错误。
var (
	// ErrUnknownID id 不存在（防枚举：上层与 timeout 归一为同一文案）。
	ErrUnknownID = errors.New("oauth flow not found")
	// ErrReadOnly 只读守卫：上游已成功但服务端禁止落盘。
	ErrReadOnly = errors.New("server is read-only")
)

// StartFunc 申请设备授权，返回 state 与授权 URL。
type StartFunc func(region upstream.Region) (state, url string, err error)

// PollFunc 轮询一次登录结果：(nil,nil)=waiting；(auth,nil)=成功；(nil,err)=失败。
type PollFunc func(region upstream.Region, state string) (*auth.Auth, error)

// Flow 单个授权流程（终态记忆值存于 Auth / Message）。
type Flow struct {
	Region  upstream.Region `json:"region"`
	State   string          `json:"state"`
	URL     string          `json:"url"`
	Status  string          `json:"status"`
	Created time.Time       `json:"created"`
	// Auth 成功终态的记忆值；仅 Status==success 时非 nil。不参与序列化。
	Auth *auth.Auth `json:"-"`
	// Message 失败/超时终态的文案。
	Message string `json:"message,omitempty"`
}

// Manager 授权流程内存状态机。并发安全。
type Manager struct {
	mu       sync.Mutex
	flows    map[string]*Flow
	now      func() time.Time
	ttl      time.Duration
	readonly bool
	startFn  StartFunc
	pollFn   PollFunc
}

// New 构建状态机；startFn/pollFn 为上游委托（生产接 upstream.Client 的对应方法）。
func New(startFn StartFunc, pollFn PollFunc) *Manager {
	return &Manager{
		flows:   map[string]*Flow{},
		now:     time.Now,
		ttl:     oauthFlowTTL,
		startFn: startFn,
		pollFn:  pollFn,
	}
}

// SetNow 注入时间源（测试用）。
func (m *Manager) SetNow(fn func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = fn
}

// SetTTL 注入 TTL（测试用）。
func (m *Manager) SetTTL(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ttl = d
}

// SetReadOnly 注入只读守卫（生产恒 false；测试用）。
func (m *Manager) SetReadOnly(ro bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readonly = ro
}

// Start 规范化 region 后向上游申请授权，创建 waiting 流程，返回 id + 授权 URL。
// id = now.UnixNano() 十进制串。
func (m *Manager) Start(region string) (id, url string, err error) {
	r, err := upstream.NormalizeRegion(region)
	if err != nil {
		return "", "", err
	}
	state, url, err := m.startFn(r)
	if err != nil {
		return "", "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	id = fmt.Sprintf("%d", now.UnixNano())
	m.flows[id] = &Flow{Region: r, State: state, URL: url, Status: StatusWaiting, Created: now}
	return id, url, nil
}

// Poll 推进流程到下一状态并返回其值快照。
//
//   - id 不存在 → (zero, ErrUnknownID)
//   - 命中终态 → 直接返回记忆值（幂等，不调上游）
//   - waiting 且超时 → 转 timeout 终态
//   - waiting 未超时 → 调 pollFn；(nil,nil) 保持 waiting，(auth,nil) 转 success，
//     (nil,err) 转 error；只读守卫下成功转 ErrReadOnly（流程保持 waiting）
func (m *Manager) Poll(id string) (Flow, error) {
	m.mu.Lock()
	flow, ok := m.flows[id]
	if !ok {
		m.mu.Unlock()
		return Flow{}, ErrUnknownID
	}
	// 终态幂等：命中终态直接返回记忆值。
	if flow.Status == StatusSuccess || flow.Status == StatusError || flow.Status == StatusTimeout {
		snap := *flow
		m.mu.Unlock()
		return snap, nil
	}
	now := m.now()
	if !now.Before(flow.Created.Add(m.ttl)) {
		// 超时边界：now >= Created+ttl → timeout 终态。
		flow.Status = StatusTimeout
		flow.Message = "流程已失效，请重新发起"
		snap := *flow
		m.mu.Unlock()
		return snap, nil
	}
	region := flow.Region
	state := flow.State
	m.mu.Unlock()

	a, err := m.pollFn(region, state)

	m.mu.Lock()
	defer m.mu.Unlock()
	// 竞态复查：pollFn 期间另一 goroutine 可能已推进到终态。
	if flow.Status == StatusSuccess || flow.Status == StatusError || flow.Status == StatusTimeout {
		return *flow, nil
	}
	if err != nil {
		flow.Status = StatusError
		flow.Message = err.Error()
		return *flow, nil
	}
	if a == nil {
		return *flow, nil // 仍 waiting
	}
	if m.readonly {
		return Flow{}, ErrReadOnly
	}
	flow.Status = StatusSuccess
	flow.Auth = a
	return *flow, nil
}
