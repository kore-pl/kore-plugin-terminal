package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// embeddedInfo 是随二进制发布的内嵌清单 manifest.json（只含握手字段，不含 size_bytes）。
//
//go:embed manifest.json
var embeddedInfo []byte

// manifest 是 info.json 中插件运行期需要的字段。
type manifest struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	Protocol     string   `json:"protocol"`
	RequiresKore string   `json:"requires_kore"`
	Capabilities []string `json:"capabilities"`
}

// 会话管理的默认值与硬上限（协议侧参数，见 README）。
const (
	defaultMaxSessions = 4
	maxSessionsLimit   = 16
	defaultIdleSec     = 1800
	maxIdleSec         = 86400
	defaultShell       = "/bin/bash"
)

// config 是运行期配置：环境变量 + 内嵌清单，不读任何配置文件。
type config struct {
	socket       string
	id           string
	token        string
	version      string
	requiresKore string
	capabilities []string
	startedAt    time.Time
	maxSessions  int
	idleTimeout  time.Duration
	defaultShell string
	replayBytes  int
	historyDir   string
	history      terminal.HistoryOptions
}

// loadConfig 读取协议环境变量、终端参数并解析内嵌 manifest.json。
func loadConfig() (config, error) {
	var m manifest
	if err := json.Unmarshal(embeddedInfo, &m); err != nil {
		return config{}, fmt.Errorf("解析内嵌 manifest.json: %w", err)
	}
	if m.Protocol != pluginapi.Protocol {
		return config{}, fmt.Errorf("info.json.protocol=%q 与 SDK 协议 %q 不一致", m.Protocol, pluginapi.Protocol)
	}
	cfg := config{
		socket:       os.Getenv("KORE_PLUGIN_SOCK"),
		id:           os.Getenv("KORE_PLUGIN_ID"),
		token:        os.Getenv("KORE_PLUGIN_TOKEN"),
		version:      m.Version,
		requiresKore: m.RequiresKore,
		capabilities: m.Capabilities,
		startedAt:    time.Now(),
		maxSessions:  envLimitedInt("KORE_TERMINAL_MAX_SESSIONS", defaultMaxSessions, maxSessionsLimit),
		idleTimeout:  time.Duration(envLimitedInt("KORE_TERMINAL_IDLE_SEC", defaultIdleSec, maxIdleSec)) * time.Second,
		defaultShell: envString("KORE_TERMINAL_SHELL", defaultShell),
		replayBytes:  envLimitedInt("KORE_TERMINAL_REPLAY_BYTES", terminal.DefaultReplayBytes, terminal.MaxReplayBytes),
	}
	cfg.history = loadHistoryOptions()
	cfg.historyDir = cfg.history.Dir
	if cfg.id == "" {
		cfg.id = m.ID
	}
	if cfg.socket == "" {
		return config{}, fmt.Errorf("缺少环境变量 KORE_PLUGIN_SOCK")
	}
	return cfg, nil
}

// loadHistoryOptions 组装命令历史配置：KORE_STATE_DIR 缺省或显式 off 时停用（不落盘）。
func loadHistoryOptions() terminal.HistoryOptions {
	opts := terminal.HistoryOptions{
		MaxEntries:    envLimitedInt("KORE_TERMINAL_HISTORY_MAX", terminal.DefaultHistoryEntries, terminal.MaxHistoryEntries),
		MaxEntryBytes: envLimitedInt("KORE_TERMINAL_HISTORY_ENTRY_BYTES", terminal.DefaultHistoryEntryBytes, terminal.MaxHistoryEntryBytes),
		MaxSessions:   envLimitedInt("KORE_TERMINAL_HISTORY_SESSIONS", terminal.DefaultHistorySessions, terminal.MaxHistorySessions),
	}
	stateDir := strings.TrimSpace(os.Getenv("KORE_STATE_DIR"))
	if stateDir == "" || !envBool("KORE_TERMINAL_HISTORY", true) {
		opts.Disabled = true
		return opts
	}
	opts.Dir = filepath.Join(stateDir, "history")
	return opts
}

// envLimitedInt 读取正整数环境变量：未设置或非法取默认值，超上限截断。
func envLimitedInt(key string, def, max int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return def
	}
	if value > max {
		return max
	}
	return value
}

// envString 读取非空字符串环境变量，未设置时取默认值。
func envString(key, def string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return def
}

// envBool 读取布尔环境变量：off/false/0/no 为假，其余（含空）取默认值。
func envBool(key string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch raw {
	case "":
		return def
	case "off", "false", "0", "no", "disabled":
		return false
	case "on", "true", "1", "yes", "enabled":
		return true
	default:
		return def
	}
}
