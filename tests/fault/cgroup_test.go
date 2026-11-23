//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package fault

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
)

// TestCgroupFaultMethods 断言 cgroup 资源受限时方法回 ErrResource，list 只读仍可用。
func TestCgroupFaultMethods(t *testing.T) {
	manager := newManager(t, 2)
	info, err := manager.Create(context.Background(), "", "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	manager.InjectFault(terminal.FaultCgroup)
	if _, err := manager.Create(context.Background(), "", "", 80, 24); !errors.Is(err, terminal.ErrResource) {
		t.Fatalf("create 错误=%v，期望 ErrResource", err)
	}
	if _, err := manager.Read(context.Background(), "", info.SessionID, nil, 1024); !errors.Is(err, terminal.ErrResource) {
		t.Fatalf("read 错误=%v，期望 ErrResource", err)
	}
	if _, err := manager.Write(context.Background(), "", info.SessionID, []byte("x")); !errors.Is(err, terminal.ErrResource) {
		t.Fatalf("write 错误=%v，期望 ErrResource", err)
	}
	if err := manager.Resize(context.Background(), "", info.SessionID, 100, 40); !errors.Is(err, terminal.ErrResource) {
		t.Fatalf("resize 错误=%v，期望 ErrResource", err)
	}
	if listed := manager.List(""); len(listed) != 1 {
		t.Fatalf("list 应只读可用且返回 1 个会话: %+v", listed)
	}
	// 清理路径不受资源故障阻断。
	if err := manager.Close(context.Background(), "", info.SessionID); err != nil {
		t.Fatalf("故障下 close 失败: %v", err)
	}
	waitPidGone(t, info.PID, 3*time.Second)
}

// TestCgroupClearedRecovers 断言资源恢复后方法恢复正常。
func TestCgroupClearedRecovers(t *testing.T) {
	manager := newManager(t, 2)
	manager.InjectFault(terminal.FaultCgroup)
	if _, err := manager.Create(context.Background(), "", "", 80, 24); !errors.Is(err, terminal.ErrResource) {
		t.Fatalf("故障下 create 错误=%v", err)
	}
	manager.InjectFault(terminal.FaultNone)
	if _, err := manager.Create(context.Background(), "", "", 80, 24); err != nil {
		t.Fatalf("清除故障后 create 失败: %v", err)
	}
}

// TestCgroupFaultNoPanic 确保资源限额下所有方法不 panic。
func TestCgroupFaultNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("cgroup 故障下发生 panic: %v", r)
		}
	}()
	manager := newManager(t, 2)
	manager.InjectFault(terminal.FaultCgroup)
	_, _ = manager.Create(context.Background(), "", "", 80, 24)
	_, _ = manager.Read(context.Background(), "", "t-missing", nil, 1024)
	_, _ = manager.Write(context.Background(), "", "t-missing", []byte("x"))
	_ = manager.Resize(context.Background(), "", "t-missing", 80, 24)
	_ = manager.Close(context.Background(), "", "t-missing")
	_ = manager.List("")
}
