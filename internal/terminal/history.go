package terminal

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// 命令历史默认值与硬上限（协议面在 README 中明示）。
const (
	// DefaultHistoryEntries 是每会话保留的历史条数。
	DefaultHistoryEntries = 200
	// MaxHistoryEntries 是每会话历史条数的硬上限。
	MaxHistoryEntries = 1000
	// DefaultHistoryEntryBytes 是单条历史记录的字节上限（超出截断并标记）。
	DefaultHistoryEntryBytes = 4096
	// MaxHistoryEntryBytes 是单条记录的硬上限。
	MaxHistoryEntryBytes = 64 << 10
	// DefaultHistorySessions 是保留历史文件的会话数上限（超出按时间淘汰最旧）。
	DefaultHistorySessions = 50
	// MaxHistorySessions 是会话数上限的硬上限。
	MaxHistorySessions = 500
	// DefaultHistoryDebounce 是两次落盘之间的最小间隔（合并连续击键的写入）。
	DefaultHistoryDebounce = 250 * time.Millisecond
	// MaxHistoryQueryLimit 是单次查询返回条数的上限。
	MaxHistoryQueryLimit = 200
	// DefaultHistoryQueryLimit 是查询未给 limit 时的返回条数。
	DefaultHistoryQueryLimit = 50
)

// sessionIDPattern 是会话 id 的形状；历史文件名只能由它派生，杜绝路径穿越。
var sessionIDPattern = regexp.MustCompile(`^t-[0-9a-f]{16}$`)

// HistoryOptions 是命令历史存储配置；Dir 为空或 Disabled 时功能停用（不落盘）。
type HistoryOptions struct {
	// Dir 是历史目录（插件状态目录下的子目录），为空表示停用。
	Dir string
	// MaxEntries 是每会话保留条数，默认 200、硬上限 1000。
	MaxEntries int
	// MaxEntryBytes 是单条记录的字节上限，默认 4 KiB、硬上限 64 KiB。
	MaxEntryBytes int
	// MaxSessions 是保留历史文件的会话数上限，默认 50、硬上限 500。
	MaxSessions int
	// Disabled 显式停用历史（KORE_TERMINAL_HISTORY=off）。
	Disabled bool
	// Debounce 是两次落盘之间的最小间隔，0 取默认值；测试可设 1ns 强制每次落盘。
	Debounce time.Duration
}

// HistoryEntry 是一条命令历史：只记录 write 的输入内容，按会话分组。
type HistoryEntry struct {
	// SessionID 是产生该输入的会话。
	SessionID string
	// ClientID 是会话创建者标识。
	ClientID string
	// Seq 是会话内单调递增序号（从 1 开始）。
	Seq uint64
	// At 是记录时间。
	At time.Time
	// Data 是该次 write 的输入字节（可能已按单条上限截断）。
	Data []byte
	// OriginalBytes 是截断前的原始字节数。
	OriginalBytes int
	// Truncated 表示该条已被单条上限截断。
	Truncated bool
}

// History 是命令历史的进程内视图 + 落盘器；历史故障不得影响终端读写（best-effort 落盘）。
type History struct {
	opts     HistoryOptions
	now      func() time.Time
	mu       sync.Mutex
	sessions map[string]*historySession
	lastErr  error
	scanned  bool
}

// historySession 是单会话历史的内存状态。
type historySession struct {
	sessionID string
	clientID  string
	createdAt time.Time
	updatedAt time.Time
	entries   []HistoryEntry
	lastSeq   uint64
	persisted bool
	dirty     bool
	lastWrite time.Time
}

// newHistory 构造历史存储；Dir 为空或 Disabled 返回停用实例（Enabled()=false）。
func newHistory(opts HistoryOptions) *History {
	opts = normalizeHistoryOptions(opts)
	return &History{
		opts:     opts,
		now:      time.Now,
		sessions: make(map[string]*historySession),
	}
}

// normalizeHistoryOptions 归一化历史配置。
func normalizeHistoryOptions(opts HistoryOptions) HistoryOptions {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultHistoryEntries
	}
	if opts.MaxEntries > MaxHistoryEntries {
		opts.MaxEntries = MaxHistoryEntries
	}
	if opts.MaxEntryBytes <= 0 {
		opts.MaxEntryBytes = DefaultHistoryEntryBytes
	}
	if opts.MaxEntryBytes > MaxHistoryEntryBytes {
		opts.MaxEntryBytes = MaxHistoryEntryBytes
	}
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = DefaultHistorySessions
	}
	if opts.MaxSessions > MaxHistorySessions {
		opts.MaxSessions = MaxHistorySessions
	}
	if opts.Debounce <= 0 {
		opts.Debounce = DefaultHistoryDebounce
	}
	return opts
}

