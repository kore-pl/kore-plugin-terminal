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

// dispatchCall 把方法调用打到 dispatch 并返回响应。
func dispatchCall(t *testing.T, cfg config, manager *terminal.Manager, method, params string) *pluginapi.Response {
	t.Helper()
	request := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: "id", ReqID: "r",
		Method: method, Params: json.RawMessage(params),
	}
	response, shutdown := dispatch(request, cfg, manager, testLogger())
	if shutdown {
		t.Fatalf("%s 不应触发 shutdown", method)
	}
	return response
}

// encodeCall 把参数序列化并做一次调用，要求成功并返回 result 原文。
func encodeCall(t *testing.T, cfg config, manager *terminal.Manager, method string, params any) string {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("序列化 %s 参数失败: %v", method, err)
	}
	response := dispatchCall(t, cfg, manager, method, string(raw))
	if response.Error != nil {
		t.Fatalf("%s 意外错误: %+v", method, response.Error)
	}
	return string(response.Result)
}

// TestDispatchTerminalRoundTrip 走真实 PTY：create → write(base64) → read 到标记 → resize → list → close。
func TestDispatchTerminalRoundTrip(t *testing.T) {
	cfg := testConfig()
	manager := terminal.New(terminal.Options{MaxSessions: 4, IdleTimeout: time.Minute, Shell: "/bin/bash"})
	t.Cleanup(manager.CloseAll)

	created := encodeCall(t, cfg, manager, "terminal.create", map[string]any{"cols": 100, "rows": 30})
	var create struct {
		SessionID string `json:"session_id"`
		Shell     string `json:"shell"`
		PID       int    `json:"pid"`
		Cols      int    `json:"cols"`
		Rows      int    `json:"rows"`
	}
	if err := json.Unmarshal([]byte(created), &create); err != nil {
		t.Fatalf("解析 create 结果失败: %v", err)
	}
	if create.SessionID == "" || create.PID <= 0 || create.Cols != 100 || create.Rows != 30 {
		t.Fatalf("create 结果不符: %s", created)
	}

	payload := base64.StdEncoding.EncodeToString([]byte("printf 'kore-method-%s\\n' ok\n"))
	written := encodeCall(t, cfg, manager, "terminal.write", map[string]any{"session_id": create.SessionID, "data": payload})
	if !strings.Contains(written, `"written":`) {
		t.Fatalf("write 结果不符: %s", written)
	}

	deadline := time.Now().Add(5 * time.Second)
	var output strings.Builder
	for time.Now().Before(deadline) {
		raw := encodeCall(t, cfg, manager, "terminal.read", map[string]any{"session_id": create.SessionID, "max_bytes": 65536})
		var read struct {
			Data  string `json:"data"`
			Bytes int    `json:"bytes"`
			EOF   bool   `json:"eof"`
		}
		if err := json.Unmarshal([]byte(raw), &read); err != nil {
			t.Fatalf("解析 read 结果失败: %v", err)
		}
		chunk, err := base64.StdEncoding.DecodeString(read.Data)
		if err != nil {
			t.Fatalf("read.data 不是合法 base64: %v", err)
		}
		if read.Bytes != len(chunk) {
			t.Fatalf("bytes=%d 与解码长度 %d 不一致", read.Bytes, len(chunk))
		}
		output.Write(chunk)
		if strings.Contains(output.String(), "kore-method-ok") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(output.String(), "kore-method-ok") {
		t.Fatalf("未读到执行结果，输出=%q", output.String())
	}

	resized := encodeCall(t, cfg, manager, "terminal.resize", map[string]any{"session_id": create.SessionID, "cols": 120, "rows": 40})
	if !strings.Contains(resized, `"cols":120`) || !strings.Contains(resized, `"rows":40`) {
		t.Fatalf("resize 结果不符: %s", resized)
	}

	listed := encodeCall(t, cfg, manager, "terminal.list", map[string]any{})
	if !strings.Contains(listed, `"count":1`) || !strings.Contains(listed, create.SessionID) {
		t.Fatalf("list 结果不符: %s", listed)
	}

	closed := encodeCall(t, cfg, manager, "terminal.close", map[string]any{"session_id": create.SessionID})
	if !strings.Contains(closed, `"closed":true`) {
		t.Fatalf("close 结果不符: %s", closed)
	}
	empty := encodeCall(t, cfg, manager, "terminal.list", map[string]any{})
	if !strings.Contains(empty, `"count":0`) {
		t.Fatalf("close 后 list 结果不符: %s", empty)
	}
}

// TestDispatchRejects 覆盖参数校验与错误码映射。
func TestDispatchRejects(t *testing.T) {
	cfg := testConfig()
	manager := terminal.New(terminal.Options{MaxSessions: 2, IdleTimeout: time.Minute, Shell: "/bin/bash"})
	t.Cleanup(manager.CloseAll)

	cases := []struct {
		name   string
		method string
		params string
		code   string
	}{
		{"坏 base64", "terminal.write", `{"session_id":"t-x","data":"%%%"}`, pluginapi.E_INVALID},
		{"缺 session_id", "terminal.write", `{"data":"eA=="}`, pluginapi.E_INVALID},
		{"缺 session_id read", "terminal.read", `{}`, pluginapi.E_INVALID},
		{"负 max_bytes", "terminal.read", `{"session_id":"t-x","max_bytes":-1}`, pluginapi.E_INVALID},
		{"非法尺寸", "terminal.resize", `{"session_id":"t-x","cols":0,"rows":24}`, pluginapi.E_INVALID},
		{"未知会话", "terminal.read", `{"session_id":"t-missing"}`, pluginapi.E_NOTFOUND},
		{"非对象 params", "terminal.list", `[]`, pluginapi.E_INVALID},
		{"未知方法", "terminal.exec", `{}`, pluginapi.E_NOTFOUND},
	}
	for _, c := range cases {
		response := dispatchCall(t, cfg, manager, c.method, c.params)
		if response.Error == nil || response.Error.Code != c.code {
			t.Fatalf("%s 错误=%+v，期望 %s", c.name, response.Error, c.code)
		}
		if response.Result != nil {
			t.Fatalf("%s 出错时不应带结果", c.name)
		}
	}
}

// TestDispatchSessionLimit 断言超过上限回 E_RATELIMIT 且消息带上限值。
func TestDispatchSessionLimit(t *testing.T) {
	cfg := testConfig()
	cfg.maxSessions = 1
	manager := terminal.New(terminal.Options{MaxSessions: 1, IdleTimeout: time.Minute, Shell: "/bin/bash"})
	t.Cleanup(manager.CloseAll)

	encodeCall(t, cfg, manager, "terminal.create", map[string]any{})
	response := dispatchCall(t, cfg, manager, "terminal.create", `{}`)
	if response.Error == nil || response.Error.Code != pluginapi.E_RATELIMIT {
		t.Fatalf("上限错误=%+v，期望 E_RATELIMIT", response.Error)
	}
	if !strings.Contains(response.Error.Message, "上限 1") {
		t.Fatalf("上限错误消息未带上限值: %q", response.Error.Message)
	}
}

// TestPingResult 断言 kore.ping 返回 ready、goroutine 与 rss 指标。
func TestPingResult(t *testing.T) {
	cfg := testConfig()
	manager := terminal.New(terminal.Options{})
	response := dispatchCall(t, cfg, manager, "kore.ping", `{}`)
	if response.Error != nil {
		t.Fatalf("ping 失败: %+v", response.Error)
	}
	var ping statusResult
	if err := json.Unmarshal(response.Result, &ping); err != nil {
		t.Fatalf("解析 ping 结果失败: %v", err)
	}
	if ping.Status != "ready" || ping.Goroutines <= 0 {
		t.Fatalf("ping 结果不符: %+v", ping)
	}
}
