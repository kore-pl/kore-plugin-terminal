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
//
// 目标回收是惰性的（G-19：无 ticker）：create / read / write / resize / close /
// attach / list 每次调用顺带摘除空闲超时会话并在锁外关闭。
type Manager struct {
	mu       sync.Mutex
	opts     Options
	sessions map[string]*Session
	history  *History
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
		history:  newHistory(opts.History),
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

// Create 起一个登录 shell 的 PTY 会话并绑定创建者；先顺带回收空闲会话，再检查会话上限。
func (m *Manager) Create(ctx context.Context, clientID, shell string, cols, rows int) (Info, error) {
	if err := ctxError(ctx); err != nil {
		return Info{}, err
	}
	if err := validateSize(cols, rows); err != nil {
		return Info{}, err
	}
	owner, err := normalizeClientID(clientID)
	if err != nil {
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
		m.flushHistory()
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
	session, err := startSession(id, owner, shellPath, pair, cols, rows, m.opts.ReplayBytes, m.now())
	if err != nil {
		m.mu.Unlock()
		closeSessions(expired)
		return Info{}, err
	}
	m.sessions[id] = session
	info := session.info()
	m.mu.Unlock()

	m.finishSweep(expired)
	return info, nil
}

// Read 增量读取会话输出；cursor 为客户端已消费的绝对偏移（nil 表示只取新输出）。
func (m *Manager) Read(ctx context.Context, clientID, id string, cursor *uint64, maxBytes int) (ReadResult, error) {
	if err := ctxError(ctx); err != nil {
		return ReadResult{}, err
	}
	if maxBytes <= 0 {
		maxBytes = m.opts.ReadChunk
	}
	if maxBytes > MaxReadBytes {
		maxBytes = MaxReadBytes
	}
	owner, err := normalizeClientID(clientID)
	if err != nil {
		return ReadResult{}, err
	}
	session, err := m.session(owner, id)
	if err != nil {
		return ReadResult{}, err
	}
	return session.read(cursor, maxBytes)
}

// Write 把 data 写入会话 stdin；返回实际写入字节数，并把已写入的输入记入会话历史。
func (m *Manager) Write(ctx context.Context, clientID, id string, data []byte) (int, error) {
	if err := ctxError(ctx); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, nil
	}
	if len(data) > MaxWriteBytes {
		return 0, fmt.Errorf("%w: 单次写入上限 %d 字节", ErrInvalid, MaxWriteBytes)
	}
	owner, err := normalizeClientID(clientID)
	if err != nil {
		return 0, err
	}
	session, err := m.session(owner, id)
	if err != nil {
		return 0, err
	}
	written, werr := session.write(data)
	if written > 0 {
		// 历史是 best-effort：失败不能影响终端输入，错误记录在 History.Status 中。
		_ = m.history.Append(id, owner, data[:written], m.now())
	}
	return written, werr
}

// Resize 调整会话窗口尺寸（TIOCSWINSZ）。
func (m *Manager) Resize(ctx context.Context, clientID, id string, cols, rows int) error {
	if err := ctxError(ctx); err != nil {
		return err
	}
	if err := validateSize(cols, rows); err != nil {
		return err
	}
	owner, err := normalizeClientID(clientID)
	if err != nil {
		return err
	}
	session, err := m.session(owner, id)
	if err != nil {
		return err
	}
	return session.resize(cols, rows)
}

// Attach 重新挂接存活会话：校验归属与读游标，返回快照与可补读窗口。
func (m *Manager) Attach(ctx context.Context, clientID, id string, cursor *uint64) (Attached, error) {
	if err := ctxError(ctx); err != nil {
		return Attached{}, err
	}
	owner, err := normalizeClientID(clientID)
	if err != nil {
		return Attached{}, err
	}
	session, err := m.session(owner, id)
	if err != nil {
		return Attached{}, err
	}
	if cursor != nil {
		offset, windowStart := session.cursorState()
		if *cursor > offset {
			return Attached{}, fmt.Errorf("%w: 读游标 %d 超过当前偏移 %d", ErrInvalid, *cursor, offset)
		}
		if *cursor < windowStart {
			return Attached{}, fmt.Errorf("%w: 游标 %d 早于可补读下界 %d（当前偏移 %d）",
				ErrCursorExpired, *cursor, windowStart, offset)
		}
	}
	attached := session.attach(m.now())
	lastActive, perr := time.Parse(time.RFC3339Nano, attached.Info.LastActive)
	if perr == nil {
		attached.IdleExpiresAt = lastActive.Add(m.opts.IdleTimeout).Format(time.RFC3339Nano)
	}
	return attached, nil
}

// Close 关闭会话：校验归属 → 移出 map → kill(-pgid) + 关 fd，并落盘该会话历史。
func (m *Manager) Close(ctx context.Context, clientID, id string) error {
	if err := ctxError(ctx); err != nil {
		return err
	}
	owner, err := normalizeClientID(clientID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	expired := m.sweepLocked()
	session, ok := m.sessions[id]
	if ok {
		if oerr := session.checkOwner(owner); oerr != nil {
			m.mu.Unlock()
			m.finishSweep(expired)
			return oerr
		}
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	m.finishSweep(expired)
	if !ok {
		return ErrNotFound
	}
	session.Close()
	m.flushHistory()
	return nil
}

// List 返回会话快照（按创建时间排序）；clientID 为空表示不过滤（管理视图），
// 非空时只返回该调用方创建的会话。
func (m *Manager) List(clientID string) []Info {
	m.mu.Lock()
	expired := m.sweepLocked()
	list := make([]Info, 0, len(m.sessions))
	for _, session := range m.sessions {
		if clientID != "" && session.owner != clientID {
			continue
		}
		list = append(list, session.info())
	}
	m.mu.Unlock()
	m.finishSweep(expired)
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedAt != list[j].CreatedAt {
			return list[i].CreatedAt < list[j].CreatedAt
		}
		return list[i].SessionID < list[j].SessionID
	})
	return list
}

// History 返回命令历史存储（只读句柄）。
func (m *Manager) History() *History {
	return m.history
}

// finishSweep 在锁外关闭被回收会话，并在确有回收时落盘历史。
// 无回收时不 Flush：否则每次方法调用都会绕过历史写入的 debounce（击键级 write 会被放大）。
func (m *Manager) finishSweep(expired []*Session) {
	closeSessions(expired)
	if len(expired) > 0 {
		m.flushHistory()
	}
}

// flushHistory 把脏历史落盘（best-effort：失败只体现在 History.Status）。
func (m *Manager) flushHistory() {
	_ = m.history.Flush()
}

// CloseAll 关闭全部会话（插件退出 / kore.shutdown 时调用），并落盘命令历史。
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for id, session := range m.sessions {
		delete(m.sessions, id)
		all = append(all, session)
	}
	m.mu.Unlock()
	closeSessions(all)
	_ = m.history.Close()
}

// session 查找并触碰会话；先做故障/空闲回收，再校验归属（越权 attach 的落点）。
func (m *Manager) session(owner, id string) (*Session, error) {
	m.mu.Lock()
	if ferr := faultError(m.fault, false); ferr != nil {
		m.mu.Unlock()
		return nil, ferr
	}
	expired := m.sweepLocked()
	session, ok := m.sessions[id]
	if ok {
		if oerr := session.checkOwner(owner); oerr != nil {
			m.mu.Unlock()
			m.finishSweep(expired)
			return nil, oerr
		}
		session.markActive(m.now())
	}
	m.mu.Unlock()
	m.finishSweep(expired)
	if !ok {
		return nil, ErrNotFound
	}
	return session, nil
}

// sweepLocked 从 map 摘除空闲超时会话并返回；调用方负责在锁外关闭。
// 这是 G-19 的落点：没有 ticker，清理只搭方法调用的顺风车。
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
