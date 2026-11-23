package terminal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestHistory 构造历史存储：临时目录 + 立即落盘（Debounce=1ns），方便断言文件。
func newTestHistory(t *testing.T, opts HistoryOptions) *History {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.Debounce == 0 {
		opts.Debounce = time.Nanosecond
	}
	history := newHistory(opts)
	t.Cleanup(func() { _ = history.Close() })
	return history
}

// historySessionID 生成测试用会话 id（形状必须与生产一致）。
func historySessionID(seed byte) string {
	return "t-" + string(bytes.Repeat([]byte{seed}, 16))
}

// TestHistoryRoundTrip 覆盖 写入 → 查询 → base64 往返 → 倒序。
func TestHistoryRoundTrip(t *testing.T) {
	history := newTestHistory(t, HistoryOptions{})
	sessionID := historySessionID('a')
	inputs := []string{"echo hello\n", "ls -la\n", "printf 'x'\n"}
	base := time.Now().Add(-3 * time.Second)
	for i, input := range inputs {
		if err := history.Append(sessionID, "alice", []byte(input), base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("append 失败: %v", err)
		}
	}
	entries, err := history.Query("alice", sessionID, 0)
	if err != nil {
		t.Fatalf("query 失败: %v", err)
	}
	if len(entries) != len(inputs) {
		t.Fatalf("条目数=%d，期望 %d", len(entries), len(inputs))
	}
	for i, entry := range entries {
		want := inputs[len(inputs)-1-i] // 最新在前
		if string(entry.Data) != want {
			t.Fatalf("第 %d 条=%q，期望 %q", i, entry.Data, want)
		}
		if entry.SessionID != sessionID || entry.ClientID != "alice" || entry.Truncated {
			t.Fatalf("第 %d 条元数据不符: %+v", i, entry)
		}
		if entry.Seq != uint64(len(inputs)-i) {
			t.Fatalf("第 %d 条 seq=%d，期望 %d", i, entry.Seq, len(inputs)-i)
		}
	}
}

// TestHistoryGroupsBySession 覆盖按会话分组与跨会话聚合查询。
func TestHistoryGroupsBySession(t *testing.T) {
	history := newTestHistory(t, HistoryOptions{})
	first, second := historySessionID('a'), historySessionID('b')
	now := time.Now()
	if err := history.Append(first, "alice", []byte("one\n"), now); err != nil {
		t.Fatalf("append 失败: %v", err)
	}
	if err := history.Append(second, "alice", []byte("two\n"), now.Add(time.Second)); err != nil {
		t.Fatalf("append 失败: %v", err)
	}
	all, err := history.Query("alice", "", 0)
	if err != nil {
		t.Fatalf("聚合查询失败: %v", err)
	}
	if len(all) != 2 || all[0].SessionID != second || all[1].SessionID != first {
		t.Fatalf("聚合查询结果不符: %+v", all)
	}
	only, err := history.Query("alice", first, 0)
	if err != nil {
		t.Fatalf("单会话查询失败: %v", err)
	}
	if len(only) != 1 || string(only[0].Data) != "one\n" {
		t.Fatalf("单会话查询结果不符: %+v", only)
	}
}

// TestHistoryEntryTruncation 覆盖单条长度上限（截断标记 + 原始字节数）。
func TestHistoryEntryTruncation(t *testing.T) {
	history := newTestHistory(t, HistoryOptions{MaxEntryBytes: 8})
	sessionID := historySessionID('a')
	long := []byte("0123456789abcdef")
	if err := history.Append(sessionID, "alice", long, time.Now()); err != nil {
		t.Fatalf("append 失败: %v", err)
	}
	entries, err := history.Query("alice", sessionID, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("query 失败: entries=%d err=%v", len(entries), err)
	}
	if string(entries[0].Data) != string(long[:8]) || !entries[0].Truncated || entries[0].OriginalBytes != 16 {
		t.Fatalf("截断结果不符: %+v", entries[0])
	}
}

