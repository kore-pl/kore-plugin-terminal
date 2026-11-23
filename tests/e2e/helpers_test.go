//go:build linux && (amd64 || arm64 || riscv64 || loong64)

// Package e2e 用真实插件进程跑终端生命周期：UDS 握手 → create → write →
// 增量 read → resize → close，并用 pgrep 证明进程组被清理。
package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// buildPlugin 构建真实插件二进制到临时目录。
func buildPlugin(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "terminal-plugin")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建插件失败: %v\n%s", err, out)
	}
	return binary
}

// startPlugin 以协议环境变量启动插件进程；stderr 收集起来便于失败时定位。
// extraEnv 追加覆盖项（如 KORE_STATE_DIR 用于命令历史、KORE_TERMINAL_IDLE_SEC 用于回收）。
func startPlugin(t *testing.T, binary, socket string, maxSessions int, extraEnv ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	stderr := &bytes.Buffer{}
	process := exec.Command(binary)
	env := append(os.Environ(),
		"KORE_PLUGIN_SOCK="+socket,
		"KORE_PLUGIN_ID=terminal",
		"KORE_PLUGIN_TOKEN=e2e-token",
		"KORE_TERMINAL_MAX_SESSIONS="+strconv.Itoa(maxSessions),
	)
	process.Env = append(env, extraEnv...)
	process.Stdout = io.Discard
	process.Stderr = stderr
	if err := process.Start(); err != nil {
		t.Fatalf("启动插件失败: %v", err)
	}
	t.Cleanup(func() {
		if process.ProcessState == nil {
			_ = process.Process.Kill()
			_, _ = process.Process.Wait()
		}
	})
	return process, stderr
}

// handshake 读 kore.hello 并回 ok 响应。
func handshake(t *testing.T, conn net.Conn) {
	t.Helper()
	request := readRequest(t, conn)
	if request.Method != "kore.hello" {
		t.Fatalf("首帧应为 kore.hello，实际 %s", request.Method)
	}
	response := &pluginapi.Response{
		Type: pluginapi.TypeResponse, ID: request.ID, ReqID: request.ReqID,
		Result: json.RawMessage(`{"ok":true,"protocol":"kore.plugin.v1"}`),
	}
	if err := pluginapi.WriteJSON(conn, response); err != nil {
		t.Fatalf("回写 hello 失败: %v", err)
	}
}

// request 发送一次方法调用并读回响应。
func request(t *testing.T, conn net.Conn, id, method string, params any) *pluginapi.Response {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("序列化 %s 参数失败: %v", method, err)
	}
	req := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: id, ReqID: "r-" + id, Method: method, Params: raw,
	}
	if err := pluginapi.WriteJSON(conn, req); err != nil {
		t.Fatalf("发送 %s 失败: %v", method, err)
	}
	return readResponse(t, conn)
}

// mustCall 要求调用成功并返回 result 原文。
func mustCall(t *testing.T, conn net.Conn, id, method string, params any) string {
	t.Helper()
	response := request(t, conn, id, method, params)
	if response.Error != nil {
		t.Fatalf("%s 失败: %+v", method, response.Error)
	}
	return string(response.Result)
}

// readRequest 读下一帧并断言是 request。
func readRequest(t *testing.T, conn net.Conn) *pluginapi.Request {
	t.Helper()
	env, err := pluginapi.DecodeFrame(readPayload(t, conn))
	if err != nil {
		t.Fatalf("解码请求失败: %v", err)
	}
	request, ok := env.(*pluginapi.Request)
	if !ok {
		t.Fatalf("期望 request，实际 %T", env)
	}
	return request
}

// readResponse 读下一帧并断言是 response。
func readResponse(t *testing.T, conn net.Conn) *pluginapi.Response {
	t.Helper()
	env, err := pluginapi.DecodeFrame(readPayload(t, conn))
	if err != nil {
		t.Fatalf("解码响应失败: %v", err)
	}
	response, ok := env.(*pluginapi.Response)
	if !ok {
		t.Fatalf("期望 response，实际 %T", env)
	}
	return response
}

// readPayload 读一整帧。
func readPayload(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	payload, err := pluginapi.ReadFrame(conn, pluginapi.MaxFrameSize)
	if err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	return payload
}

// waitExit 等待插件进程退出。
func waitExit(t *testing.T, process *exec.Cmd) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("插件退出异常: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("插件未在 5s 内退出")
	}
}

// b64 是 terminal.write.data 的唯一编码（标准 base64）。
func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// writeData 调用 terminal.write 并返回 written。
func writeData(t *testing.T, conn net.Conn, id, sessionID, data string) int {
	t.Helper()
	raw := mustCall(t, conn, id, "terminal.write", map[string]any{
		"session_id": sessionID, "data": b64(data),
	})
	var result struct {
		Written int `json:"written"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("解析 write 结果失败: %v", err)
	}
	return result.Written
}

// readUntil 轮询 terminal.read（增量语义）直到出现 want，返回累计解码输出。
func readUntil(t *testing.T, conn net.Conn, sessionID, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var output strings.Builder
	for time.Now().Before(deadline) {
		raw := mustCall(t, conn, "e2e-read", "terminal.read", map[string]any{
			"session_id": sessionID, "max_bytes": 65536,
		})
		var result struct {
			Data  string `json:"data"`
			Bytes int    `json:"bytes"`
			EOF   bool   `json:"eof"`
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatalf("解析 read 结果失败: %v", err)
		}
		chunk, err := base64.StdEncoding.DecodeString(result.Data)
		if err != nil {
			t.Fatalf("read.data 不是合法 base64: %v", err)
		}
		if result.Bytes != len(chunk) {
			t.Fatalf("bytes=%d 与解码长度 %d 不一致", result.Bytes, len(chunk))
		}
		output.Write(chunk)
		if strings.Contains(output.String(), want) {
			return output.String()
		}
		if result.EOF {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s 内未读到 %q，累计输出=%q", timeout, want, output.String())
	return ""
}

// pgrep 返回匹配 pattern 的 pid 列表（每行一个）；无匹配返回空串。
func pgrep(t *testing.T, pattern string) string {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", "--", pattern).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return ""
		}
		t.Fatalf("pgrep 失败: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// pidInList 判断 pgrep 输出里是否包含指定 pid。
func pidInList(list string, pid int) bool {
	want := strconv.Itoa(pid)
	for _, line := range strings.Split(list, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
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

// waitPgrepEmpty 断言 pattern 在超时内不再匹配任何进程。
func waitPgrepEmpty(t *testing.T, pattern string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pgrep(t, pattern) == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s 内仍有进程匹配 %q: %s", timeout, pattern, pgrep(t, pattern))
}
