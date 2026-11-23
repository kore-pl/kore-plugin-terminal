package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// testConfig 构造握手与会话循环共用的一致配置（不依赖环境变量）。
func testConfig() config {
	return config{
		id:           "terminal",
		version:      "0.2.0",
		requiresKore: ">=0.1.0 <0.2.0",
		capabilities: []string{"system:read", "system:admin", "proc:write"},
		startedAt:    time.Now(),
		maxSessions:  4,
		idleTimeout:  time.Minute,
		defaultShell: "/bin/bash",
	}
}

// testLogger 丢弃日志，测试输出只保留断言信息。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// readFrame 读取并解码一帧。
func readFrame(t *testing.T, conn net.Conn) any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	payload, err := pluginapi.ReadFrame(conn, pluginapi.MaxFrameSize)
	if err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	env, err := pluginapi.DecodeFrame(payload)
	if err != nil {
		t.Fatalf("解码帧失败: %v", err)
	}
	return env
}

// readRequest 断言下一帧是 request。
func readRequest(t *testing.T, conn net.Conn) *pluginapi.Request {
	t.Helper()
	request, ok := readFrame(t, conn).(*pluginapi.Request)
	if !ok {
		t.Fatal("帧类型不是 request")
	}
	return request
}

// writeResponse 写一个成功响应帧。
func writeResponse(t *testing.T, conn net.Conn, id, reqID string, result json.RawMessage) {
	t.Helper()
	if err := pluginapi.WriteJSON(conn, &pluginapi.Response{
		Type: pluginapi.TypeResponse, ID: id, ReqID: reqID, Result: result,
	}); err != nil {
		t.Fatalf("写响应失败: %v", err)
	}
}

// TestHandshake 校验 hello 首帧字段与三步握手。
func TestHandshake(t *testing.T) {
	pluginConn, kernelConn := net.Pipe()
	defer func() { _ = kernelConn.Close() }()
	done := make(chan error, 1)
	cfg := testConfig()
	go func() { done <- handshake(pluginConn, cfg); _ = pluginConn.Close() }()

	request := readRequest(t, kernelConn)
	if request.Method != helloMethod {
		t.Fatalf("首帧方法=%q，期望 %q", request.Method, helloMethod)
	}
	var params helloParams
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatalf("解析 hello 参数失败: %v", err)
	}
	if params.PluginID != "terminal" || params.ID != "terminal" {
		t.Fatalf("plugin_id/id=%q/%q，期望 terminal", params.PluginID, params.ID)
	}
	if params.Protocol != pluginapi.Protocol || params.Version != "0.2.0" {
		t.Fatalf("protocol/version=%q/%q 不符合 §5", params.Protocol, params.Version)
	}
	if params.PID != os.Getpid() {
		t.Fatalf("pid=%d，期望 %d", params.PID, os.Getpid())
	}
	if len(params.Capabilities) != 3 || params.Capabilities[0] != "system:read" {
		t.Fatalf("capabilities 不符: %+v", params.Capabilities)
	}
	result, err := json.Marshal(helloResult{OK: true, Protocol: pluginapi.Protocol})
	if err != nil {
		t.Fatalf("序列化握手响应失败: %v", err)
	}
	writeResponse(t, kernelConn, request.ID, request.ReqID, result)
	if err := <-done; err != nil {
		t.Fatalf("握手返回错误: %v", err)
	}
}

// TestServeShutdown 会话循环收到 kore.shutdown 后优雅退出。
func TestServeShutdown(t *testing.T) {
	pluginConn, kernelConn := net.Pipe()
	defer func() { _ = kernelConn.Close() }()
	manager := terminal.New(terminal.Options{MaxSessions: 2, IdleTimeout: time.Minute, Shell: "/bin/bash"})
	defer manager.CloseAll()
	done := make(chan error, 1)
	go func() { done <- serve(pluginConn, testConfig(), manager, testLogger()); _ = pluginConn.Close() }()

	request := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: "id-1", ReqID: "r-1", Method: "kore.shutdown",
	}
	if err := pluginapi.WriteJSON(kernelConn, request); err != nil {
		t.Fatalf("写请求失败: %v", err)
	}
	env := readFrame(t, kernelConn)
	response, ok := env.(*pluginapi.Response)
	if !ok || response.ID != "id-1" {
		t.Fatalf("响应帧异常: %+v", env)
	}
	if string(response.Result) != `{"ok":true}` {
		t.Fatalf("shutdown 结果=%s", response.Result)
	}
	if err := <-done; err != nil {
		t.Fatalf("serve 返回错误: %v", err)
	}
}

// TestUnknownMethod 断言未知方法返回 E_NOTFOUND 且不影响后续调用。
func TestUnknownMethod(t *testing.T) {
	request := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: "id", ReqID: "r", Method: "terminal.exec",
	}
	response, shutdown := dispatch(request, testConfig(), terminal.New(terminal.Options{}), testLogger())
	if shutdown {
		t.Fatal("未知方法不应触发 shutdown")
	}
	if response.Error == nil || response.Error.Code != pluginapi.E_NOTFOUND {
		t.Fatalf("未知方法错误=%+v，期望 E_NOTFOUND", response.Error)
	}
}