// TestHistoryEntriesCap 覆盖条数上限：超出后淘汰最旧并落盘一致。
func TestHistoryEntriesCap(t *testing.T) {
	history := newTestHistory(t, HistoryOptions{MaxEntries: 3})
	sessionID := historySessionID('a')
	now := time.Now()
	for i := 0; i < 5; i++ {
		if err := history.Append(sessionID, "alice", []byte{byte('0' + i)}, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("append #%d 失败: %v", i, err)
		}
	}
	entries, err := history.Query("alice", sessionID, 0)
	if err != nil {
		t.Fatalf("query 失败: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("条数=%d，期望 3", len(entries))
	}
	wantSeq := []uint64{5, 4, 3}
	for i, entry := range entries {
		if entry.Seq != wantSeq[i] {
			t.Fatalf("第 %d 条 seq=%d，期望 %d", i, entry.Seq, wantSeq[i])
		}
	}
	if err := history.Flush(); err != nil {
		t.Fatalf("flush 失败: %v", err)
	}
	reloaded := newHistory(HistoryOptions{Dir: history.Options().Dir})
	t.Cleanup(func() { _ = reloaded.Close() })
	persisted, err := reloaded.Query("alice", sessionID, 0)
	if err != nil || len(persisted) != 3 {
		t.Fatalf("落盘条数=%d err=%v，期望 3", len(persisted), err)
	}
}

// TestHistoryOwnerIsolation 断言历史按创建者隔离：跨调用方查询/清理一律拒绝。
func TestHistoryOwnerIsolation(t *testing.T) {
	history := newTestHistory(t, HistoryOptions{})
	sessionID := historySessionID('a')
	if err := history.Append(sessionID, "alice", []byte("secret\n"), time.Now()); err != nil {
		t.Fatalf("append 失败: %v", err)
	}
	if _, err := history.Query("bob", sessionID, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("跨调用方查询错误=%v，期望 ErrForbidden", err)
	}
	if entries, err := history.Query("bob", "", 0); err != nil || len(entries) != 0 {
		t.Fatalf("跨调用方聚合查询应返回空: entries=%d err=%v", len(entries), err)
	}
	if _, _, err := history.Clear("bob", sessionID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("跨调用方清理错误=%v，期望 ErrForbidden", err)
	}
}

// TestHistoryClear 覆盖历史清理（单会话 + 全部）。
func TestHistoryClear(t *testing.T) {
	history := newTestHistory(t, HistoryOptions{})
	first, second := historySessionID('a'), historySessionID('b')
	now := time.Now()
	for _, id := range []string{first, second} {
		if err := history.Append(id, "alice", []byte("x\n"), now); err != nil {
			t.Fatalf("append 失败: %v", err)
		}
	}
	if err := history.Flush(); err != nil {
		t.Fatalf("flush 失败: %v", err)
	}
	sessions, entries, err := history.Clear("alice", first)
	if err != nil || sessions != 1 || entries != 1 {
		t.Fatalf("清理单会话=%d/%d err=%v，期望 1/1", sessions, entries, err)
	}
	if _, err := os.Stat(history.historyPath(first)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("清理后文件仍存在: %v", err)
	}
	sessions, entries, err = history.Clear("alice", "")
	if err != nil || sessions != 1 || entries != 1 {
		t.Fatalf("清理全部=%d/%d err=%v，期望 1/1", sessions, entries, err)
	}
	if remaining, _ := history.Query("alice", "", 0); len(remaining) != 0 {
		t.Fatalf("清理后仍有 %d 条历史", len(remaining))
	}
}

// TestHistoryPersistence 断言重启（新实例）后仍能读到未清理的历史。
func TestHistoryPersistence(t *testing.T) {
	dir := t.TempDir()
	sessionID := historySessionID('a')
	first := newHistory(HistoryOptions{Dir: dir, Debounce: time.Nanosecond})
	if err := first.Append(sessionID, "alice", []byte("persisted\n"), time.Now()); err != nil {
		t.Fatalf("append 失败: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close 失败: %v", err)
	}
	second := newHistory(HistoryOptions{Dir: dir})
	t.Cleanup(func() { _ = second.Close() })
	entries, err := second.Query("alice", sessionID, 0)
	if err != nil || len(entries) != 1 || string(entries[0].Data) != "persisted\n" {
		t.Fatalf("重启后历史不符: entries=%+v err=%v", entries, err)
	}
}

// TestHistoryFilePermissions 断言目录 0700、历史文件 0600（历史含口令，必须私有）。
func TestHistoryFilePermissions(t *testing.T) {
	history := newTestHistory(t, HistoryOptions{})
	sessionID := historySessionID('a')
	if err := history.Append(sessionID, "alice", []byte("mysql -p***\n"), time.Now()); err != nil {
		t.Fatalf("append 失败: %v", err)
	}
	if err := history.Flush(); err != nil {
		t.Fatalf("flush 失败: %v", err)
	}
	dirInfo, err := os.Stat(history.Options().Dir)
	if err != nil {
		t.Fatalf("stat 目录失败: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("目录权限=%o，期望 700", perm)
	}
	fileInfo, err := os.Stat(history.historyPath(sessionID))
	if err != nil {
		t.Fatalf("stat 文件失败: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("文件权限=%o，期望 600", perm)
	}
}

// TestHistoryDisabled 断言未配置状态目录时功能停用且不报错（KORE_STATE_DIR 缺失）。
func TestHistoryDisabled(t *testing.T) {
	history := newHistory(HistoryOptions{})
	t.Cleanup(func() { _ = history.Close() })
	if history.Enabled() {
		t.Fatal("空目录应停用历史")
	}
	if err := history.Append(historySessionID('a'), "alice", []byte("x\n"), time.Now()); err != nil {
		t.Fatalf("停用状态下 append 不应报错: %v", err)
	}
	if entries, err := history.Query("alice", "", 0); err != nil || len(entries) != 0 {
		t.Fatalf("停用状态下查询应返回空: entries=%d err=%v", len(entries), err)
	}
	if sessions, entries, err := history.Clear("alice", ""); err != nil || sessions != 0 || entries != 0 {
		t.Fatalf("停用状态下清理=%d/%d err=%v", sessions, entries, err)
	}
}

// TestHistoryRetentionBySessions 断言会话数上限：超出后淘汰最旧的历史文件。
func TestHistoryRetentionBySessions(t *testing.T) {
	history := newTestHistory(t, HistoryOptions{MaxSessions: 2})
	old := time.Now().Add(-time.Hour)
	ids := []string{historySessionID('a'), historySessionID('b'), historySessionID('c')}
	for _, id := range ids {
		if err := history.Append(id, "alice", []byte("x\n"), time.Now()); err != nil {
			t.Fatalf("append 失败: %v", err)
		}
		if err := history.Flush(); err != nil {
			t.Fatalf("flush 失败: %v", err)
		}
		// 显式错开 mtime，保证淘汰顺序确定（按时间升序删最旧）。
		_ = os.Chtimes(history.historyPath(id), old, old)
		old = old.Add(time.Minute)
	}
	files, err := filepath.Glob(filepath.Join(history.Options().Dir, "session-*.json"))
	if err != nil {
		t.Fatalf("glob 失败: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("保留历史文件数=%d，期望 2（files=%v）", len(files), files)
	}
	if _, err := os.Stat(history.historyPath(ids[0])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("最旧会话历史未淘汰: %v", err)
	}
}

// TestHistoryRejectsBadSessionID 断言会话 id 形状非法时拒绝（文件名不允许路径穿越）。
func TestHistoryRejectsBadSessionID(t *testing.T) {
	history := newTestHistory(t, HistoryOptions{})
	for _, id := range []string{"", "../../etc/passwd", "t-XYZ", "t-1234"} {
		if err := history.Append(id, "alice", []byte("x\n"), time.Now()); !errors.Is(err, ErrInvalid) {
			t.Fatalf("会话 id=%q 错误=%v，期望 ErrInvalid", id, err)
		}
	}
}
