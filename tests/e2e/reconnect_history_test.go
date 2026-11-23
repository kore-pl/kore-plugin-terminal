//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// startIsolatedPlugin 起一个独立插件进程（独立 socket + 状态目录）并完成握手。
func startIsolatedPlugin(t *testing.T, binary string, env ...string) net.Conn {
	t.Helper()
	tmp := t.TempDir()
	socket := filepath.Join(tmp, "terminal.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("监听 UDS 失败: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(120 * time.Second))
	process, stderr := startPlugin(t, binary, socket, 2, append(env, "KORE_STATE_DIR="+filepath.Join(tmp, "state"))...)
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("接受插件连接失败: %v (stderr=%s)", err, stderr.String())
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(120 * time.Second))
	handshake(t, conn)
	t.Cleanup(func() {
		if process.ProcessState == nil {
			_ = process.Process.Kill()
			_, _ = process.Process.Wait()
		}
	})
	return conn
}

// writeClient 以指定 client_id 写入输入。
func writeClient(t *testing.T, conn net.Conn, clientID, sessionID, data string) int {
	t.Helper()
	raw := mustCall(t, conn, "e2e-write-client", "terminal.write", map[string]any{
		"client_id": clientID, "session_id": sessionID, "data": b64(data),
	})
	var result struct {
		Written int `json:"written"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("解析 write 结果失败: %v", err)
	}
	return result.Written
}

// readClient 以指定 client_id（与可选游标）读取一次。
func readClient(t *testing.T, conn net.Conn, clientID, sessionID string, cursor *uint64) (string, uint64, bool, bool) {
	t.Helper()
	params := map[string]any{"client_id": clientID, "session_id": sessionID, "max_bytes": 65536}
	if cursor != nil {
		params["cursor"] = *cursor
	}
	raw := mustCall(t, conn, "e2e-read-client", "terminal.read", params)
	var result struct {
		Data   string `json:"data"`
		Cursor uint64 `json:"cursor"`
		Replay bool   `json:"replay"`
		EOF    bool   `json:"eof"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("解析 read 结果失败: %v", err)
	}
	chunk, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil {
		t.Fatalf("read.data 不是合法 base64: %v", err)
	}
	return string(chunk), result.Cursor, result.Replay, result.EOF
}

// drainQuietClient 读到静默 timeout 为止，返回累计输出与最终游标。
func drainQuietClient(t *testing.T, conn net.Conn, clientID, sessionID string, quiet time.Duration) (string, uint64) {
	t.Helper()
	var output strings.Builder
	var cursor uint64
	deadline := time.Now().Add(10 * time.Second)
	lastData := time.Now()
	for time.Now().Before(deadline) {
		chunk, next, _, _ := readClient(t, conn, clientID, sessionID, nil)
		cursor = next
		if chunk != "" {
			output.WriteString(chunk)
			lastData = time.Now()
			continue
		}
		if time.Since(lastData) >= quiet {
			return output.String(), cursor
		}
		time.Sleep(20 * time.Millisecond)
	}
	return output.String(), cursor
}

