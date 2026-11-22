//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package fault

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
)

// TestKilledCreateRejected 断言 kill 故障下 create 返回 ErrIO，而不是拿到一个死会话。
func TestKilledCreateRejected(t *testing.T) {
	manager := newManager(t, 2)
	manager.InjectFault(terminal.FaultKilled)
	if _, err := manager.Create(context.Background(), "", 80, 24); !errors.Is(err, terminal.ErrIO) {
		t.Fatalf("create 错误=%v，期望 ErrIO", err)
	}
	if listed := manager.List(); len(listed) != 0 {
		t.Fatalf("故障下不应登记会话: %+v", listed)
	}
}

// TestKilledSessionBySignal 用真实 SIGKILL 杀 shell，断言读到 eof、写入回 ErrClosed。
func TestKilledSessionBySignal(t *testing.T) {
	manager := newManager(t, 2)
	info, err := manager.Create(context.Background(), "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	if err := syscall.Kill(info.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill 会话进程失败: %v", err)
	}
	waitEOF(t, manager, info.SessionID, 5*time.Second)
	if _, err := manager.Write(context.Background(), info.SessionID, []byte("echo x\n")); !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("kill 后写入错误=%v，期望 ErrClosed", err)
	}
	if err := manager.Close(context.Background(), info.SessionID); err != nil {
		t.Fatalf("kill 后 close 失败: %v", err)
	}
	if listed := manager.List(); len(listed) != 0 {
		t.Fatalf("close 后仍有会话: %+v", listed)
	}
}

// TestInjectedKillBlocksSessionIO 断言注入 kill 后已有会话读写/调整都回 ErrClosed，
// 但 close 仍可执行——清理路径不能被故障阻断。
func TestInjectedKillBlocksSessionIO(t *testing.T) {
	manager := newManager(t, 2)
	info, err := manager.Create(context.Background(), "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	manager.InjectFault(terminal.FaultKilled)
	if _, err := manager.Read(context.Background(), info.SessionID, 1024); !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("read 错误=%v，期望 ErrClosed", err)
	}
	if _, err := manager.Write(context.Background(), info.SessionID, []byte("x")); !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("write 错误=%v，期望 ErrClosed", err)
	}
	if err := manager.Resize(context.Background(), info.SessionID, 100, 40); !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("resize 错误=%v，期望 ErrClosed", err)
	}
	if err := manager.Close(context.Background(), info.SessionID); err != nil {
		t.Fatalf("故障下 close 失败: %v", err)
	}
	waitPidGone(t, info.PID, 3*time.Second)
}

// TestKillFaultNoPanic 确保故障下所有方法都不 panic。
func TestKillFaultNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("kill 故障下发生 panic: %v", r)
		}
	}()
	manager := newManager(t, 2)
	manager.InjectFault(terminal.FaultKilled)
	_, _ = manager.Create(context.Background(), "", 80, 24)
	_, _ = manager.Read(context.Background(), "t-missing", 1024)
	_, _ = manager.Write(context.Background(), "t-missing", []byte("x"))
	_ = manager.Resize(context.Background(), "t-missing", 80, 24)
	_ = manager.Close(context.Background(), "t-missing")
}
