//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package fault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
)

// 本文件覆盖 G-20 审计指出的三个新方法（attach / history / history.clear）：
//   - attach：注入 kill / timeout / cgroup 后必须回对应哨兵错误，不得阻塞、不得留下死会话；
//   - attach：客户端游标超出可恢复窗口 / 超前必须是明确错误（E_CONFLICT / E_INVALID 的来源），
//     越权 attach 回 ErrForbidden；
//   - history：历史文件损坏时 Query 必须报错并保留原文件（不静默重置、不删除）；
//   - history.clear：历史目录不可写时必须报错，且磁盘文件原样保留（不能假报成功）。
// 断言的是哨兵错误语义 + 文件系统副作用 + 进程回收，不是「没 panic」。

// newHistoryManager 构造带命令历史目录的真实 PTY 管理器；t.Cleanup 关闭全部会话与历史。
func newHistoryManager(t *testing.T, dir string) *terminal.Manager {
	t.Helper()
	manager := terminal.New(terminal.Options{
		MaxSessions: 2,
		IdleTimeout: time.Minute,
		Shell:       "/bin/bash",
		History: terminal.HistoryOptions{
			Dir:      dir,
			Debounce: time.Nanosecond,
		},
	})
	t.Cleanup(manager.CloseAll)
	return manager
}

// createShell 创建一个会话并确保测试结束时进程被回收。
func createShell(t *testing.T, manager *terminal.Manager, clientID string) terminal.Info {
	t.Helper()
	info, err := manager.Create(context.Background(), clientID, "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	return info
}

// TestAttachInjectedFaults 断言四类注入故障下 attach 回对应哨兵错误且不阻塞。
func TestAttachInjectedFaults(t *testing.T) {
	manager := newManager(t, 2)
	info := createShell(t, manager, "web")
	ctx := context.Background()

	cases := []struct {
		name  string
		fault terminal.Fault
		want  error
	}{
		{"timeout", terminal.FaultTimeout, terminal.ErrTimeout},
		{"cgroup", terminal.FaultCgroup, terminal.ErrResource},
		{"killed", terminal.FaultKilled, terminal.ErrClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager.InjectFault(tc.fault)
			defer manager.InjectFault(terminal.FaultNone)
			start := time.Now()
			attached, err := manager.Attach(ctx, "web", info.SessionID, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("attach 错误=%v，期望 %v", err, tc.want)
			}
			if attached.Info.SessionID != "" {
				t.Fatalf("故障时不应返回会话快照: %+v", attached.Info)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("故障下 attach 阻塞了 %s", elapsed)
			}
		})
	}

	// 清除故障后必须恢复可重连，且游标与快照落在真实会话上。
	manager.InjectFault(terminal.FaultNone)
	attached, err := manager.Attach(ctx, "web", info.SessionID, nil)
	if err != nil {
		t.Fatalf("清除故障后 attach 失败: %v", err)
	}
	if attached.Info.SessionID != info.SessionID || attached.Info.PID != info.PID {
		t.Fatalf("attach 返回了错误会话: %+v，期望 %s/%d", attached.Info, info.SessionID, info.PID)
	}
	if err := manager.Close(ctx, "web", info.SessionID); err != nil {
		t.Fatalf("close 失败: %v", err)
	}
	waitPidGone(t, info.PID, 3*time.Second)
}

