package terminal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 业务错误哨兵；main 的 methods 层按 errors.Is 映射为协议错误码。
var (
	// ErrInvalid 表示参数非法（会话 id 缺失、窗口尺寸越界、shell 不可用等）。
	ErrInvalid = errors.New("terminal: 参数非法")
	// ErrNotFound 表示会话不存在（未创建、已关闭或已被空闲回收）。
	ErrNotFound = errors.New("terminal: 会话不存在")
	// ErrClosed 表示会话已关闭或子进程已退出，不能再读写。
	ErrClosed = errors.New("terminal: 会话已关闭")
	// ErrTimeout 表示请求预算用尽或 PTY 缓冲区长时间不可写。
	ErrTimeout = errors.New("terminal: 操作超时")
	// ErrIO 表示 PTY 读写或进程启动失败。
	ErrIO = errors.New("terminal: PTY 读写失败")
	// ErrResource 表示资源不足（进程数上限、内存配额等）。
	ErrResource = errors.New("terminal: 资源不足")
	// ErrRateLimit 表示会话数已达上限。
	ErrRateLimit = errors.New("terminal: 会话数已达上限")
	// ErrUnsupported 表示当前平台不支持 PTY（显式降级，不静默）。
	ErrUnsupported = errors.New("terminal: 当前平台不支持 PTY")
	// ErrForbidden 表示调用方不是会话创建者：越权 attach / 读写 / 关闭一律拒绝。
	ErrForbidden = errors.New("terminal: 会话不属于当前调用方")
	// ErrCursorExpired 表示读游标早于补读窗口下界，已读内容无法续读（必须重新对齐）。
	ErrCursorExpired = errors.New("terminal: 读游标已过期")
)

// Fault 是测试用故障注入枚举，生产路径默认 FaultNone。
type Fault int

// 四类故障注入值，对应 tests/fault 的 kill / timeout / disk / cgroup 用例。
const (
	// FaultNone 无故障。
	FaultNone Fault = iota
	// FaultKilled 模拟会话进程被 kill：create 回 ErrIO，已有会话读写回 ErrClosed。
	FaultKilled
	// FaultTimeout 模拟请求超时：所有终端方法回 ErrTimeout。
	FaultTimeout
	// FaultDiskFull 模拟磁盘/设备不可用：create 回 ErrIO，已有会话不受影响。
	FaultDiskFull
	// FaultCgroup 模拟 cgroup 限额：所有终端方法回 ErrResource。
	FaultCgroup
)

// 会话参数默认值与硬上限（协议面在 README 中明示）。
const (
	// DefaultMaxSessions 是默认同时存活会话数。
	DefaultMaxSessions = 4
	// DefaultIdleTimeout 是默认空闲回收阈值（30 分钟）。
	DefaultIdleTimeout = 30 * time.Minute
	// DefaultReadChunk 是 terminal.read 未给 max_bytes 时的单次读取上限（64 KiB）。
	DefaultReadChunk = 64 << 10
	// MaxReadBytes 是单次读取的硬上限（1 MiB）。
	MaxReadBytes = 1 << 20
	// MaxWriteBytes 是单次写入的硬上限（1 MiB）。
	MaxWriteBytes = 1 << 20
	// maxGeometry 是 cols/rows 的硬上限。
	maxGeometry = 1000
	// maxSessionsLimit 是 MaxSessions 的硬上限。
	maxSessionsLimit = 64
	// DefaultClientID 是未声明 client_id 时的调用方标识（保持既有 6 方法的旧语义）。
	DefaultClientID = "default"
	// maxClientIDLen 是 client_id 的长度上限。
	maxClientIDLen = 64
	// DefaultReplayBytes 是重连补读窗口的默认字节数（保留最近交付的输出）。
	DefaultReplayBytes = 64 << 10
	// MaxReplayBytes 是补读窗口的硬上限（4 MiB）。
	MaxReplayBytes = 4 << 20
)

// Options 是 Manager 的运行参数；零值取默认值。
type Options struct {
	// MaxSessions 同时存活会话上限，默认 4、硬上限 64。
	MaxSessions int
	// IdleTimeout 空闲回收阈值，默认 30 分钟；<=0 取默认值。
	IdleTimeout time.Duration
	// ReadChunk 单次读取默认字节数，默认 64 KiB。
	ReadChunk int
	// Shell 是 create 未指定 shell 时的默认登录 shell。
	Shell string
	// ReplayBytes 是重连补读窗口字节数，默认 64 KiB、硬上限 4 MiB。
	ReplayBytes int
	// History 是命令历史存储配置；Dir 为空（或 Disabled）时不落盘、不记录。
	History HistoryOptions
}

// Info 是会话快照（对外 schema，字段显式）。
type Info struct {
	// SessionID 会话标识，形如 t-1a2b3c4d5e6f7a8b。
	SessionID string `json:"session_id"`
	// ClientID 是会话创建者标识；后续读写/关闭必须携带同一标识。
	ClientID string `json:"client_id"`
	// Shell 是登录 shell 的绝对路径。
	Shell string `json:"shell"`
	// PID 是 shell 进程 pid，同时也是进程组号（Setsid 后）。
	PID int `json:"pid"`
	// Cols 是当前列数。
	Cols int `json:"cols"`
	// Rows 是当前行数。
	Rows int `json:"rows"`
	// CreatedAt 是会话创建时间（RFC 3339 带纳秒）。
	CreatedAt string `json:"created_at"`
	// LastActive 是最近一次 create/write/read/resize/attach 触碰时间。
	LastActive string `json:"last_active"`
	// Cursor 是当前输出游标（已交付字节数，单调递增）。
	Cursor uint64 `json:"cursor"`
	// EOF 表示 shell 已退出且输出已读完。
	EOF bool `json:"eof"`
}

