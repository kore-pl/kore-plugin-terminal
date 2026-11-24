//go:build linux && (amd64 || arm64 || riscv64 || loong64)

// Package boundary 是 terminal 插件的边界用例（G-20 判据①），覆盖四类：
//
//	异常输入：非法会话 id、超长命令、非法终端尺寸、格式错误的帧（malformed frame）。
//	并发：真实 goroutine 并发创建/销毁会话、同一会话并发写、并发 attach/detach。
//	权限不足：PTY 设备无权限（EACCES）、shell 所在目录 chmod 000、历史目录不可写。
//	资源耗尽：真实注入 ulimit -n（fd 耗尽）、ulimit -v（地址空间耗尽）、tmpfs 写满（ENOSPC）。
//
// 本包刻意不调用 InjectFault：模拟故障属于 tests/fault 的判据②，边界用例只打真实内核边界。
// 所有会话都用立即执行的 `cat` 脚本，不启动交互式 shell，避免测试挂住；每个用例都在
// t.Cleanup 中关闭全部会话，测试结束不留进程。
package boundary

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
)

const (
	// testClient 是边界用例默认的调用方标识。
	testClient = "boundary"
	// readChunk 是轮询读取的块大小。
	readChunk = 64 << 10
)

// writeShell 在临时目录写一个立即执行的辅助脚本（非交互，避免测试挂住）。
func writeShell(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kore-boundary-shell")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("写辅助 shell 失败: %v", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("设置辅助 shell 权限失败: %v", err)
	}
	return path
}

// catShell 是回显 cat：读多少写多少，收到 EOF 退出，不产生交互提示符。
func catShell(t *testing.T) string {
	t.Helper()
	return writeShell(t, "#!/bin/sh\nexec cat\n")
}

// quietCatShell 关闭回显并把 cat 输出丢弃：用于大量写入且测试不消费输出的场景。
func quietCatShell(t *testing.T) string {
	t.Helper()
	return writeShell(t, "#!/bin/sh\nstty -echo 2>/dev/null\nexec cat >/dev/null 2>&1\n")
}

// newManager 构造真实 PTY 管理器；t.Cleanup 关闭全部会话，测试不留进程。
func newManager(t *testing.T, maxSessions int, history terminal.HistoryOptions) *terminal.Manager {
	t.Helper()
	manager := terminal.New(terminal.Options{
		MaxSessions: maxSessions,
		IdleTimeout: time.Minute,
		Shell:       catShell(t),
		History:     history,
	})
	t.Cleanup(manager.CloseAll)
	return manager
}

// mustCreate 断言 create 成功并返回会话快照；shell 为空时用管理器默认 shell。
func mustCreate(t *testing.T, manager *terminal.Manager, clientID, shell string, cols, rows int) terminal.Info {
	t.Helper()
	info, err := manager.Create(context.Background(), clientID, shell, cols, rows)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	if info.SessionID == "" {
		t.Fatal("create 返回空 session_id")
	}
	return info
}

// readUntil 轮询读取，直到 cond 成立或超时；返回已读内容。
func readUntil(t *testing.T, manager *terminal.Manager, clientID, id string, timeout time.Duration, cond func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var output strings.Builder
	for time.Now().Before(deadline) {
		result, err := manager.Read(context.Background(), clientID, id, nil, readChunk)
		if err != nil {
			t.Fatalf("读取会话失败: %v", err)
		}
		output.Write(result.Data)
		if cond(output.String()) {
			return output.String()
		}
		if result.EOF {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return output.String()
}

// waitToken 断言 token 在超时内出现。
func waitToken(t *testing.T, manager *terminal.Manager, clientID, id, token string, timeout time.Duration) {
	t.Helper()
	output := readUntil(t, manager, clientID, id, timeout, func(s string) bool {
		return strings.Contains(s, token)
	})
	if !strings.Contains(output, token) {
		t.Fatalf("%s 内未读到 %q，已读 %d 字节: %q", timeout, token, len(output), tail(output, 512))
	}
}

// tail 截取末尾 n 字节，避免失败信息过长。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
