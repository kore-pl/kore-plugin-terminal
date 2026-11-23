// Package main 是 kore 的 terminal 插件：Web 终端的真实 PTY 后端。
//
// 插件做协议握手 + 会话管理 + 参数校验；PTY 由 internal/terminal 用标准库
// syscall 直接打开（不引 creack/pty 等第三方库），每个会话对应一个登录 shell。
// 协议面（kore.plugin.v1）：UDS → kore.hello 握手 → 会话循环读 request、
// 按 method 分发并回 response；stdout 留给协议，日志一律写 stderr。
package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
)

// main 按退出码区分失败：0 优雅退出、1 启动或协议失败。
func main() {
	logger := newLogger(os.Getenv("KORE_LOG_LEVEL"))
	if err := run(logger); err != nil {
		logger.Error("插件退出", "err", err)
		os.Exit(1)
	}
}

// run 依次完成建连、hello 握手与会话循环；收到 kore.shutdown 后关闭全部终端。
func run(logger *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	conn, err := net.Dial("unix", cfg.socket)
	if err != nil {
		return fmt.Errorf("连接主壳 socket %s: %w", cfg.socket, err)
	}
	defer func() { _ = conn.Close() }()
	if err := handshake(conn, cfg); err != nil {
		return err
	}
	logger.Info("hello 完成", "plugin_id", cfg.id, "version", cfg.version)
	if cfg.historyDir == "" {
		logger.Warn("命令历史未启用：KORE_STATE_DIR 未设置或 KORE_TERMINAL_HISTORY=off，write 输入不落盘")
	}
	manager := terminal.New(terminal.Options{
		MaxSessions: cfg.maxSessions,
		IdleTimeout: cfg.idleTimeout,
		Shell:       cfg.defaultShell,
		ReplayBytes: cfg.replayBytes,
		History:     cfg.history,
	})
	defer manager.CloseAll()
	return serve(conn, cfg, manager, logger)
}
