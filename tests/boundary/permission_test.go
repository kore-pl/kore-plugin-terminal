//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package boundary

// 权限不足（permission / EACCES）边界：全部用真实 chmod 与真实 PTY 设备，不用模拟故障。
// 四类落点：shell 文件无执行位、shell 所在目录 chmod 000、历史目录不可写、PTY 从端 chmod 000。
// 断言明确的哨兵错误（ErrInvalid）或系统错误 errno（EACCES），不接受含糊字符串。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
)

// TestShellWithoutExecPermissionRejected 断言 shell 文件 chmod 000（无执行位）时
// create 返回 ErrInvalid，且错误信息点明"不是可执行文件"。
func TestShellWithoutExecPermissionRejected(t *testing.T) {
	shell := catShell(t)
	if err := os.Chmod(shell, 0o000); err != nil {
		t.Fatalf("chmod 000 shell 失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(shell, 0o755) })

	manager := newManager(t, 2, terminal.HistoryOptions{})
	_, err := manager.Create(bg(), testClient, shell, 80, 24)
	if !errors.Is(err, terminal.ErrInvalid) {
		t.Fatalf("无执行位 shell 错误=%v，期望 ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "不是可执行文件") {
		t.Fatalf("错误=%v，期望点明不是可执行文件", err)
	}
}

// TestShellInUnreadableDirectoryRejected 断言 shell 所在目录 chmod 000 时，
// stat 返回 EACCES，create 返回 ErrInvalid 且保留 permission denied 原文。
func TestShellInUnreadableDirectoryRejected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 绕过 DAC 权限检查，本用例只对非 root 有意义")
	}
	dir := t.TempDir()
	shell := filepath.Join(dir, "kore-boundary-shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nexec cat\n"), 0o755); err != nil {
		t.Fatalf("写 shell 失败: %v", err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod 000 目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	manager := newManager(t, 2, terminal.HistoryOptions{})
	_, err := manager.Create(bg(), testClient, shell, 80, 24)
	if !errors.Is(err, terminal.ErrInvalid) {
		t.Fatalf("目录不可读时错误=%v，期望 ErrInvalid", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Fatalf("错误=%v，期望保留 permission denied 原文", err)
	}
}

// TestHistoryDirectoryPermissionDenied 断言历史目标目录不可创建（父目录 chmod 000）时：
// 终端 write 仍然成功（历史是 best-effort），但 History.Status 如实披露 permission denied，
// 不把"没落住盘"静默吞掉。
func TestHistoryDirectoryPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 绕过 DAC 权限检查，本用例只对非 root 有意义")
	}
	base := t.TempDir()
	if err := os.Chmod(base, 0o000); err != nil {
		t.Fatalf("chmod 000 父目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o755) })

	historyDir := filepath.Join(base, "history")
	manager := newManager(t, 2, terminal.HistoryOptions{Dir: historyDir, Debounce: time.Nanosecond})
	info := mustCreate(t, manager, testClient, "", 80, 24)

	payload := []byte("echo kore-permission\n")
	written, err := manager.Write(bg(), testClient, info.SessionID, payload)
	if err != nil || written != len(payload) {
		t.Fatalf("历史落盘失败不应影响终端输入: written=%d err=%v", written, err)
	}

	dir, lastErr := manager.History().Status()
	if dir != historyDir {
		t.Fatalf("历史目录=%q，期望 %q", dir, historyDir)
	}
	if !strings.Contains(strings.ToLower(lastErr), "permission denied") {
		t.Fatalf("历史状态 lastErr=%q，期望披露 permission denied", lastErr)
	}
}

// TestPTYDevicePermissionDenied 断言真实 PTY 从端 chmod 000 后，打开设备返回 EACCES：
// 这是"PTY 设备无权限"内核语义的直接证据（已打开的 fd 不受影响，故不影响存活会话）。
func TestPTYDevicePermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 绕过 DAC 权限检查，本用例只对非 root 有意义")
	}
	manager := newManager(t, 2, terminal.HistoryOptions{})
	info := mustCreate(t, manager, testClient, "", 80, 24)

	// 子进程 fd 0 就是 PTY 从端；从 /proc 精确取路径，避免扫描 /dev/pts 的竞态。
	slave, err := os.Readlink(filepath.Join("/proc", itoa(info.PID), "fd", "0"))
	if err != nil || !strings.HasPrefix(slave, "/dev/pts/") {
		t.Skipf("无法定位 PTY 从端（pid=%d link=%q err=%v）", info.PID, slave, err)
	}
	stat, err := os.Stat(slave)
	if err != nil {
		t.Fatalf("stat PTY 从端失败: %v", err)
	}
	origin := stat.Mode().Perm()
	if err := os.Chmod(slave, 0o000); err != nil {
		t.Fatalf("chmod 000 PTY 从端失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(slave, origin) })

	file, err := os.OpenFile(slave, os.O_RDWR, 0)
	if err == nil {
		_ = file.Close()
		t.Fatalf("PTY 从端 %s chmod 000 后仍可打开，权限语义失效", slave)
	}
	if !errors.Is(err, fs.ErrPermission) && !errors.Is(err, syscall.EACCES) {
		t.Fatalf("打开无权限 PTY 错误=%v，期望 EACCES", err)
	}
}

// itoa 是 strconv.Itoa 的薄封装，避免为单次调用引入额外 import 区域。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
