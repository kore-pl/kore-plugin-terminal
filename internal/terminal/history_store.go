package terminal

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// historyFileVersion 是历史落盘 schema 版本；版本不符按损坏处理（不猜测兼容）。
const historyFileVersion = 1

// historySessionDoc 是单会话历史的落盘 schema（对外不暴露，仅插件自用）。
type historySessionDoc struct {
	// Version 是 schema 版本。
	Version int `json:"version"`
	// SessionID 是会话标识，必须与文件名一致。
	SessionID string `json:"session_id"`
	// ClientID 是创建者标识（跨重启保留归属，用于历史查询的越权拒绝）。
	ClientID string `json:"client_id"`
	// CreatedAt 是会话创建时间。
	CreatedAt string `json:"created_at"`
	// UpdatedAt 是最近一条记录时间。
	UpdatedAt string `json:"updated_at"`
	// Entries 是历史条目（最旧在前）。
	Entries []historyEntryDoc `json:"entries"`
}

// historyEntryDoc 是单条历史的落盘 schema；input 统一 base64（可能含任意字节）。
type historyEntryDoc struct {
	// Seq 是会话内单调递增序号。
	Seq uint64 `json:"seq"`
	// At 是记录时间。
	At string `json:"at"`
	// Data 是输入字节的 base64。
	Data string `json:"data"`
	// OriginalBytes 是截断前字节数。
	OriginalBytes int `json:"original_bytes"`
	// Truncated 表示已按单条上限截断。
	Truncated bool `json:"truncated"`
}

// encodeHistoryDoc 序列化落盘 schema。
func encodeHistoryDoc(doc historySessionDoc) ([]byte, error) {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化命令历史: %w", err)
	}
	return append(data, '\n'), nil
}

// formatHistoryTime 统一时间格式（RFC 3339 带纳秒）。
func formatHistoryTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// parseHistoryTime 解析时间；空串返回零值。
func parseHistoryTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, raw)
}

// historyDir 返回历史目录（已去空白）。
func (h *History) historyDir() string {
	return strings.TrimSpace(h.opts.Dir)
}

// historyPath 由会话 id 派生历史文件路径（id 已按形状校验，杜绝路径穿越）。
func (h *History) historyPath(sessionID string) string {
	return filepath.Join(h.historyDir(), "session-"+sessionID+".json")
}

// ensureDir 创建历史目录并收紧为 0700（插件状态目录下的私有子目录）。
func (h *History) ensureDir() error {
	dir := h.historyDir()
	if dir == "" {
		return errors.New("命令历史目录为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建命令历史目录: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("收紧命令历史目录权限: %w", err)
	}
	return nil
}

// writeFile 原子写历史文件：同目录临时文件（0600）→ fsync → chmod → rename。
func (h *History) writeFile(sessionID string, data []byte) error {
	dir := h.historyDir()
	temp, err := os.CreateTemp(dir, ".session-*.tmp")
	if err != nil {
		return fmt.Errorf("创建命令历史临时文件: %w", err)
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("写入命令历史: %w", err)
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("同步命令历史: %w", err)
	}
	if err := temp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("设置命令历史权限: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("关闭命令历史临时文件: %w", err)
	}
	if err := os.Rename(tempPath, h.historyPath(sessionID)); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("落盘命令历史: %w", err)
	}
	return nil
}

// removeFile 删除单会话历史文件（不存在视为已清理）。
func (h *History) removeFile(sessionID string) error {
	if err := os.Remove(h.historyPath(sessionID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除命令历史: %w", err)
	}
	return nil
}

// loadLocked 从磁盘恢复单会话历史；文件不存在视为空历史。
func (h *History) loadLocked(session *historySession) error {
	path := h.historyPath(session.sessionID)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取命令历史: %w", err)
	}
	session.persisted = true
	var doc historySessionDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("解析命令历史 %s: %w", path, err)
	}
	if doc.Version != historyFileVersion {
		return fmt.Errorf("命令历史 %s 版本 %d 不受支持（期望 %d）", path, doc.Version, historyFileVersion)
	}
	if doc.SessionID != session.sessionID {
		return fmt.Errorf("命令历史 %s 的 session_id=%q 与文件名不符", path, doc.SessionID)
	}
	session.clientID = doc.ClientID
	session.createdAt, _ = parseHistoryTime(doc.CreatedAt)
	session.updatedAt, _ = parseHistoryTime(doc.UpdatedAt)
	for _, entry := range doc.Entries {
		raw, err := base64.StdEncoding.DecodeString(entry.Data)
		if err != nil {
			return fmt.Errorf("命令历史 %s 第 %d 条 data 不是合法 base64: %w", path, entry.Seq, err)
		}
		at, err := parseHistoryTime(entry.At)
		if err != nil {
			return fmt.Errorf("命令历史 %s 第 %d 条时间非法: %w", path, entry.Seq, err)
		}
		session.entries = append(session.entries, HistoryEntry{
			SessionID: session.sessionID, ClientID: session.clientID, Seq: entry.Seq, At: at,
			Data: raw, OriginalBytes: entry.OriginalBytes, Truncated: entry.Truncated,
		})
		session.lastSeq = entry.Seq
	}
	if !session.createdAt.IsZero() && session.lastWrite.IsZero() {
		session.lastWrite = session.updatedAt
	}
	return nil
}

// scanLocked 首次查询时扫描历史目录，把磁盘上的会话加载进内存（会话可能早已回收）。
func (h *History) scanLocked() error {
	if h.scanned {
		return nil
	}
	h.scanned = true
	entries, err := os.ReadDir(h.historyDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("扫描命令历史目录: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		sessionID := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "session-"), ".json")
		if validateHistorySessionID(sessionID) != nil {
			continue // 形状不符的文件不加载，也不删除
		}
		if _, ok := h.sessions[sessionID]; ok {
			continue
		}
		session := &historySession{sessionID: sessionID}
		if err := h.loadLocked(session); err != nil {
			return err
		}
		h.sessions[sessionID] = session
	}
	return nil
}

// pruneLocked 按会话数上限淘汰最旧的历史文件（按 mtime 升序删）。
func (h *History) pruneLocked() error {
	entries, err := os.ReadDir(h.historyDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("扫描命令历史目录: %w", err)
	}
	type fileInfo struct {
		name    string
		modTime time.Time
	}
	files := make([]fileInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{name: entry.Name(), modTime: info.ModTime()})
	}
	if len(files) <= h.opts.MaxSessions {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, file := range files[:len(files)-h.opts.MaxSessions] {
		if err := os.Remove(filepath.Join(h.historyDir(), file.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("淘汰旧命令历史 %s: %w", file.name, err)
		}
	}
	return nil
}
