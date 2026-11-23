//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package fault

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
)

// TestDiskFullCreateRejected 断言 PTY 设备不可用（磁盘/设备故障）时 create 回 ErrIO。
func TestDiskFullCreateRejected(t *testing.T) {
	manager := newManager(t, 2)
	manager.InjectFault(terminal.FaultDiskFull)
	if _, err := manager.Create(context.Background(), "", "", 80, 24); !errors.Is(err, terminal.ErrIO) {
		t.Fatalf("create 错误=%v，期望 ErrIO", err)
	}
}

// TestDiskFullExistingSessionUnaffected 断言故障只影响建新会话，已有会话照常读写
// （方法级故障隔离，不是全局崩溃）。
func TestDiskFullExistingSessionUnaffected(t *testing.T) {
	manager := newManager(t, 2)
	info, err := manager.Create(context.Background(), "", "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	manager.InjectFault(terminal.FaultDiskFull)
	if _, err := manager.Write(context.Background(), "", info.SessionID, []byte("printf 'kore-fault-%s\\n' ok\n")); err != nil {
		t.Fatalf("故障下 write 失败: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var output strings.Builder
	for time.Now().Before(deadline) {
		result, err := manager.Read(context.Background(), "", info.SessionID, nil, 64<<10)
		if err != nil {
			t.Fatalf("故障下 read 失败: %v", err)
		}
		output.Write(result.Data)
		if strings.Contains(output.String(), "kore-fault-ok") {
			return
		}
		if result.EOF {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("故障下未读到执行结果: %q", output.String())
}

// TestDiskFullClearedRecovers 断言清除故障后可重新建会话。
func TestDiskFullClearedRecovers(t *testing.T) {
	manager := newManager(t, 2)
	manager.InjectFault(terminal.FaultDiskFull)
	if _, err := manager.Create(context.Background(), "", "", 80, 24); !errors.Is(err, terminal.ErrIO) {
		t.Fatalf("故障下 create 错误=%v，期望 ErrIO", err)
	}
	manager.InjectFault(terminal.FaultNone)
	if _, err := manager.Create(context.Background(), "", "", 80, 24); err != nil {
		t.Fatalf("清除故障后 create 失败: %v", err)
	}
}
