//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package terminal

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newTestManager 构造真实 PTY 管理器；清理时关闭全部会话，保证不残留 shell。
func newTestManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	if opts.MaxSessions == 0 {
		opts.MaxSessions = DefaultMaxSessions
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = time.Minute
	}
	opts.Shell = "/bin/bash"
	manager := New(opts)
	t.Cleanup(manager.CloseAll)
	return manager
}

// waitContains 轮询读取，直到出现 want（增量读语义下空读不是错误）。
func waitContains(t *testing.T, manager *Manager, id, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got strings.Builder
	for time.Now().Before(deadline) {
		result, err := manager.Read(context.Background(), id, 64<<10)
		if err != nil {
			t.Fatalf("读取会话失败: %v", err)
		}
		got.Write(result.Data)
		if strings.Contains(got.String(), want) {
			return got.String()
		}
		if result.EOF {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s 内未读到 %q，实际输出=%q", timeout, want, got.String())
	return ""
}

// waitPidGone 断言 pid 在超时内不存在。
func waitPidGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d 在 %s 内未退出", pid, timeout)
}

// TestCreateWriteRead 是验收要求的最小闭环：create → write echo → read 到 hello。
func TestCreateWriteRead(t *testing.T) {
	manager := newTestManager(t, Options{})
	info, err := manager.Create(context.Background(), "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	if info.SessionID == "" || info.PID <= 0 || info.Shell != "/bin/bash" {
		t.Fatalf("create 结果不符: %+v", info)
	}
	if _, err := manager.Write(context.Background(), info.SessionID, []byte("echo hello\n")); err != nil {
		t.Fatalf("write 失败: %v", err)
	}
	waitContains(t, manager, info.SessionID, "hello", 5*time.Second)
}

// TestReadProvesExecution 用 printf 输出一个只存在于结果里的标记，排除 tty 回显的干扰。
func TestReadProvesExecution(t *testing.T) {
	manager := newTestManager(t, Options{})
	info, err := manager.Create(context.Background(), "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	if _, err := manager.Write(context.Background(), info.SessionID, []byte("printf 'kore-pty-%s\\n' ok\n")); err != nil {
		t.Fatalf("write 失败: %v", err)
	}
	// 命令里写的是 %s 占位，只有真正执行才会出现 kore-pty-ok。
	waitContains(t, manager, info.SessionID, "kore-pty-ok", 5*time.Second)
}

// TestResizeApplies 用 stty size 证明 TIOCSWINSZ 真的生效。
func TestResizeApplies(t *testing.T) {
	manager := newTestManager(t, Options{})
	info, err := manager.Create(context.Background(), "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	if err := manager.Resize(context.Background(), info.SessionID, 120, 40); err != nil {
		t.Fatalf("resize 失败: %v", err)
	}
	if _, err := manager.Write(context.Background(), info.SessionID, []byte("stty size\n")); err != nil {
		t.Fatalf("write 失败: %v", err)
	}
	waitContains(t, manager, info.SessionID, "40 120", 5*time.Second)

	listed := manager.List()
	if len(listed) != 1 || listed[0].Cols != 120 || listed[0].Rows != 40 {
		t.Fatalf("list 未反映 resize: %+v", listed)
	}
}

// TestReadIsIncremental 断言没有输出时读立即返回空串，不阻塞会话循环。
func TestReadIsIncremental(t *testing.T) {
	manager := newTestManager(t, Options{})
	info, err := manager.Create(context.Background(), "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		start := time.Now()
		result, err := manager.Read(context.Background(), info.SessionID, 4096)
		if err != nil {
			t.Fatalf("read 失败: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("read 阻塞 %s，不符合增量语义", elapsed)
		}
		if len(result.Data) == 0 {
			return // 读到空串即满足断言
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Log("会话持续有输出，未观察到空读（仍满足非阻塞断言）")
}

// TestCloseKillsProcessGroup 断言 close 后 shell 进程不存在、会话从 map 移除。
func TestCloseKillsProcessGroup(t *testing.T) {
	manager := newTestManager(t, Options{})
	info, err := manager.Create(context.Background(), "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	if err := manager.Close(context.Background(), info.SessionID); err != nil {
		t.Fatalf("close 失败: %v", err)
	}
	waitPidGone(t, info.PID, 3*time.Second)
	if listed := manager.List(); len(listed) != 0 {
		t.Fatalf("close 后 list 仍有会话: %+v", listed)
	}
	if _, err := manager.Read(context.Background(), info.SessionID, 1024); !errors.Is(err, ErrNotFound) {
		t.Fatalf("close 后 read 错误=%v，期望 ErrNotFound", err)
	}
	if err := manager.Close(context.Background(), info.SessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复 close 错误=%v，期望 ErrNotFound", err)
	}
}

// TestSessionLimit 断言超出上限返回 ErrRateLimit，关掉一个后可继续创建。
func TestSessionLimit(t *testing.T) {
	manager := newTestManager(t, Options{MaxSessions: 2})
	first, err := manager.Create(context.Background(), "", 80, 24)
	if err != nil {
		t.Fatalf("create #1 失败: %v", err)
	}
	if _, err := manager.Create(context.Background(), "", 80, 24); err != nil {
		t.Fatalf("create #2 失败: %v", err)
	}
	if _, err := manager.Create(context.Background(), "", 80, 24); !errors.Is(err, ErrRateLimit) {
		t.Fatalf("create #3 错误=%v，期望 ErrRateLimit", err)
	}
	if err := manager.Close(context.Background(), first.SessionID); err != nil {
		t.Fatalf("close 失败: %v", err)
	}
	if _, err := manager.Create(context.Background(), "", 80, 24); err != nil {
		t.Fatalf("释放名额后 create 失败: %v", err)
	}
}

// TestIdleSweep 断言没有 ticker 也能在 create 时顺带回收空闲会话。
func TestIdleSweep(t *testing.T) {
	manager := newTestManager(t, Options{IdleTimeout: 300 * time.Millisecond})
	stale, err := manager.Create(context.Background(), "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := manager.Create(context.Background(), "", 80, 24); err != nil {
		t.Fatalf("第二次 create 失败: %v", err)
	}
	if listed := manager.List(); len(listed) != 1 {
		t.Fatalf("空闲会话未回收: %+v", listed)
	}
	waitPidGone(t, stale.PID, 3*time.Second)
	if _, err := manager.Read(context.Background(), stale.SessionID, 1024); !errors.Is(err, ErrNotFound) {
		t.Fatalf("被回收会话 read 错误=%v，期望 ErrNotFound", err)
	}
}

// TestShellExitSetsEOF 断言 shell 退出后读到 eof，随后写入回 ErrClosed。
func TestShellExitSetsEOF(t *testing.T) {
	manager := newTestManager(t, Options{})
	info, err := manager.Create(context.Background(), "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	if _, err := manager.Write(context.Background(), info.SessionID, []byte("exit\n")); err != nil {
		t.Fatalf("write exit 失败: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		result, err := manager.Read(context.Background(), info.SessionID, 4096)
		if err != nil {
			t.Fatalf("read 失败: %v", err)
		}
		if result.EOF {
			if _, err := manager.Write(context.Background(), info.SessionID, []byte("echo after-exit\n")); !errors.Is(err, ErrClosed) {
				t.Fatalf("退出后写入错误=%v，期望 ErrClosed", err)
			}
			return
		}
	}
	t.Fatal("shell 退出后未读到 eof")
}

// TestInvalidParams 断言尺寸与 shell 校验在起进程之前拦下。
func TestInvalidParams(t *testing.T) {
	manager := newTestManager(t, Options{})
	cases := []struct {
		name  string
		shell string
		cols  int
		rows  int
	}{
		{"cols=0", "", 0, 24},
		{"rows=0", "", 80, 0},
		{"cols 超限", "", 1001, 24},
		{"相对路径 shell", "bash", 80, 24},
		{"不存在的 shell", "/nonexistent/bash", 80, 24},
		{"目录当 shell", "/tmp", 80, 24},
	}
	for _, c := range cases {
		if _, err := manager.Create(context.Background(), c.shell, c.cols, c.rows); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s 错误=%v，期望 ErrInvalid", c.name, err)
		}
	}
}

// TestUnknownSession 断言四个会话方法对未知 id 都回 ErrNotFound。
func TestUnknownSession(t *testing.T) {
	manager := newTestManager(t, Options{})
	ctx := context.Background()
	if _, err := manager.Read(ctx, "t-missing", 1024); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read 错误=%v", err)
	}
	if _, err := manager.Write(ctx, "t-missing", []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("write 错误=%v", err)
	}
	if err := manager.Resize(ctx, "t-missing", 80, 24); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resize 错误=%v", err)
	}
	if err := manager.Close(ctx, "t-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("close 错误=%v", err)
	}
}

// TestWriteTooLarge 断言单次写入超上限时拒绝。
func TestWriteTooLarge(t *testing.T) {
	manager := newTestManager(t, Options{})
	info, err := manager.Create(context.Background(), "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	if _, err := manager.Write(context.Background(), info.SessionID, make([]byte, MaxWriteBytes+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("超限写入错误=%v，期望 ErrInvalid", err)
	}
}

// TestCloseAll 断言插件退出路径清空全部会话与进程。
func TestCloseAll(t *testing.T) {
	manager := newTestManager(t, Options{MaxSessions: 3})
	pids := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		info, err := manager.Create(context.Background(), "", 80, 24)
		if err != nil {
			t.Fatalf("create #%d 失败: %v", i, err)
		}
		pids = append(pids, info.PID)
	}
	manager.CloseAll()
	for _, pid := range pids {
		waitPidGone(t, pid, 3*time.Second)
	}
	if listed := manager.List(); len(listed) != 0 {
		t.Fatalf("CloseAll 后仍有会话: %+v", listed)
	}
}
