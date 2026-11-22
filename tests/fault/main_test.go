//go:build linux && (amd64 || arm64 || riscv64 || loong64)

// Package fault 是 terminal 插件的四类故障注入测试（kill / timeout / disk / cgroup）：
// 断言方法级错误返回符合协议映射，且清理路径永远可用（不能因故障留下残留 shell）。
package fault

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
)

// newManager 构造真实 PTY 管理器；t.Cleanup 关闭全部会话，测试不留进程。
func newManager(t *testing.T, maxSessions int) *terminal.Manager {
	t.Helper()
	if maxSessions <= 0 {
		maxSessions = 4
	}
	manager := terminal.New(terminal.Options{MaxSessions: maxSessions, IdleTimeout: time.Minute, Shell: "/bin/bash"})
	t.Cleanup(manager.CloseAll)
	return manager
}

// waitEOF 轮询读取直到 eof（shell 退出 + 输出读完）。
func waitEOF(t *testing.T, manager *terminal.Manager, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result, err := manager.Read(context.Background(), id, 64<<10)
		if err != nil {
			t.Fatalf("读取会话失败: %v", err)
		}
		if result.EOF {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s 内未读到 eof", timeout)
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