// Enabled 返回是否启用历史（目录为空或显式停用时为 false）。
func (h *History) Enabled() bool {
	return h != nil && !h.opts.Disabled && strings.TrimSpace(h.opts.Dir) != ""
}

// Options 返回归一化后的配置（只读）。
func (h *History) Options() HistoryOptions {
	if h == nil {
		return HistoryOptions{}
	}
	return h.opts
}

// Status 返回落盘目录与最近一次错误，供 history 查询如实披露（不为空即说明历史可能不全）。
func (h *History) Status() (dir string, lastErr string) {
	if h == nil {
		return "", ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastErr != nil {
		lastErr = h.lastErr.Error()
	}
	return h.opts.Dir, lastErr
}

// Append 记录一次 write 的输入内容；按会话分组、按单条上限截断、按条数上限淘汰最旧。
//
// 返回错误只表示"没落住盘"（调用方不得据此拒绝终端输入），错误同时记录到 Status。
func (h *History) Append(sessionID, clientID string, data []byte, at time.Time) error {
	if !h.Enabled() || len(data) == 0 {
		return nil
	}
	if err := validateHistorySessionID(sessionID); err != nil {
		return h.fail(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	session, err := h.sessionLocked(sessionID)
	if err != nil {
		return h.failLocked(err)
	}
	entry := h.newEntry(sessionID, clientID, data, at)
	entry.Seq = session.lastSeq + 1
	if session.clientID != "" && session.clientID != clientID {
		return h.failLocked(fmt.Errorf("%w: 历史归属 %q 与写入方 %q 不一致",
			ErrForbidden, session.clientID, clientID))
	}
	session.clientID = clientID
	session.entries = append(session.entries, entry)
	if len(session.entries) > h.opts.MaxEntries {
		session.entries = append(session.entries[:0], session.entries[len(session.entries)-h.opts.MaxEntries:]...)
	}
	session.lastSeq = entry.Seq
	session.updatedAt = at
	session.dirty = true

	if err := h.persistDueLocked(session, at); err != nil {
		return h.failLocked(err)
	}
	return nil
}

// newEntry 生成一条记录并按单条上限截断。
func (h *History) newEntry(sessionID, clientID string, data []byte, at time.Time) HistoryEntry {
	original := len(data)
	truncated := false
	if original > h.opts.MaxEntryBytes {
		data = data[:h.opts.MaxEntryBytes]
		truncated = true
	}
	return HistoryEntry{
		SessionID: sessionID, ClientID: clientID, At: at,
		Data: append([]byte(nil), data...), OriginalBytes: original, Truncated: truncated,
	}
}

// Query 查询历史：sessionID 为空时返回该调用方全部会话的历史（按时间倒序）。
func (h *History) Query(clientID, sessionID string, limit int) ([]HistoryEntry, error) {
	if !h.Enabled() {
		return nil, nil
	}
	if limit <= 0 {
		limit = DefaultHistoryQueryLimit
	}
	if limit > MaxHistoryQueryLimit {
		limit = MaxHistoryQueryLimit
	}
	if sessionID != "" {
		if err := validateHistorySessionID(sessionID); err != nil {
			return nil, err
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	targets, err := h.targetsLocked(sessionID)
	if err != nil {
		return nil, h.failLocked(err)
	}
	entries := make([]HistoryEntry, 0, limit)
	for _, session := range targets {
		if sessionID == "" && session.clientID != clientID {
			continue
		}
		if sessionID != "" && session.clientID != "" && session.clientID != clientID {
			return nil, fmt.Errorf("%w: 历史属于 %q", ErrForbidden, session.clientID)
		}
		entries = append(entries, session.entries...)
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].At.Equal(entries[j].At) {
			return entries[i].At.After(entries[j].At)
		}
		if entries[i].SessionID != entries[j].SessionID {
			return entries[i].SessionID < entries[j].SessionID
		}
		return entries[i].Seq > entries[j].Seq
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// targetsLocked 解析查询目标：指定会话只取它，否则扫描历史目录加载全部会话。
func (h *History) targetsLocked(sessionID string) ([]*historySession, error) {
	if sessionID != "" {
		session, err := h.sessionLocked(sessionID)
		if err != nil {
			return nil, err
		}
		return []*historySession{session}, nil
	}
	if err := h.scanLocked(); err != nil {
		return nil, err
	}
	targets := make([]*historySession, 0, len(h.sessions))
	for _, session := range h.sessions {
		targets = append(targets, session)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].sessionID < targets[j].sessionID })
	return targets, nil
}

// Clear 清理历史：sessionID 为空时清理该调用方全部会话，返回清理的会话数与条目数。
func (h *History) Clear(clientID, sessionID string) (int, int, error) {
	if !h.Enabled() {
		return 0, 0, nil
	}
	if sessionID != "" {
		if err := validateHistorySessionID(sessionID); err != nil {
			return 0, 0, err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	targets, err := h.targetsLocked(sessionID)
	if err != nil {
		return 0, 0, h.failLocked(err)
	}
	sessions, entries := 0, 0
	for _, session := range targets {
		if session.clientID != clientID {
			if sessionID != "" && session.clientID != "" {
				return 0, 0, fmt.Errorf("%w: 历史属于 %q", ErrForbidden, session.clientID)
			}
			continue
		}
		entries += len(session.entries)
		delete(h.sessions, session.sessionID)
		sessions++
		if err := h.removeFile(session.sessionID); err != nil {
			return sessions, entries, h.failLocked(err)
		}
	}
	return sessions, entries, nil
}

// Flush 强制把全部脏会话落盘（关闭会话 / 插件退出前调用）。
func (h *History) Flush() error {
	if !h.Enabled() {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var firstErr error
	for _, session := range h.sessions {
		if !session.dirty {
			continue
		}
		if err := h.persistLocked(session); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return h.failLocked(firstErr)
	}
	return nil
}

// Close 停用历史：先 Flush 再释放（幂等，可在 defer 中重复调用）。
func (h *History) Close() error {
	return h.Flush()
}

// sessionLocked 取会话状态（首次访问时从磁盘加载），调用方持有 h.mu。
func (h *History) sessionLocked(sessionID string) (*historySession, error) {
	if session, ok := h.sessions[sessionID]; ok {
		return session, nil
	}
	session := &historySession{sessionID: sessionID, createdAt: h.now()}
	if err := h.loadLocked(session); err != nil {
		return nil, err
	}
	h.sessions[sessionID] = session
	return session, nil
}

// persistDueLocked 按 debounce 决定是否立即落盘；连续击键时合并写。
func (h *History) persistDueLocked(session *historySession, at time.Time) error {
	if h.opts.Debounce > 0 && !session.lastWrite.IsZero() && at.Sub(session.lastWrite) < h.opts.Debounce {
		return nil
	}
	return h.persistLocked(session)
}

// persistLocked 原子落盘单个会话（同目录临时文件 0600 → fsync → rename）。
func (h *History) persistLocked(session *historySession) error {
	if err := h.ensureDir(); err != nil {
		return err
	}
	data, err := h.encodeSession(session)
	if err != nil {
		return err
	}
	fresh := !session.persisted
	if err := h.writeFile(session.sessionID, data); err != nil {
		return err
	}
	session.dirty = false
	session.persisted = true
	session.lastWrite = h.now()
	if fresh {
		// 新会话文件才做保留策略：目录条目数有界，避免每次写都扫目录。
		if err := h.pruneLocked(); err != nil {
			return err
		}
	}
	return nil
}

// encodeSession 把会话状态编码为落盘 schema。
func (h *History) encodeSession(session *historySession) ([]byte, error) {
	doc := historySessionDoc{
		Version:   historyFileVersion,
		SessionID: session.sessionID,
		ClientID:  session.clientID,
		CreatedAt: formatHistoryTime(session.createdAt),
		UpdatedAt: formatHistoryTime(session.updatedAt),
		Entries:   make([]historyEntryDoc, 0, len(session.entries)),
	}
	for _, entry := range session.entries {
		doc.Entries = append(doc.Entries, historyEntryDoc{
			Seq:           entry.Seq,
			At:            formatHistoryTime(entry.At),
			Data:          base64.StdEncoding.EncodeToString(entry.Data),
			OriginalBytes: entry.OriginalBytes,
			Truncated:     entry.Truncated,
		})
	}
	return encodeHistoryDoc(doc)
}

// fail 记录错误并返回；失败不阻断终端输入，只在 Status 中如实暴露。
func (h *History) fail(err error) error {
	if h == nil {
		return err
	}
	h.mu.Lock()
	h.lastErr = err
	h.mu.Unlock()
	return err
}

// failLocked 在持锁路径记录错误。
func (h *History) failLocked(err error) error {
	h.lastErr = err
	return err
}

// validateHistorySessionID 校验会话 id 形状（文件名只由它派生）。
func validateHistorySessionID(sessionID string) error {
	if !sessionIDPattern.MatchString(sessionID) {
		return fmt.Errorf("%w: 会话 id 形状非法: %q", ErrInvalid, sessionID)
	}
	return nil
}
