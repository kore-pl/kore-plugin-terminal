//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// newHistoryManager 构造带真实命令历史（临时目录）的管理器。
func newHistoryManager(t *testing.T, replayBytes int) *terminal.Manager {
	t.Helper()
	manager := terminal.New(terminal.Options{
		MaxSessions: 4,
		IdleTimeout: time.Minute,
		Shell:       "/bin/bash",
		ReplayBytes: replayBytes,
		History: terminal.HistoryOptions{
			Dir: t.TempDir(), Debounce: time.Nanosecond,
		},
	})
	t.Cleanup(manager.CloseAll)
	return manager
}

// mustSession 断言 create 成功并解析会话信息。
func mustSession(t *testing.T, cfg config, manager *terminal.Manager, clientID string) createResult {
	t.Helper()
	raw := encodeCall(t, cfg, manager, "terminal.create", map[string]any{"client_id": clientID, "cols": 100, "rows": 30})
	var created createResult
	if err := json.Unmarshal([]byte(raw), &created); err != nil {
		t.Fatalf("解析 create 结果失败: %v", err)
	}
	if created.SessionID == "" || created.ClientID != clientID {
		t.Fatalf("create 结果不符: %s", raw)
	}
	return created
}

// writeClient 以指定 client_id 写入原始文本。
func writeClient(t *testing.T, cfg config, manager *terminal.Manager, clientID, sessionID, text string) int {
	t.Helper()
	raw := encodeCall(t, cfg, manager, "terminal.write", map[string]any{
		"client_id": clientID, "session_id": sessionID,
		"data": base64.StdEncoding.EncodeToString([]byte(text)),
	})
	var written writeResult
	if err := json.Unmarshal([]byte(raw), &written); err != nil {
		t.Fatalf("解析 write 结果失败: %v", err)
	}
	return written.Written
}

// readClient 以指定 client_id（与可选游标）读取一次。
func readClient(t *testing.T, cfg config, manager *terminal.Manager, clientID, sessionID string, cursor *uint64) readResult {
	t.Helper()
	params := map[string]any{"client_id": clientID, "session_id": sessionID, "max_bytes": 64 << 10}
	if cursor != nil {
		params["cursor"] = *cursor
	}
	raw := encodeCall(t, cfg, manager, "terminal.read", params)
	var result readResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("解析 read 结果失败: %v", err)
	}
	return result
}

