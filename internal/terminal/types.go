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
}

// Info 是会话快照（对外 schema，字段显式）。
type Info struct {
	// SessionID 会话标识，形如 t-1a2b3c4d5e6f7a8b。
	SessionID string `json:"session_id"`
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
	// LastActive 是最近一次 create/write/read 触碰时间。
	LastActive string `json:"last_active"`
	// EOF 表示 shell 已退出且输出已读完。
	EOF bool `json:"eof"`
}

// ReadResult 是增量读取结果；Data 可能为空（当前没有输出）。
type ReadResult struct {
	// Data 是本次读到的原始字节（可能含任意二进制与未完成的 UTF-8 序列）。
	Data []byte
	// EOF 表示 shell 已退出且输出已读完。
	EOF bool
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
	return opts
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
