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

// terminalVerbs 是全部终端方法动词（capability 声明与实现一致性的唯一真源）。
// 新增方法必须同时出现在这里、info.json.method_capabilities 与 dispatch 分支。
var terminalVerbs = []string{
	"create", "write", "resize", "read", "close", "list",
	"attach", "history", "history.clear",
}

// createParams 是 terminal.create 入参：cols/rows 缺省 80×24，shell 缺省取配置，
// client_id 是创建者标识（缺省 default，会话归属由此绑定）。
type createParams struct {
	ClientID string `json:"client_id"`
	Cols     *int   `json:"cols"`
	Rows     *int   `json:"rows"`
	Shell    string `json:"shell"`
}

// writeParams 是 terminal.write 入参；data 统一 base64（见 README「数据编码」）。
type writeParams struct {
	SessionID string `json:"session_id"`
	ClientID  string `json:"client_id"`
	Data      string `json:"data"`
}

// resizeParams 是 terminal.resize 入参：cols/rows 必填。
type resizeParams struct {
	SessionID string `json:"session_id"`
	ClientID  string `json:"client_id"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

// readParams 是 terminal.read 入参；cursor 是已消费偏移（向后兼容：缺省沿用增量读），
// max_bytes 缺省 64 KiB、上限 1 MiB。
type readParams struct {
	SessionID string  `json:"session_id"`
	ClientID  string  `json:"client_id"`
	Cursor    *uint64 `json:"cursor"`
	MaxBytes  *int    `json:"max_bytes"`
}

// closeParams 是 terminal.close 入参。
type closeParams struct {
	SessionID string `json:"session_id"`
	ClientID  string `json:"client_id"`
}

// attachParams 是 terminal.attach 入参；cursor 为客户端已消费偏移（可选）。
type attachParams struct {
	SessionID string  `json:"session_id"`
	ClientID  string  `json:"client_id"`
	Cursor    *uint64 `json:"cursor"`
}

// historyParams 是 terminal.history 入参；session_id 为空时返回该调用方全部会话历史。
type historyParams struct {
	SessionID string `json:"session_id"`
	ClientID  string `json:"client_id"`
	Limit     *int   `json:"limit"`
}

// historyClearParams 是 terminal.history.clear 入参。
type historyClearParams struct {
	SessionID string `json:"session_id"`
	ClientID  string `json:"client_id"`
}

// createResult 是 terminal.create 结果。
type createResult struct {
	SessionID string `json:"session_id"`
	ClientID  string `json:"client_id"`
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

// readResult 是 terminal.read 结果；data 为 base64，cursor 是下次续读游标。
type readResult struct {
	Data   string `json:"data"`
	Bytes  int    `json:"bytes"`
	Cursor uint64 `json:"cursor"`
	Replay bool   `json:"replay"`
	EOF    bool   `json:"eof"`
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

// attachResult 是 terminal.attach 结果：会话快照 + 游标可恢复信息。
type attachResult struct {
	SessionID     string `json:"session_id"`
	ClientID      string `json:"client_id"`
	Attached      bool   `json:"attached"`
	Cursor        uint64 `json:"cursor"`
	ReplayBytes   int    `json:"replay_bytes"`
	EOF           bool   `json:"eof"`
	Shell         string `json:"shell"`
	PID           int    `json:"pid"`
	Cols          int    `json:"cols"`
	Rows          int    `json:"rows"`
	CreatedAt     string `json:"created_at"`
	LastActive    string `json:"last_active"`
	IdleExpiresAt string `json:"idle_expires_at"`
}

// historyEntryResult 是命令历史条目（data 统一 base64）。
type historyEntryResult struct {
	SessionID     string `json:"session_id"`
	Seq           uint64 `json:"seq"`
	At            string `json:"at"`
	Data          string `json:"data"`
	Bytes         int    `json:"bytes"`
	OriginalBytes int    `json:"original_bytes"`
	Truncated     bool   `json:"truncated"`
}

// historyResult 是 terminal.history 结果。
type historyResult struct {
	Enabled      bool                 `json:"enabled"`
	ClientID     string               `json:"client_id"`
	SessionID    string               `json:"session_id,omitempty"`
	Entries      []historyEntryResult `json:"entries"`
	Count        int                  `json:"count"`
	Limit        int                  `json:"limit"`
	MaxEntries   int                  `json:"max_entries"`
	StorageError string               `json:"storage_error,omitempty"`
}

// historyClearResult 是 terminal.history.clear 结果。
type historyClearResult struct {
	ClearedSessions int `json:"cleared_sessions"`
	ClearedEntries  int `json:"cleared_entries"`
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
	for _, known := range terminalVerbs {
		if verb == known {
			return verb, true
		}
	}
	return "", false
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
	case "attach":
		return handleAttach(ctx, params, manager, response)
	case "close":
		return handleClose(ctx, params, manager, response)
	case "history":
		return handleHistory(params, manager, response)
	case "history.clear":
		return handleHistoryClear(params, manager, response)
	case "list":
		return handleList(params, manager, cfg, response)
	default:
		return methodError(pluginapi.E_NOTFOUND, "未知方法: terminal."+verb)
	}
}

// handleCreate 处理 terminal.create：绑定 client_id 创建会话。
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
	info, err := manager.Create(ctx, p.ClientID, p.Shell, cols, rows)
	if err != nil {
		return createError(err, cfg)
	}
	response.Result = encode(createResult{
		SessionID: info.SessionID, ClientID: info.ClientID, Shell: info.Shell,
		PID: info.PID, Cols: info.Cols, Rows: info.Rows,
	})
	return nil
}

// handleWrite 处理 terminal.write：data 为 base64，解码后写入 PTY 主端并记入历史。
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
	written, err := manager.Write(ctx, p.ClientID, p.SessionID, data)
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
	if err := manager.Resize(ctx, p.ClientID, p.SessionID, p.Cols, p.Rows); err != nil {
		return mapTerminalError(err)
	}
	response.Result = encode(resizeResult{Cols: p.Cols, Rows: p.Rows})
	return nil
}

// handleRead 处理 terminal.read：cursor 缺省为增量读（旧行为），带游标则从游标续读。
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
	result, err := manager.Read(ctx, p.ClientID, p.SessionID, p.Cursor, maxBytes)
	if err != nil {
		return mapTerminalError(err)
	}
	response.Result = encode(readResult{
		Data:   base64.StdEncoding.EncodeToString(result.Data),
		Bytes:  len(result.Data),
		Cursor: result.Cursor,
		Replay: result.Replay,
		EOF:    result.EOF,
	})
	return nil
}

// handleAttach 处理 terminal.attach：重连存活会话并校验读游标可恢复性。
func handleAttach(ctx context.Context, params json.RawMessage, manager *terminal.Manager, response *pluginapi.Response) *pluginapi.Error {
	var p attachParams
	if perr := decodeObject(params, &p); perr != nil {
		return perr
	}
	if p.SessionID == "" {
		return methodError(pluginapi.E_INVALID, "session_id 必填")
	}
	attached, err := manager.Attach(ctx, p.ClientID, p.SessionID, p.Cursor)
	if err != nil {
		return mapTerminalError(err)
	}
	response.Result = encode(attachResult{
		SessionID: attached.Info.SessionID, ClientID: attached.Info.ClientID, Attached: true,
		Cursor: attached.Cursor, ReplayBytes: attached.ReplayBytes, EOF: attached.Info.EOF,
		Shell: attached.Info.Shell, PID: attached.Info.PID, Cols: attached.Info.Cols, Rows: attached.Info.Rows,
		CreatedAt: attached.Info.CreatedAt, LastActive: attached.Info.LastActive,
		IdleExpiresAt: attached.IdleExpiresAt,
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
	if err := manager.Close(ctx, p.ClientID, p.SessionID); err != nil {
		return mapTerminalError(err)
	}
	response.Result = encode(closeResult{Closed: true})
	return nil
}

// handleList 处理 terminal.list：client_id 缺省为管理视图（全部会话），非空时按归属过滤。
func handleList(params json.RawMessage, manager *terminal.Manager, cfg config, response *pluginapi.Response) *pluginapi.Error {
	var p struct {
		ClientID string `json:"client_id"`
	}
	if perr := decodeObject(params, &p); perr != nil {
		return perr
	}
	filter := ""
	if strings.TrimSpace(p.ClientID) != "" {
		normalized, err := terminal.NormalizeClientID(p.ClientID)
		if err != nil {
			return mapTerminalError(err)
		}
		filter = normalized
	}
	sessions := manager.List(filter)
	if sessions == nil {
		sessions = []terminal.Info{}
	}
	response.Result = encode(listResult{Sessions: sessions, Count: len(sessions), MaxSessions: cfg.maxSessions})
	return nil
}

// handleHistory 处理 terminal.history：按会话分组返回该调用方的输入历史。
func handleHistory(params json.RawMessage, manager *terminal.Manager, response *pluginapi.Response) *pluginapi.Error {
	var p historyParams
	if perr := decodeObject(params, &p); perr != nil {
		return perr
	}
	owner, perr := normalizeClientIDParam(p.ClientID)
	if perr != nil {
		return perr
	}
	limit := 0
	if p.Limit != nil {
		if *p.Limit < 0 {
			return methodError(pluginapi.E_INVALID, "limit 不得为负数")
		}
		limit = *p.Limit
	}
	history := manager.History()
	entries, err := history.Query(owner, p.SessionID, limit)
	if err != nil {
		return mapTerminalError(err)
	}
	_, storageErr := history.Status()
	results := make([]historyEntryResult, 0, len(entries))
	for _, entry := range entries {
		results = append(results, historyEntryResult{
			SessionID: entry.SessionID, Seq: entry.Seq, At: entry.At.Format(time.RFC3339Nano),
			Data:  base64.StdEncoding.EncodeToString(entry.Data),
			Bytes: len(entry.Data), OriginalBytes: entry.OriginalBytes, Truncated: entry.Truncated,
		})
	}
	response.Result = encode(historyResult{
		Enabled: history.Enabled(), ClientID: owner, SessionID: p.SessionID,
		Entries: results, Count: len(results), Limit: resolvedHistoryLimit(limit),
		MaxEntries: history.Options().MaxEntries, StorageError: storageErr,
	})
	return nil
}

// handleHistoryClear 处理 terminal.history.clear：清理该调用方的历史（按会话或全部）。
func handleHistoryClear(params json.RawMessage, manager *terminal.Manager, response *pluginapi.Response) *pluginapi.Error {
	var p historyClearParams
	if perr := decodeObject(params, &p); perr != nil {
		return perr
	}
	owner, perr := normalizeClientIDParam(p.ClientID)
	if perr != nil {
		return perr
	}
	sessions, entries, err := manager.History().Clear(owner, p.SessionID)
	if err != nil {
		return mapTerminalError(err)
	}
	response.Result = encode(historyClearResult{ClearedSessions: sessions, ClearedEntries: entries})
	return nil
}

// normalizeClientIDParam 归一化 client_id 参数（空串取 default）。
func normalizeClientIDParam(raw string) (string, *pluginapi.Error) {
	owner, err := terminal.NormalizeClientID(raw)
	if err != nil {
		return "", mapTerminalError(err)
	}
	return owner, nil
}

// resolvedHistoryLimit 返回查询实际生效的 limit（仅用于回显）。
func resolvedHistoryLimit(limit int) int {
	if limit <= 0 {
		return terminal.DefaultHistoryQueryLimit
	}
	if limit > terminal.MaxHistoryQueryLimit {
		return terminal.MaxHistoryQueryLimit
	}
	return limit
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
	case errors.Is(err, terminal.ErrForbidden):
		return methodError(pluginapi.E_UNAUTHORIZED, err.Error())
	case errors.Is(err, terminal.ErrCursorExpired):
		return methodError(pluginapi.E_CONFLICT, err.Error())
	case errors.Is(err, terminal.ErrRateLimit):
		return methodError(pluginapi.E_RATELIMIT, err.Error())
	case errors.Is(err, terminal.ErrTimeout):
		return methodError(pluginapi.E_UPSTREAM, err.Error())
	case errors.Is(err, terminal.ErrClosed):
		return methodError(pluginapi.E_CONFLICT, err.Error())
	case errors.Is(err, terminal.ErrUnsupported):
		return methodError(pluginapi.E_UNSUPPORTED, err.Error())
	case errors.Is(err, terminal.ErrIO):
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