// TestReconnectCursorAndHistoryE2E 用真实插件进程覆盖：attach 续读、越权拒绝、命令历史往返。
func TestReconnectCursorAndHistoryE2E(t *testing.T) {
	binary := buildPlugin(t)
	conn := startIsolatedPlugin(t, binary, "KORE_TERMINAL_IDLE_SEC=120")
	const clientID = "e2e-alpha"

	createdRaw := mustCall(t, conn, "e2e-rh-create", "terminal.create", map[string]any{
		"client_id": clientID, "cols": 100, "rows": 30,
	})
	var created sessionInfo
	if err := json.Unmarshal([]byte(createdRaw), &created); err != nil {
		t.Fatalf("解析 create 结果失败: %v", err)
	}
	t.Logf("E2E_RH_CREATE %s", createdRaw)

	// 1) 第一段输出：增量读到标记，记录游标。
	writeClient(t, conn, clientID, created.SessionID, "printf 'kore-e2e-one-%s\\n' ok\n")
	first, firstCursor := drainQuietClient(t, conn, clientID, created.SessionID, time.Second)
	if !strings.Contains(first, "kore-e2e-one-ok") {
		t.Fatalf("第一段输出未包含标记: %q", first)
	}
	t.Logf("E2E_RH_FIRST cursor=%d bytes=%d", firstCursor, len(first))

	// 2) attach：游标可恢复，存活语义字段齐备。
	attachRaw := mustCall(t, conn, "e2e-rh-attach", "terminal.attach", map[string]any{
		"client_id": clientID, "session_id": created.SessionID, "cursor": firstCursor,
	})
	var attached struct {
		Attached      bool   `json:"attached"`
		Cursor        uint64 `json:"cursor"`
		ReplayBytes   int    `json:"replay_bytes"`
		IdleExpiresAt string `json:"idle_expires_at"`
	}
	if err := json.Unmarshal([]byte(attachRaw), &attached); err != nil {
		t.Fatalf("解析 attach 结果失败: %v", err)
	}
	if !attached.Attached || attached.Cursor != firstCursor || attached.IdleExpiresAt == "" {
		t.Fatalf("attach 结果不符: %s", attachRaw)
	}
	t.Logf("E2E_RH_ATTACH %s", attachRaw)

	// 3) 掉线期间的新输出：先用增量读消费掉，再用旧游标续读，必须回放同一段字节。
	writeClient(t, conn, clientID, created.SessionID, "printf 'kore-e2e-two-%s\\n' ok\n")
	second, secondCursor := drainQuietClient(t, conn, clientID, created.SessionID, time.Second)
	if !strings.Contains(second, "kore-e2e-two-ok") {
		t.Fatalf("第二段输出未包含标记: %q", second)
	}
	replayed, replayCursor, isReplay, _ := readClient(t, conn, clientID, created.SessionID, &firstCursor)
	if !isReplay || replayCursor != secondCursor || replayed != second {
		t.Fatalf("续读回放不符: replay=%v cursor=%d/%d", isReplay, replayCursor, secondCursor)
	}
	t.Logf("E2E_RH_REPLAY cursor=%d bytes=%d", replayCursor, len(replayed))

	// 4) 越权：别的调用方 attach/read/close 一律 E_UNAUTHORIZED。
	foreign := request(t, conn, "e2e-rh-foreign", "terminal.attach", map[string]any{
		"client_id": "e2e-beta", "session_id": created.SessionID,
	})
	if foreign.Error == nil || foreign.Error.Code != pluginapi.E_UNAUTHORIZED {
		t.Fatalf("越权 attach 错误=%+v，期望 E_UNAUTHORIZED", foreign.Error)
	}
	t.Logf("E2E_RH_FOREIGN %s: %s", foreign.Error.Code, foreign.Error.Message)

	// 5) 命令历史：只记 write 输入，按会话分组，清理后为空。
	historyRaw := mustCall(t, conn, "e2e-rh-history", "terminal.history", map[string]any{"client_id": clientID})
	var history struct {
		Enabled bool `json:"enabled"`
		Count   int  `json:"count"`
		Entries []struct {
			SessionID string `json:"session_id"`
			Seq       uint64 `json:"seq"`
			Data      string `json:"data"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(historyRaw), &history); err != nil {
		t.Fatalf("解析 history 结果失败: %v", err)
	}
	if !history.Enabled || history.Count < 2 {
		t.Fatalf("history 未落盘: %s", historyRaw)
	}
	joined := ""
	for _, entry := range history.Entries {
		chunk, err := base64.StdEncoding.DecodeString(entry.Data)
		if err != nil {
			t.Fatalf("历史 data 不是合法 base64: %v", err)
		}
		if entry.SessionID != created.SessionID {
			t.Fatalf("历史会话归属不符: %+v", entry)
		}
		joined += string(chunk)
	}
	if !strings.Contains(joined, "kore-e2e-one") || !strings.Contains(joined, "kore-e2e-two") {
		t.Fatalf("历史内容未包含两次输入: %q", joined)
	}
	t.Logf("E2E_RH_HISTORY count=%d", history.Count)

	clearRaw := mustCall(t, conn, "e2e-rh-clear", "terminal.history.clear", map[string]any{"client_id": clientID})
	var cleared struct {
		ClearedEntries int `json:"cleared_entries"`
	}
	if err := json.Unmarshal([]byte(clearRaw), &cleared); err != nil {
		t.Fatalf("解析 history.clear 结果失败: %v", err)
	}
	if cleared.ClearedEntries != history.Count {
		t.Fatalf("清理条数=%d，期望 %d", cleared.ClearedEntries, history.Count)
	}
	after := mustCall(t, conn, "e2e-rh-history-after", "terminal.history", map[string]any{"client_id": clientID})
	if !strings.Contains(after, `"count":0`) {
		t.Fatalf("清理后历史未空: %s", after)
	}

	// 6) 收尾：关闭会话并优雅退出。
	mustCall(t, conn, "e2e-rh-close", "terminal.close", map[string]any{
		"client_id": clientID, "session_id": created.SessionID,
	})
	if _, err := mustCallOK(t, conn, "e2e-rh-shutdown", "kore.shutdown", map[string]any{}); err != nil {
		t.Fatalf("shutdown 失败: %v", err)
	}
}

// mustCallOK 调用并返回结果原文（要求成功）。
func mustCallOK(t *testing.T, conn net.Conn, id, method string, params any) (string, error) {
	t.Helper()
	response := request(t, conn, id, method, params)
	if response.Error != nil {
		return "", response.Error
	}
	return string(response.Result), nil
}

// TestIdleReclaimRejectsAttachE2E 断言超时回收后重连明确失败（会话不可恢复）。
func TestIdleReclaimRejectsAttachE2E(t *testing.T) {
	binary := buildPlugin(t)
	conn := startIsolatedPlugin(t, binary, "KORE_TERMINAL_IDLE_SEC=1")
	createdRaw := mustCall(t, conn, "e2e-idle-create", "terminal.create", map[string]any{"client_id": "e2e-idle"})
	var created sessionInfo
	if err := json.Unmarshal([]byte(createdRaw), &created); err != nil {
		t.Fatalf("解析 create 结果失败: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)

	response := request(t, conn, "e2e-idle-attach", "terminal.attach", map[string]any{
		"client_id": "e2e-idle", "session_id": created.SessionID,
	})
	if response.Error == nil || response.Error.Code != pluginapi.E_NOTFOUND {
		t.Fatalf("超时后 attach 错误=%+v，期望 E_NOTFOUND", response.Error)
	}
	t.Logf("E2E_IDLE_ATTACH_FAIL %s: %s", response.Error.Code, response.Error.Message)
	waitPidGone(t, created.PID, 5*time.Second)
	t.Logf("E2E_IDLE_PID_GONE pid=%d", created.PID)
}
