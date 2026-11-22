package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// requestTimeout 是单个请求的处理预算：用 context.WithTimeout，defer cancel 释放。
// 终端读写都是非阻塞/短等待，5s 足够；超时不代表会话被关闭。
const requestTimeout = 5 * time.Second

// createParams 是 terminal.create 入参：cols/rows 缺省 80×24，shell 缺省取配置。
type createParams struct {
	Cols  *int   `json:"cols"`
	Rows  *int   `json:"rows"`
	Shell string `json:"shell"`
}

// writeParams 是 terminal.write 入参；data 统一 base64（见 README「数据编码」）。
type writeParams struct {
	SessionID string `json:"session_id"`
	Data      string `json:"data"`
}

// resizeParams 是 terminal.resize 入参：cols/rows 必填。
type resizeParams struct {
	SessionID string `json:"session_id"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

// readParams 是 terminal.read 入参；max_bytes 缺省 64 KiB、上限 1 MiB。
type readParams struct {
	SessionID string `json:"session_id"`
	MaxBytes  *int   `json:"max_bytes"`
}

// closeParams 是 terminal.close 入参。
type closeParams struct {
	SessionID string `json:"session_id"`
}

// createResult 是 terminal.create 结果。
type createResult struct {
	SessionID string `json:"session_id"`
	Shell     string `json:"shell"`
	PID       int    `json:"pid"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

// writeResult 是 terminal.write 结果。
type writeResult struct {
	Written int `json:"written"`
}

// resizeResult 是 terminal.resize 结果，回显生效值。
type resizeResult struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// readResult 是 terminal.read 结果；data 为 base64，eof 表示 shell 已退出且输出已读完。
type readResult struct {
	Data  string `json:"data"`
	Bytes int    `json:"bytes"`
	EOF   bool   `json:"eof"`
}

// closeResult 是 terminal.close 结果。
type closeResult struct {
	Closed bool `json:"closed"`
}

// listResult 是 terminal.list 结果。
type listResult struct {
	Sessions    []terminal.Info `json:"sessions"`
	Count       int             `json:"count"`
	MaxSessions int             `json:"max_sessions"`
}

// dispatch 把请求分发为响应帧；第二个返回值表示收到 kore.shutdown。
func dispatch(request *pluginapi.Request, cfg config, manager *terminal.Manager, logger *slog.Logger) (*pluginapi.Response, bool) {
	response := &pluginapi.Response{Type: pluginapi.TypeResponse, ID: request.ID, ReqID: request.ReqID}
	switch request.Method {
	case "kore.shutdown":
		response.Result = json.RawMessage(`{"ok":true}`)
		logger.Info("收到 kore.shutdown，关闭全部终端会话")
		return response, true
	case "kore.ping":
		response.Result = encode(pingResult(cfg.startedAt))
		return response, false
	case "kore.reload":
		response.Result = json.RawMessage(`{"ok":true,"config_digest":""}`)
		return response, false
	}
	verb, ok := terminalVerb(request.Method, cfg.id)
	if !ok {
		response.Error = methodError(pluginapi.E_NOTFOUND, "未知方法: "+request.Method)
		return response, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if perr := handleTerminalVerb(ctx, verb, request.Params, manager, cfg, response); perr != nil {
		response.Error = perr
		response.Result = nil
	}
	return response, false
}

// terminalVerb 把 <id>.<verb> 或裸 <verb> 归一为动词名。
func terminalVerb(method, id string) (string, bool) {
	verb := strings.TrimPrefix(method, id+".")
	switch verb {
	case "create", "write", "resize", "read", "close", "list":
		return verb, true
	default:
		return "", false
	}
}

// handleTerminalVerb 执行终端方法，把内部错误映射为协议错误码。
func handleTerminalVerb(ctx context.Context, verb string, params json.RawMessage, manager *terminal.Manager, cfg config, response *pluginapi.Response) *pluginapi.Error {
	switch verb {
	case "create":
		return handleCreate(ctx, params, manager, cfg, response)
	case "write":
		return handleWrite(ctx, params, manager, response)
	case "resize":
		return handleResize(ctx, params, manager, response)
	case "read":
		return handleRead(ctx, params, manager, response)
	case "close":
		return handleClose(ctx, params, manager, response)
	case "list":
		var p struct{}
		if perr := decodeObject(params, &p); perr != nil {
			return perr
		}
		sessions := manager.List()
		if sessions == nil {
			sessions = []terminal.Info{}
		}
		response.Result = encode(listResult{Sessions: sessions, Count: len(sessions), MaxSessions: cfg.maxSessions})
		return nil
	default:
		return methodError(pluginapi.E_NOTFOUND, "未知方法: terminal."+verb)
	}
}

// handleCreate 处理 terminal.create。
func handleCreate(ctx context.Context, params json.RawMessage, manager *terminal.Manager, cfg config, response *pluginapi.Response) *pluginapi.Error {
	var p createParams
	if perr := decodeObject(params, &p); perr != nil {
		return perr
	}
	cols, rows := 80, 24
	if p.Cols != nil {
		cols = *p.Cols
	}
	if p.Rows != nil {
		rows = *p.Rows
	}
	info, err := manager.Create(ctx, p.Shell, cols, rows)
	if err != nil {
		return createError(err, cfg)
	}
	response.Result = encode(createResult{
		SessionID: info.SessionID, Shell: info.Shell, PID: info.PID, Cols: info.Cols, Rows: info.Rows,
	})
	return nil
}

// handleWrite 处理 terminal.write：data 为 base64，解码后写入 PTY 主端。
func handleWrite(ctx context.Context, params json.RawMessage, manager *terminal.Manager, response *pluginapi.Response) *pluginapi.Error {
	var p writeParams
	if perr := decodeObject(params, &p); perr != nil {
		return perr
	}
	if p.SessionID == "" {
		return methodError(pluginapi.E_INVALID, "session_id 必填")
	}
	data, err := base64.StdEncoding.DecodeString(p.Data)
	if err != nil {
		return methodError(pluginapi.E_INVALID, "data 必须是标准 base64: "+err.Error())
	}
	written, err := manager.Write(ctx, p.SessionID, data)
	if err != nil {
		perr := mapTerminalError(err)
		if written > 0 {
			perr.Message = fmt.Sprintf("%s（已写入 %d/%d 字节，请勿重复发送整段）", perr.Message, written, len(data))
		}
		return perr
	}
	response.Result = encode(writeResult{Written: written})
	return nil
}

// handleResize 处理 terminal.resize：TIOCSWINSZ 生效后回显 cols/rows。
func handleResize(ctx context.Context, params json.RawMessage, manager *terminal.Manager, response *pluginapi.Response) *pluginapi.Error {
	var p resizeParams
	if perr := decodeObject(params, &p); perr != nil {
		return perr
	}
	if p.SessionID == "" {
		return methodError(pluginapi.E_INVALID, "session_id 必填")
	}
	if err := manager.Resize(ctx, p.SessionID, p.Cols, p.Rows); err != nil {
		return mapTerminalError(err)
	}
	response.Result = encode(resizeResult{Cols: p.Cols, Rows: p.Rows})
	return nil
}

// handleRead 处理 terminal.read：增量读取，无数据返回空串（不是错误）。
func handleRead(ctx context.Context, params json.RawMessage, manager *terminal.Manager, response *pluginapi.Response) *pluginapi.Error {
	var p readParams
	if perr := decodeObject(params, &p); perr != nil {
		return perr
	}
	if p.SessionID == "" {
		return methodError(pluginapi.E_INVALID, "session_id 必填")
	}
	maxBytes := 0
	if p.MaxBytes != nil {
		if *p.MaxBytes < 0 {
			return methodError(pluginapi.E_INVALID, "max_bytes 不得为负数")
		}
		maxBytes = *p.MaxBytes
	}
	result, err := manager.Read(ctx, p.SessionID, maxBytes)
	if err != nil {
		return mapTerminalError(err)
	}
	response.Result = encode(readResult{
		Data:  base64.StdEncoding.EncodeToString(result.Data),
		Bytes: len(result.Data),
		EOF:   result.EOF,
	})
	return nil
}

// handleClose 处理 terminal.close：kill(-pgid) 后从 map 移除。
func handleClose(ctx context.Context, params json.RawMessage, manager *terminal.Manager, response *pluginapi.Response) *pluginapi.Error {
	var p closeParams
	if perr := decodeObject(params, &p); perr != nil {
		return perr
	}
	if p.SessionID == "" {
		return methodError(pluginapi.E_INVALID, "session_id 必填")
	}
	if err := manager.Close(ctx, p.SessionID); err != nil {
		return mapTerminalError(err)
	}
	response.Result = encode(closeResult{Closed: true})
	return nil
}

// createError 把 create 错误映射为协议错误；会话上限错误带上限值。
func createError(err error, cfg config) *pluginapi.Error {
	perr := mapTerminalError(err)
	if errors.Is(err, terminal.ErrRateLimit) {
		perr.Message = fmt.Sprintf("会话数已达上限 %d，请先关闭空闲会话", cfg.maxSessions)
	}
	return perr
}

// decodeObject 确保 raw 是 JSON 对象并反序列化到 dst。
func decodeObject(raw json.RawMessage, dst any) *pluginapi.Error {
	if len(raw) != 0 && string(raw) != "null" {
		if raw[0] != '{' {
			return methodError(pluginapi.E_INVALID, "params 必须是 JSON 对象")
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return methodError(pluginapi.E_INVALID, "params 解析失败: "+err.Error())
		}
	}
	return nil
}

// mapTerminalError 把终端内核哨兵错误映射为协议错误码。
func mapTerminalError(err error) *pluginapi.Error {
	switch {
	case errors.Is(err, terminal.ErrInvalid):
		return methodError(pluginapi.E_INVALID, err.Error())
	case errors.Is(err, terminal.ErrNotFound):
		return methodError(pluginapi.E_NOTFOUND, err.Error())
	case errors.Is(err, terminal.ErrRateLimit):
		return methodError(pluginapi.E_RATELIMIT, err.Error())
	case errors.Is(err, terminal.ErrTimeout):
		return methodError(pluginapi.E_UPSTREAM, err.Error())
	case errors.Is(err, terminal.ErrClosed):
		return methodError(pluginapi.E_CONFLICT, err.Error())
	case errors.Is(err, terminal.ErrUnsupported):
		return methodError(pluginapi.E_UNSUPPORTED, err.Error())
	case errors.Is(err, terminal.ErrIO), errors.Is(err, terminal.ErrResource):
		return methodError(pluginapi.E_UPSTREAM, err.Error())
	default:
		return methodError(pluginapi.E_INTERNAL, err.Error())
	}
}

// methodError 构造协议错误体。
func methodError(code string, msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: code, Message: msg}
}

// encode 序列化结构化结果为 RawMessage。
func encode(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}