// TestAttachCursorAndOwnerFaults 断言非法游标与越权 attach 都是明确错误，不是静默成功。
func TestAttachCursorAndOwnerFaults(t *testing.T) {
	manager := terminal.New(terminal.Options{
		MaxSessions: 2,
		IdleTimeout: time.Minute,
		Shell:       "/bin/bash",
		ReplayBytes: 8,
	})
	t.Cleanup(manager.CloseAll)
	ctx := context.Background()
	info := createShell(t, manager, "web")

	if _, err := manager.Write(ctx, "web", info.SessionID, []byte("printf '0123456789abcdef\\n'\n")); err != nil {
		t.Fatalf("write 失败: %v", err)
	}
	var cursor uint64
	deadline := time.Now().Add(5 * time.Second)
	var got strings.Builder
	for time.Now().Before(deadline) {
		result, err := manager.Read(ctx, "web", info.SessionID, nil, 64<<10)
		if err != nil {
			t.Fatalf("read 失败: %v", err)
		}
		got.Write(result.Data)
		cursor = result.Cursor
		if strings.Contains(got.String(), "0123456789abcdef") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(got.String(), "0123456789abcdef") {
		t.Fatalf("未读到预期输出: %q", got.String())
	}
	if cursor <= 8 {
		t.Fatalf("游标=%d 不足以越过补读窗口", cursor)
	}

	stale := uint64(0)
	if _, err := manager.Attach(ctx, "web", info.SessionID, &stale); !errors.Is(err, terminal.ErrCursorExpired) {
		t.Fatalf("过期游标错误=%v，期望 ErrCursorExpired", err)
	}
	future := cursor + 1024
	if _, err := manager.Attach(ctx, "web", info.SessionID, &future); !errors.Is(err, terminal.ErrInvalid) {
		t.Fatalf("超前游标错误=%v，期望 ErrInvalid", err)
	}
	if _, err := manager.Attach(ctx, "other", info.SessionID, nil); !errors.Is(err, terminal.ErrForbidden) {
		t.Fatalf("越权 attach 错误=%v，期望 ErrForbidden", err)
	}
	ok := cursor
	attached, err := manager.Attach(ctx, "web", info.SessionID, &ok)
	if err != nil {
		t.Fatalf("合法游标 attach 失败: %v", err)
	}
	if !attached.Info.EOF && attached.Cursor < cursor {
		t.Fatalf("attach 游标=%d 小于已交付偏移 %d", attached.Cursor, cursor)
	}
	if err := manager.Close(ctx, "web", info.SessionID); err != nil {
		t.Fatalf("close 失败: %v", err)
	}
	waitPidGone(t, info.PID, 3*time.Second)
}

// TestHistoryCorruptStore 注入历史文件损坏：新实例 Query 必须报错并保留损坏文件（不自动重置）。
func TestHistoryCorruptStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	manager := newHistoryManager(t, dir)
	info := createShell(t, manager, "web")
	if _, err := manager.Write(context.Background(), "web", info.SessionID, []byte("echo hi\n")); err != nil {
		t.Fatalf("write 失败: %v", err)
	}
	if err := manager.Close(context.Background(), "web", info.SessionID); err != nil {
		t.Fatalf("close 失败: %v", err)
	}

	path := filepath.Join(dir, "session-"+info.SessionID+".json")
	corrupt := []byte(`{"version":1,"session_id":`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("写入损坏历史失败: %v", err)
	}

	reader := newHistoryManager(t, dir)
	entries, err := reader.History().Query("web", "", 0)
	if err == nil {
		t.Fatalf("损坏历史应报错，实际返回 %d 条", len(entries))
	}
	if !strings.Contains(err.Error(), "解析命令历史") {
		t.Fatalf("错误信息未指向历史解析失败: %v", err)
	}

	clearer := newHistoryManager(t, dir)
	sessions, cleared, err := clearer.History().Clear("web", "")
	if err == nil {
		t.Fatalf("损坏历史清理应报错，实际清理 sessions=%d entries=%d", sessions, cleared)
	}

	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, corrupt) {
		t.Fatalf("损坏历史文件被改写/删除: err=%v got=%q", err, got)
	}
}

// TestHistoryClearReadOnlyDir 注入历史目录不可写：clear 必须报错且磁盘文件原样保留。
func TestHistoryClearReadOnlyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	manager := newHistoryManager(t, dir)
	info := createShell(t, manager, "web")
	if _, err := manager.Write(context.Background(), "web", info.SessionID, []byte("echo keep\n")); err != nil {
		t.Fatalf("write 失败: %v", err)
	}
	if err := manager.Close(context.Background(), "web", info.SessionID); err != nil {
		t.Fatalf("close 失败: %v", err)
	}

	path := filepath.Join(dir, "session-"+info.SessionID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取历史文件失败: %v", err)
	}

	reader := newHistoryManager(t, dir)
	entries, err := reader.History().Query("web", "", 0)
	if err != nil || len(entries) == 0 {
		t.Fatalf("前置查询失败: err=%v entries=%d", err, len(entries))
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("设置历史目录只读失败: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()

	sessions, cleared, err := reader.History().Clear("web", "")
	if err == nil {
		t.Fatalf("历史目录只读时 clear 未报错: sessions=%d entries=%d", sessions, cleared)
	}
	if !strings.Contains(err.Error(), "删除命令历史") {
		t.Fatalf("错误信息未指向删除失败: %v", err)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(got, before) {
		t.Fatalf("clear 失败后历史文件被改写: err=%v", readErr)
	}
}
