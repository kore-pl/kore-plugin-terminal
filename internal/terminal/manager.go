//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package terminal

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Manager 管理一组 PTY 会话；零后台 goroutine（除每个会话唯一的 cmd.Wait 等待者）。
type Manager struct {
	mu       sync.Mutex
	opts     Options
	sessions map[string]*Session
	fault    Fault
	now      func() time.Time
	open     func() (ptyPair, error)
	ids      func() (string, error)
}

// New 创建会话管理器；Options 零值取默认值。
func New(opts Options) *Manager {
	opts = normalizeOptions(opts)
	return &Manager{
		opts:     opts,
		sessions: make(map[string]*Session),
		now:      time.Now,
		open:     openPTY,
		ids:      newSessionID,
	}
}

// InjectFault 注入测试故障（生产路径不使用）；FaultNone 清除。
func (m *Manager) InjectFault(fault Fault) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fault = fault
}

// Create 起一个登录 shell 的 PTY 会话；先顺带回收空闲会话，再检查会话上限。
func (m *Manager) Create(ctx context.Context, shell string, cols, rows int) (Info, error) {
	if err := ctxError(ctx); err != nil {
		return Info{}, err
	}
	if err := validateSize(cols, rows); err != nil {
		return Info{}, err
	}
	shellPath, err := resolveShell(shell, m.opts.Shell)
	if err != nil {
		return Info{}, err
	}

	m.mu.Lock()
	if ferr := faultError(m.fault, true); ferr != nil {
		m.mu.Unlock()
		return Info{}, ferr
	}
	expired := m.sweepLocked()
	if len(m.sessions) >= m.opts.MaxSessions {
		m.mu.Unlock()
		closeSessions(expired)
		return Info{}, ErrRateLimit
	}
	id, err := m.ids()
	if err != nil {
		m.mu.Unlock()
		closeSessions(expired)
		return Info{}, fmt.Errorf("%w: 生成会话 id: %v", ErrIO, err)
	}
	pair, err := m.open()
	if err != nil {
		m.mu.Unlock()
		closeSessions(expired)
		if errors.Is(err, ErrUnsupported) {
			return Info{}, ErrUnsupported
		}
		return Info{}, fmt.Errorf("%w: 打开 PTY: %v", ErrIO, err)
	}
	session, err := startSession(id, shellPath, pair, cols, rows, m.now())
	if err != nil {
		m.mu.Unlock()
		closeSessions(expired)
		return Info{}, err
	}
	m.sessions[id] = session
	info := session.info()
	m.mu.Unlock()

	closeSessions(expired)
	return info, nil
}

// Read 增量读取会话输出；无数据返回空 Data（不是错误），maxBytes<=0 取默认块大小。
func (m *Manager) Read(ctx context.Context, id string, maxBytes int) (ReadResult, error) {
	if err := ctxError(ctx); err != nil {
		return ReadResult{}, err
	}
	if maxBytes <= 0 {
		maxBytes = m.opts.ReadChunk
	}
	if maxBytes > MaxReadBytes {
		maxBytes = MaxReadBytes
	}
	session, err := m.session(id)
	if err != nil {
		return ReadResult{}, err
	}
	return session.read(maxBytes)
}

// Write 把 data 写入会话 stdin；返回实际写入字节数。
func (m *Manager) Write(ctx context.Context, id string, data []byte) (int, error) {
	if err := ctxError(ctx); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, nil
	}
	if len(data) > MaxWriteBytes {
		return 0, fmt.Errorf("%w: 单次写入上限 %d 字节", ErrInvalid, MaxWriteBytes)
	}
	session, err := m.session(id)
	if err != nil {
		return 0, err
	}
	return session.write(data)
}

// Resize 调整会话窗口尺寸（TIOCSWINSZ）。
func (m *Manager) Resize(ctx context.Context, id string, cols, rows int) error {
	if err := ctxError(ctx); err != nil {
		return err
	}
	if err := validateSize(cols, rows); err != nil {
		return err
	}
	session, err := m.session(id)
	if err != nil {
		return err
	}
	return session.resize(cols, rows)
}

// Close 关闭会话：kill(-pgid) → 等进程退出 → 关 fd → 从 map 移除。
// 清理操作不受注入故障阻断，保证任何状态下都能回收进程。
func (m *Manager) Close(ctx context.Context, id string) error {
	if err := ctxError(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	expired := m.sweepLocked()
	session, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	closeSessions(expired)
	if !ok {
		return ErrNotFound
	}
	session.Close()
	return nil
}

// List 返回当前会话快照（按创建时间排序），不触发空闲回收。
func (m *Manager) List() []Info {
	m.mu.Lock()
	list := make([]Info, 0, len(m.sessions))
	for _, session := range m.sessions {
		list = append(list, session.info())
	}
	m.mu.Unlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedAt != list[j].CreatedAt {
			return list[i].CreatedAt < list[j].CreatedAt
		}
		return list[i].SessionID < list[j].SessionID
	})
	return list
}

// CloseAll 关闭全部会话（插件退出 / kore.shutdown 时调用）。
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for id, session := range m.sessions {
		delete(m.sessions, id)
		all = append(all, session)
	}
	m.mu.Unlock()
	closeSessions(all)
}

// session 查找并触碰会话；调用前先做故障与超时检查。
func (m *Manager) session(id string) (*Session, error) {
	m.mu.Lock()
	if ferr := faultError(m.fault, false); ferr != nil {
		m.mu.Unlock()
		return nil, ferr
	}
	expired := m.sweepLocked()
	session, ok := m.sessions[id]
	if ok {
		session.markActive(m.now())
	}
	m.mu.Unlock()
	closeSessions(expired)
	if !ok {
		return nil, ErrNotFound
	}
	return session, nil
}

// sweepLocked 从 map 摘除空闲超时会话并返回；调用方负责在锁外关闭。
// 这是 G-19 的落点：没有 ticker，清理只搭 create/write/read 的顺风车。
func (m *Manager) sweepLocked() []*Session {
	if m.opts.IdleTimeout <= 0 {
		return nil
	}
	now := m.now()
	var expired []*Session
	for id, session := range m.sessions {
		if now.Sub(session.lastActiveTime()) > m.opts.IdleTimeout {
			delete(m.sessions, id)
			expired = append(expired, session)
		}
	}
	return expired
}

// closeSessions 逐个关闭会话，忽略单个关闭错误（已退出/kill 失败的进程由 Close 兜底）。
func closeSessions(sessions []*Session) {
	for _, session := range sessions {
		session.Close()
	}
}