// TestDispatchAttachCursorRoundTrip 走协议面覆盖 断线 → attach → 从游标续读。
func TestDispatchAttachCursorRoundTrip(t *testing.T) {
	cfg := testConfig()
	manager := terminal.New(terminal.Options{MaxSessions: 4, IdleTimeout: time.Minute, Shell: "/bin/bash"})
	t.Cleanup(manager.CloseAll)

	created := mustSession(t, cfg, manager, "web-1")
	writeClient(t, cfg, manager, "web-1", created.SessionID, "printf 'kore-attach-%s\\n' ok\n")

	var cursor uint64
	var output strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		result := readClient(t, cfg, manager, "web-1", created.SessionID, nil)
		chunk, err := base64.StdEncoding.DecodeString(result.Data)
		if err != nil {
			t.Fatalf("read.data 不是合法 base64: %v", err)
		}
		output.Write(chunk)
		cursor = result.Cursor
		if strings.Contains(output.String(), "kore-attach-ok") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(output.String(), "kore-attach-ok") {
		t.Fatalf("未读到执行结果: %q", output.String())
	}

	rawAttach := encodeCall(t, cfg, manager, "terminal.attach", map[string]any{
		"client_id": "web-1", "session_id": created.SessionID, "cursor": cursor,
	})
	var attached attachResult
	if err := json.Unmarshal([]byte(rawAttach), &attached); err != nil {
		t.Fatalf("解析 attach 结果失败: %v", err)
	}
	if !attached.Attached || attached.SessionID != created.SessionID || attached.ClientID != "web-1" {
		t.Fatalf("attach 结果不符: %s", rawAttach)
	}
	if attached.Cursor < cursor || attached.ReplayBytes <= 0 || attached.IdleExpiresAt == "" {
		t.Fatalf("attach 游标/存活语义不符: %s", rawAttach)
	}

	// 模拟"服务器已读出但响应在断线中丢失"：先用增量读把新输出消费掉，
	// 再用旧游标续读——必须回放同一段字节，且不重复吐更早的已读内容。
	writeClient(t, cfg, manager, "web-1", created.SessionID, "printf 'kore-resume-%s\\n' ok\n")
	var phaseTwo strings.Builder
	var resumedCursor uint64
	deadline = time.Now().Add(8 * time.Second)
	quietSince := time.Now()
	for time.Now().Before(deadline) {
		result := readClient(t, cfg, manager, "web-1", created.SessionID, nil)
		decoded, err := base64.StdEncoding.DecodeString(result.Data)
		if err != nil {
			t.Fatalf("read.data 不是合法 base64: %v", err)
		}
		if result.Bytes > 0 {
			phaseTwo.Write(decoded)
			resumedCursor = result.Cursor
			quietSince = time.Now()
			continue
		}
		if time.Since(quietSince) >= 800*time.Millisecond {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(phaseTwo.String(), "kore-resume-ok") || resumedCursor <= cursor {
		t.Fatalf("掉线期间输出/游标不符: cursor=%d resumed=%d output=%q", cursor, resumedCursor, phaseTwo.String())
	}

	replayed := readClient(t, cfg, manager, "web-1", created.SessionID, &cursor)
	decodedReplay, err := base64.StdEncoding.DecodeString(replayed.Data)
	if err != nil {
		t.Fatalf("续读 data 不是合法 base64: %v", err)
	}
	if !replayed.Replay || replayed.Cursor != resumedCursor {
		t.Fatalf("续读应回放且游标对齐: %+v", replayed)
	}
	if string(decodedReplay) != phaseTwo.String() {
		t.Fatalf("续读字节=%q，期望 %q", decodedReplay, phaseTwo.String())
	}
	// 对齐到最新游标后读到空串，不重复吐已读内容。
	tail := readClient(t, cfg, manager, "web-1", created.SessionID, &resumedCursor)
	if tail.Bytes != 0 || tail.Replay {
		t.Fatalf("对齐游标后应读到空串: %+v", tail)
	}
}

// TestDispatchForbiddenByClient 断言跨调用方读写/attach/close 全部 E_UNAUTHORIZED。
func TestDispatchForbiddenByClient(t *testing.T) {
	cfg := testConfig()
	manager := terminal.New(terminal.Options{MaxSessions: 2, IdleTimeout: time.Minute, Shell: "/bin/bash"})
	t.Cleanup(manager.CloseAll)
	created := mustSession(t, cfg, manager, "alice")

	cases := []struct {
		name   string
		method string
		params map[string]any
	}{
		{"越权 write", "terminal.write", map[string]any{"client_id": "bob", "session_id": created.SessionID, "data": "eA=="}},
		{"越权 read", "terminal.read", map[string]any{"client_id": "bob", "session_id": created.SessionID}},
		{"越权 attach", "terminal.attach", map[string]any{"client_id": "bob", "session_id": created.SessionID}},
		{"越权 resize", "terminal.resize", map[string]any{"client_id": "bob", "session_id": created.SessionID, "cols": 90, "rows": 30}},
		{"越权 close", "terminal.close", map[string]any{"client_id": "bob", "session_id": created.SessionID}},
	}
	for _, c := range cases {
		raw, err := json.Marshal(c.params)
		if err != nil {
			t.Fatalf("序列化参数失败: %v", err)
		}
		response := dispatchCall(t, cfg, manager, c.method, string(raw))
		if response.Error == nil || response.Error.Code != pluginapi.E_UNAUTHORIZED {
			t.Fatalf("%s 错误=%+v，期望 E_UNAUTHORIZED", c.name, response.Error)
		}
	}
	// 越权失败后归属方会话仍在。
	listed := encodeCall(t, cfg, manager, "terminal.list", map[string]any{"client_id": "alice"})
	if !strings.Contains(listed, `"count":1`) || !strings.Contains(listed, created.SessionID) {
		t.Fatalf("越权后归属方列表不符: %s", listed)
	}
	if err := json.Unmarshal([]byte(encodeCall(t, cfg, manager, "terminal.close", map[string]any{
		"client_id": "alice", "session_id": created.SessionID,
	})), &closeResult{}); err != nil {
		t.Fatalf("归属方 close 失败: %v", err)
	}
}

// TestDispatchListFilter 断言 list 缺省为管理视图，带 client_id 时按归属过滤。
func TestDispatchListFilter(t *testing.T) {
	cfg := testConfig()
	manager := terminal.New(terminal.Options{MaxSessions: 4, IdleTimeout: time.Minute, Shell: "/bin/bash"})
	t.Cleanup(manager.CloseAll)
	first := mustSession(t, cfg, manager, "alice")
	second := mustSession(t, cfg, manager, "bob")

	all := encodeCall(t, cfg, manager, "terminal.list", map[string]any{})
	if !strings.Contains(all, `"count":2`) {
		t.Fatalf("管理视图 count 不符: %s", all)
	}
	onlyAlice := encodeCall(t, cfg, manager, "terminal.list", map[string]any{"client_id": "alice"})
	if !strings.Contains(onlyAlice, `"count":1`) || !strings.Contains(onlyAlice, first.SessionID) ||
		strings.Contains(onlyAlice, second.SessionID) {
		t.Fatalf("归属性过滤不符: %s", onlyAlice)
	}
	bad := dispatchCall(t, cfg, manager, "terminal.list", `{"client_id":"bad id"}`)
	if bad.Error == nil || bad.Error.Code != pluginapi.E_INVALID {
		t.Fatalf("非法 client_id 错误=%+v，期望 E_INVALID", bad.Error)
	}
}

// TestDispatchHistoryRoundTrip 覆盖 写入 → 查询 → 越权拒绝 → 清理 的协议面往返。
func TestDispatchHistoryRoundTrip(t *testing.T) {
	cfg := testConfig()
	manager := newHistoryManager(t, 0)
	created := mustSession(t, cfg, manager, "hist")

	input := "mysql -u root -p*** -e 'select 1'\n"
	if written := writeClient(t, cfg, manager, "hist", created.SessionID, input); written != len(input) {
		t.Fatalf("write written=%d，期望 %d", written, len(input))
	}

	raw := encodeCall(t, cfg, manager, "terminal.history", map[string]any{"client_id": "hist"})
	var history historyResult
	if err := json.Unmarshal([]byte(raw), &history); err != nil {
		t.Fatalf("解析 history 结果失败: %v", err)
	}
	if !history.Enabled || history.Count != 1 || len(history.Entries) != 1 {
		t.Fatalf("history 结果不符: %s", raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(history.Entries[0].Data)
	if err != nil || string(decoded) != input {
		t.Fatalf("历史内容=%q err=%v，期望 %q", decoded, err, input)
	}
	if history.Entries[0].SessionID != created.SessionID || history.Entries[0].Seq != 1 {
		t.Fatalf("历史条目归属/序号不符: %+v", history.Entries[0])
	}

	// 越权：别的调用方查询/清理该会话历史一律拒绝。
	denied := dispatchCall(t, cfg, manager, "terminal.history", `{"client_id":"eve","session_id":"`+created.SessionID+`"}`)
	if denied.Error == nil || denied.Error.Code != pluginapi.E_UNAUTHORIZED {
		t.Fatalf("越权查询错误=%+v，期望 E_UNAUTHORIZED", denied.Error)
	}
	deniedClear := dispatchCall(t, cfg, manager, "terminal.history.clear", `{"client_id":"eve","session_id":"`+created.SessionID+`"}`)
	if deniedClear.Error == nil || deniedClear.Error.Code != pluginapi.E_UNAUTHORIZED {
		t.Fatalf("越权清理错误=%+v，期望 E_UNAUTHORIZED", deniedClear.Error)
	}

	// 清理后查询为空。
	cleared := encodeCall(t, cfg, manager, "terminal.history.clear", map[string]any{"client_id": "hist"})
	var clearResult historyClearResult
	if err := json.Unmarshal([]byte(cleared), &clearResult); err != nil {
		t.Fatalf("解析 clear 结果失败: %v", err)
	}
	if clearResult.ClearedSessions != 1 || clearResult.ClearedEntries != 1 {
		t.Fatalf("清理结果不符: %s", cleared)
	}
	empty := encodeCall(t, cfg, manager, "terminal.history", map[string]any{"client_id": "hist"})
	if !strings.Contains(empty, `"count":0`) {
		t.Fatalf("清理后历史未空: %s", empty)
	}

	// 参数校验。
	for _, c := range []struct{ name, params string }{
		{"负 limit", `{"client_id":"hist","limit":-1}`},
		{"非法 client_id", `{"client_id":"bad id"}`},
	} {
		response := dispatchCall(t, cfg, manager, "terminal.history", c.params)
		if response.Error == nil || response.Error.Code != pluginapi.E_INVALID {
			t.Fatalf("%s 错误=%+v，期望 E_INVALID", c.name, response.Error)
		}
	}
}

// TestDispatchCursorErrors 覆盖游标超前（E_INVALID）与游标过期（E_CONFLICT）。
func TestDispatchCursorErrors(t *testing.T) {
	cfg := testConfig()
	manager := newHistoryManager(t, 8)
	created := mustSession(t, cfg, manager, "cursor")

	ahead := uint64(1 << 20)
	raw, _ := json.Marshal(map[string]any{"client_id": "cursor", "session_id": created.SessionID, "cursor": ahead})
	if response := dispatchCall(t, cfg, manager, "terminal.read", string(raw)); response.Error == nil || response.Error.Code != pluginapi.E_INVALID {
		t.Fatalf("游标超前 read 错误=%+v，期望 E_INVALID", response.Error)
	}
	if response := dispatchCall(t, cfg, manager, "terminal.attach", string(raw)); response.Error == nil || response.Error.Code != pluginapi.E_INVALID {
		t.Fatalf("游标超前 attach 错误=%+v，期望 E_INVALID", response.Error)
	}

	writeClient(t, cfg, manager, "cursor", created.SessionID, "printf '0123456789abcdef\\n'\n")
	deadline := time.Now().Add(5 * time.Second)
	var cursor uint64
	for time.Now().Before(deadline) {
		result := readClient(t, cfg, manager, "cursor", created.SessionID, nil)
		if result.Bytes > 0 {
			cursor = result.Cursor
			continue
		}
		if cursor > 8 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stale := uint64(0)
	rawStale, _ := json.Marshal(map[string]any{"client_id": "cursor", "session_id": created.SessionID, "cursor": stale})
	if response := dispatchCall(t, cfg, manager, "terminal.read", string(rawStale)); response.Error == nil || response.Error.Code != pluginapi.E_CONFLICT {
		t.Fatalf("游标过期 read 错误=%+v，期望 E_CONFLICT", response.Error)
	}
}

// TestDispatchSessionLimitMessage 断言并发上限的错误码与提示带上限值（安全红线可配）。
func TestDispatchSessionLimitMessage(t *testing.T) {
	cfg := testConfig()
	cfg.maxSessions = 1
	manager := terminal.New(terminal.Options{MaxSessions: 1, IdleTimeout: time.Minute, Shell: "/bin/bash"})
	t.Cleanup(manager.CloseAll)
	mustSession(t, cfg, manager, "limit")
	response := dispatchCall(t, cfg, manager, "terminal.create", `{"client_id":"limit"}`)
	if response.Error == nil || response.Error.Code != pluginapi.E_RATELIMIT {
		t.Fatalf("上限错误=%+v，期望 E_RATELIMIT", response.Error)
	}
	if !strings.Contains(response.Error.Message, "上限 1") {
		t.Fatalf("上限消息未带上限值: %q", response.Error.Message)
	}
}
