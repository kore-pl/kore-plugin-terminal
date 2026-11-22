//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// sessionInfo 是 terminal.create 的结果子集。
type sessionInfo struct {
	SessionID string `json:"session_id"`
	Shell     string `json:"shell"`
	PID       int    `json:"pid"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

// TestTerminalProcessE2E 用真实插件二进制跑完整生命周期，并证明进程组被清理。
func TestTerminalProcessE2E(t *testing.T) {
	tmp := t.TempDir()
	binary := buildPlugin(t)
	socket := filepath.Join(tmp, "terminal.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("监听 UDS 失败: %v", err)
	}
	defer func() { _ = listener.Close() }()
	_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(120 * time.Second))

	process, stderr := startPlugin(t, binary, socket, 2)
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("接受插件连接失败: %v (stderr=%s)", err, stderr.String())
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(120 * time.Second))
	handshake(t, conn)

	var cleanupPids []int
	t.Cleanup(func() {
		for _, pid := range cleanupPids {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	// 1) create：真实 PTY + 登录 shell
	createdRaw := mustCall(t, conn, "e2e-create", "terminal.create", map[string]any{"cols": 100, "rows": 30})
	var created sessionInfo
	if err := json.Unmarshal([]byte(createdRaw), &created); err != nil {
		t.Fatalf("解析 create 结果失败: %v", err)
	}
	if created.SessionID == "" || created.PID <= 0 || created.Shell != "/bin/bash" || created.Cols != 100 || created.Rows != 30 {
		t.Fatalf("create 结果不符: %s", createdRaw)
	}
	cleanupPids = append(cleanupPids, created.PID)
	t.Logf("E2E_CREATE_JSON %s", createdRaw)
	t.Logf("E2E_LIST_AFTER_CREATE %s", mustCall(t, conn, "e2e-list-created", "terminal.list", map[string]any{}))

	// 2) write：验收要求的最小闭环用 echo（tty 会回显输入，命中即算读到）
	written := writeData(t, conn, "e2e-write", created.SessionID, "echo kore-terminal-ok\n")
	if written <= 0 {
		t.Fatalf("write 返回 written=%d", written)
	}
	t.Logf("E2E_WRITE written=%d", written)
	output := readUntil(t, conn, created.SessionID, "kore-terminal-ok", 10*time.Second)
	t.Logf("E2E_READ_DECODED %q", truncate(output, 400))

	// 3) 再用 printf 证明"真的执行了"：标记只存在于输出，不存在于输入回显
	writeData(t, conn, "e2e-exec", created.SessionID, "printf 'kore-terminal-exec-%s\\n' ok\n")
	execOutput := readUntil(t, conn, created.SessionID, "kore-terminal-exec-ok", 10*time.Second)
	// 标记来自 printf 的输出，输入回显里只有 %s 占位——命中即证明命令真的被执行。
	if !strings.Contains(execOutput, "kore-terminal-exec-ok") {
		t.Fatalf("printf 执行标记未出现在输出中")
	}
	t.Logf("E2E_EXEC_PROOF_WINDOW %q", window(execOutput, "kore-terminal-exec-ok", 40))

	// 4) resize：用 stty size 证明 TIOCSWINSZ 生效
	resizedRaw := mustCall(t, conn, "e2e-resize", "terminal.resize", map[string]any{"session_id": created.SessionID, "cols": 120, "rows": 40})
	t.Logf("E2E_RESIZE_JSON %s", resizedRaw)
	writeData(t, conn, "e2e-stty", created.SessionID, "stty size\n")
	sttyOutput := readUntil(t, conn, created.SessionID, "40 120", 10*time.Second)
	t.Logf("E2E_RESIZE_PROOF_WINDOW %q", window(sttyOutput, "40 120", 20))

	// 5) 用 exec -a 把会话首进程的 argv[0] 换成唯一标记，pgrep -f 可验证进程树
	marker := "kore-terminal-session-sleeper-" + randomSuffix(t)
	writeData(t, conn, "e2e-marker", created.SessionID, "exec -a "+marker+" sleep 300\n")
	time.Sleep(200 * time.Millisecond)
	before := pgrep(t, marker)
	t.Logf("E2E_PGREP_BEFORE %q (marker=%s pid=%d)", before, marker, created.PID)
	if !pidInList(before, created.PID) {
		t.Fatalf("pgrep 未匹配到会话进程: pattern=%s pid=%d output=%q", marker, created.PID, before)
	}

	// 6) close：必须 kill(-pgid)，pid 与 pgrep 双证
	closedRaw := mustCall(t, conn, "e2e-close", "terminal.close", map[string]any{"session_id": created.SessionID})
	if !strings.Contains(closedRaw, `"closed":true`) {
		t.Fatalf("close 结果不符: %s", closedRaw)
	}
	t.Logf("E2E_CLOSE_JSON %s", closedRaw)
	waitPidGone(t, created.PID, 5*time.Second)
	t.Logf("E2E_PID_GONE pid=%d kill0_err=%v", created.PID, syscall.Kill(created.PID, 0))
	waitPgrepEmpty(t, marker, 5*time.Second)
	t.Logf("E2E_PGREP_AFTER %q (marker=%s)", pgrep(t, marker), marker)

	listedRaw := mustCall(t, conn, "e2e-list-empty", "terminal.list", map[string]any{})
	if !strings.Contains(listedRaw, `"count":0`) {
		t.Fatalf("close 后 list 结果不符: %s", listedRaw)
	}
	t.Logf("E2E_LIST_AFTER_CLOSE %s", listedRaw)

	// 7) 会话上限：asset 上限 2，第 3 个必须回 E_RATELIMIT
	first := createSession(t, conn, "e2e-limit-1")
	second := createSession(t, conn, "e2e-limit-2")
	cleanupPids = append(cleanupPids, first.PID, second.PID)
	t.Logf("E2E_LIST_AT_LIMIT %s", mustCall(t, conn, "e2e-list-limit", "terminal.list", map[string]any{}))
	third := request(t, conn, "e2e-limit-3", "terminal.create", map[string]any{})
	if third.Error == nil || third.Error.Code != pluginapi.E_RATELIMIT {
		t.Fatalf("第 3 个会话错误=%+v，期望 E_RATELIMIT", third.Error)
	}
	t.Logf("E2E_LIMIT_ERROR %s: %s", third.Error.Code, third.Error.Message)
	for _, id := range []string{first.SessionID, second.SessionID} {
		mustCall(t, conn, "e2e-limit-close", "terminal.close", map[string]any{"session_id": id})
	}
	waitPidGone(t, first.PID, 5*time.Second)
	waitPidGone(t, second.PID, 5*time.Second)

	// 8) shutdown：优雅退出且无 panic
	shutdownRaw := mustCall(t, conn, "e2e-shutdown", "kore.shutdown", map[string]any{})
	if !strings.Contains(shutdownRaw, `"ok":true`) {
		t.Fatalf("shutdown 结果不符: %s", shutdownRaw)
	}
	t.Logf("E2E_SHUTDOWN_JSON %s", shutdownRaw)
	waitExit(t, process)
	if strings.Contains(stderr.String(), "panic:") {
		t.Fatalf("插件 stderr 出现 panic: %s", stderr.String())
	}
}

// createSession 调用 terminal.create 并解析会话信息。
func createSession(t *testing.T, conn net.Conn, id string) sessionInfo {
	t.Helper()
	raw := mustCall(t, conn, id, "terminal.create", map[string]any{})
	var info sessionInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("解析 create 结果失败: %v", err)
	}
	if info.SessionID == "" || info.PID <= 0 {
		t.Fatalf("create 结果不符: %s", raw)
	}
	return info
}

// randomSuffix 生成每次运行唯一的标记后缀，避免匹配到历史残留进程。
func randomSuffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("生成随机后缀失败: %v", err)
	}
	return hex.EncodeToString(buf)
}

// window 返回 want 命中位置前后 context 字节的片段，作为证据窗口。
func window(s, want string, contextBytes int) string {
	index := strings.Index(s, want)
	if index < 0 {
		return ""
	}
	start := index - contextBytes
	if start < 0 {
		start = 0
	}
	end := index + len(want) + contextBytes
	if end > len(s) {
		end = len(s)
	}
	return fmt.Sprintf("…%q…", s[start:end])
}

// truncate 截断过长日志，保留关键片段。
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf("...(共 %d 字节)", len(s))
}
