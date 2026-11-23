//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package fault

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
)

// TestTimeoutContextExpired 断言请求预算耗尽时所有方法回 ErrTimeout。
func TestTimeoutContextExpired(t *testing.T) {
	manager := newManager(t, 2)
	info, err := manager.Create(context.Background(), "", "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Create(ctx, "", "", 80, 24); !errors.Is(err, terminal.ErrTimeout) {
		t.Fatalf("create 错误=%v，期望 ErrTimeout", err)
	}
	if _, err := manager.Read(ctx, "", info.SessionID, nil, 1024); !errors.Is(err, terminal.ErrTimeout) {
		t.Fatalf("read 错误=%v，期望 ErrTimeout", err)
	}
	if _, err := manager.Write(ctx, "", info.SessionID, []byte("x")); !errors.Is(err, terminal.ErrTimeout) {
		t.Fatalf("write 错误=%v，期望 ErrTimeout", err)
	}
	if err := manager.Resize(ctx, "", info.SessionID, 100, 40); !errors.Is(err, terminal.ErrTimeout) {
		t.Fatalf("resize 错误=%v，期望 ErrTimeout", err)
	}
	if err := manager.Close(ctx, "", info.SessionID); !errors.Is(err, terminal.ErrTimeout) {
		t.Fatalf("close 错误=%v，期望 ErrTimeout", err)
	}
	if err := manager.Close(context.Background(), "", info.SessionID); err != nil {
		t.Fatalf("用正常 ctx 清理失败: %v", err)
	}
}

// TestTimeoutFaultMethods 断言注入超时故障后方法立即返回 ErrTimeout，不阻塞。
func TestTimeoutFaultMethods(t *testing.T) {
	manager := newManager(t, 2)
	info, err := manager.Create(context.Background(), "", "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	manager.InjectFault(terminal.FaultTimeout)
	start := time.Now()
	if _, err := manager.Create(context.Background(), "", "", 80, 24); !errors.Is(err, terminal.ErrTimeout) {
		t.Fatalf("create 错误=%v，期望 ErrTimeout", err)
	}
	if _, err := manager.Read(context.Background(), "", info.SessionID, nil, 1024); !errors.Is(err, terminal.ErrTimeout) {
		t.Fatalf("read 错误=%v，期望 ErrTimeout", err)
	}
	if _, err := manager.Write(context.Background(), "", info.SessionID, []byte("x")); !errors.Is(err, terminal.ErrTimeout) {
		t.Fatalf("write 错误=%v，期望 ErrTimeout", err)
	}
	if err := manager.Resize(context.Background(), "", info.SessionID, 100, 40); !errors.Is(err, terminal.ErrTimeout) {
		t.Fatalf("resize 错误=%v，期望 ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("超时故障下方法阻塞了 %s", elapsed)
	}
}

// TestTimeoutClearedRecovers 断言清除故障后终端恢复可用。
func TestTimeoutClearedRecovers(t *testing.T) {
	manager := newManager(t, 2)
	manager.InjectFault(terminal.FaultTimeout)
	if _, err := manager.Create(context.Background(), "", "", 80, 24); !errors.Is(err, terminal.ErrTimeout) {
		t.Fatalf("故障下 create 错误=%v", err)
	}
	manager.InjectFault(terminal.FaultNone)
	info, err := manager.Create(context.Background(), "", "", 80, 24)
	if err != nil {
		t.Fatalf("清除故障后 create 失败: %v", err)
	}
	if _, err := manager.Write(context.Background(), "", info.SessionID, []byte("echo recovered\n")); err != nil {
		t.Fatalf("清除故障后 write 失败: %v", err)
	}
}