// ReadResult 是增量读取结果；Data 可能为空（当前没有输出）。
type ReadResult struct {
	// Data 是本次读到的原始字节（可能含任意二进制与未完成的 UTF-8 序列）。
	Data []byte
	// EOF 表示 shell 已退出且输出已读完。
	EOF bool
	// Cursor 是本次读取后的输出游标，客户端下次带它续读。
	Cursor uint64
	// Replay 表示本次是从补读窗口回放（非首次交付），用于客户端识别重连续读。
	Replay bool
}

// Attached 是 attach 的结果：会话快照 + 游标可恢复性。
type Attached struct {
	// Info 是会话快照。
	Info Info
	// Cursor 是当前输出游标。
	Cursor uint64
	// ReplayBytes 是可补读窗口字节数（offset 与窗口下界之差）。
	ReplayBytes int
	// IdleExpiresAt 是按当前空闲阈值推算的回收时刻（RFC 3339 带纳秒）。
	IdleExpiresAt string
}

// normalizeOptions 归一化零值参数。
func normalizeOptions(opts Options) Options {
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = DefaultMaxSessions
	}
	if opts.MaxSessions > maxSessionsLimit {
		opts.MaxSessions = maxSessionsLimit
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.ReadChunk <= 0 {
		opts.ReadChunk = DefaultReadChunk
	}
	if opts.ReadChunk > MaxReadBytes {
		opts.ReadChunk = MaxReadBytes
	}
	if strings.TrimSpace(opts.Shell) == "" {
		opts.Shell = "/bin/bash"
	}
	if opts.ReplayBytes <= 0 {
		opts.ReplayBytes = DefaultReplayBytes
	}
	if opts.ReplayBytes > MaxReplayBytes {
		opts.ReplayBytes = MaxReplayBytes
	}
	opts.History = normalizeHistoryOptions(opts.History)
	return opts
}

// normalizeClientID 归一化调用方标识：空串取 default，其余做长度与字符集校验
// （标识只作为插件内的逻辑归属标签，真正的认证边界在内核 capability）。
func normalizeClientID(raw string) (string, error) {
	clientID := strings.TrimSpace(raw)
	if clientID == "" {
		return DefaultClientID, nil
	}
	if len(clientID) > maxClientIDLen {
		return "", fmt.Errorf("%w: client_id 最多 %d 字节", ErrInvalid, maxClientIDLen)
	}
	for _, r := range clientID {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == ':' || r == '@'
		if !valid {
			return "", fmt.Errorf("%w: client_id 仅允许字母、数字与 -_.:@", ErrInvalid)
		}
	}
	return clientID, nil
}

// NormalizeClientID 归一化调用方标识（main 层参数校验与归属归一复用同一实现）。
func NormalizeClientID(raw string) (string, error) {
	return normalizeClientID(raw)
}

// validateSize 校验终端窗口尺寸。
func validateSize(cols, rows int) error {
	if cols < 1 || cols > maxGeometry || rows < 1 || rows > maxGeometry {
		return fmt.Errorf("%w: cols/rows 必须在 1..%d", ErrInvalid, maxGeometry)
	}
	return nil
}

// resolveShell 校验并返回登录 shell 的规范绝对路径。
func resolveShell(requested, fallback string) (string, error) {
	shellPath := strings.TrimSpace(requested)
	if shellPath == "" {
		shellPath = strings.TrimSpace(fallback)
	}
	if shellPath == "" {
		shellPath = "/bin/bash"
	}
	if !filepath.IsAbs(shellPath) || filepath.Clean(shellPath) != shellPath {
		return "", fmt.Errorf("%w: shell 必须是规范绝对路径", ErrInvalid)
	}
	stat, err := os.Stat(shellPath)
	if err != nil {
		return "", fmt.Errorf("%w: shell 不可用: %v", ErrInvalid, err)
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: shell 不是可执行文件", ErrInvalid)
	}
	return shellPath, nil
}

// loginShells 是需要 -l 参数启动的常见登录 shell。
var loginShells = map[string]bool{
	"bash": true, "sh": true, "dash": true, "zsh": true, "ksh": true, "mksh": true, "fish": true,
}

// shellArgs 返回登录 shell 的启动参数；未知 shell 不加参数，避免把 -l 当成业务参数。
func shellArgs(shellPath string) []string {
	if loginShells[filepath.Base(shellPath)] {
		return []string{"-l"}
	}
	return nil
}

// newSessionID 生成 t- 前缀的会话标识（8 字节随机 hex）。
func newSessionID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "t-" + hex.EncodeToString(buf), nil
}

// ctxError 把已取消/超时的请求上下文转成 ErrTimeout。
func ctxError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	return nil
}

// faultError 返回指定操作在当前注入故障下应返回的错误；nil 表示放行。
func faultError(fault Fault, create bool) error {
	switch fault {
	case FaultTimeout:
		return ErrTimeout
	case FaultCgroup:
		return ErrResource
	case FaultKilled:
		if create {
			return fmt.Errorf("%w: PTY 设备不可用", ErrIO)
		}
		return ErrClosed
	case FaultDiskFull:
		if create {
			return fmt.Errorf("%w: PTY 设备不可用", ErrIO)
		}
		return nil
	default:
		return nil
	}
}
