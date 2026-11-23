//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package main

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// pluginInfo 是 info.json 中本测试需要的字段（method_capabilities 是 §3.5 #12 的声明面）。
type pluginInfo struct {
	ID                 string            `json:"id"`
	Version            string            `json:"version"`
	Protocol           string            `json:"protocol"`
	RequiresKore       string            `json:"requires_kore"`
	Capabilities       []string          `json:"capabilities"`
	MethodCapabilities map[string]string `json:"method_capabilities"`
}

// privilegedCapabilities 是 PLUGIN-PROTOCOL 规定的 6 项特权能力（方法级取值集合）。
var privilegedCapabilities = map[string]bool{
	"system:read": true, "system:admin": true,
	"net:read": true, "net:write": true,
	"proc:read": true, "proc:write": true,
}

// loadJSON 读取仓根 JSON 文件（go test 的包目录即仓根）。
func loadJSON(t *testing.T, path string, dst any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}
}

// TestMethodCapabilityDeclarations 是 capability 声明 vs 实现的表驱动一致性门禁：
//  1. 每个已实现动词（terminalVerbs，唯一真源）都有方法级声明，且没有多余声明；
//  2. 只读方法 → system:read，写/管理方法 → system:admin（拒绝只读方法进写审计）；
//  3. 键必须是 `<id>.<action>`，值必须是 6 项特权能力之一（与内核 validate.go 同口径）；
//  4. info.json 与 manifest.json 的版本/协议/能力集一致（G-17 发版口径）；
//  5. 每个声明的方法都能被 dispatch 识别（参数非法 ≠ 未知方法）。
func TestMethodCapabilityDeclarations(t *testing.T) {
	var info pluginInfo
	loadJSON(t, "info.json", &info)
	var manifest pluginInfo
	loadJSON(t, "manifest.json", &manifest)

	if info.ID != "terminal" || info.Protocol != pluginapi.Protocol {
		t.Fatalf("info.json 基础字段不符: id=%q protocol=%q", info.ID, info.Protocol)
	}
	if manifest.ID != info.ID || manifest.Version != info.Version ||
		manifest.Protocol != info.Protocol || manifest.RequiresKore != info.RequiresKore {
		t.Fatalf("manifest.json 与 info.json 不一致: manifest=%+v info=%+v", manifest, info)
	}
	if strings.Join(manifest.Capabilities, ",") != strings.Join(info.Capabilities, ",") {
		t.Fatalf("manifest.capabilities=%v 与 info.capabilities=%v 不一致",
			manifest.Capabilities, info.Capabilities)
	}

	expected := map[string]string{
		"terminal.list":          "system:read",
		"terminal.read":          "system:read",
		"terminal.attach":        "system:read",
		"terminal.history":       "system:read",
		"terminal.create":        "system:admin",
		"terminal.write":         "system:admin",
		"terminal.resize":        "system:admin",
		"terminal.close":         "system:admin",
		"terminal.history.clear": "system:admin",
	}
	if len(info.MethodCapabilities) != len(terminalVerbs) {
		t.Fatalf("method_capabilities 条数=%d，已实现动词=%d（必须一一对应）",
			len(info.MethodCapabilities), len(terminalVerbs))
	}
	methods := make([]string, 0, len(info.MethodCapabilities))
	for method := range info.MethodCapabilities {
		methods = append(methods, method)
	}
	sort.Strings(methods)
	for _, method := range methods {
		capability := info.MethodCapabilities[method]
		want, ok := expected[method]
		if !ok {
			t.Fatalf("method_capabilities 声明了未定义方法 %q", method)
		}
		if capability != want {
			t.Fatalf("method_capabilities[%q]=%q，期望 %q", method, capability, want)
		}
		if !strings.HasPrefix(method, info.ID+".") {
			t.Fatalf("方法级键 %q 必须以插件 id 为域", method)
		}
		if !privilegedCapabilities[capability] {
			t.Fatalf("method_capabilities[%q]=%q 不是 6 项特权能力之一", method, capability)
		}
	}
	for _, verb := range terminalVerbs {
		method := info.ID + "." + verb
		if _, ok := info.MethodCapabilities[method]; !ok {
			t.Fatalf("已实现动词 %q 缺少方法级 capability 声明", verb)
		}
	}

	// 实现侧覆盖：每个声明的方法都必须被 dispatch 识别（非法参数返回 E_INVALID 等，
	// 而不是 E_NOTFOUND）；用 cols=0 让 create 在起进程前被拦下，测试不产生真实 shell。
	cfg := testConfig()
	manager := terminal.New(terminal.Options{MaxSessions: 2, IdleTimeout: time.Minute, Shell: "/bin/bash"})
	t.Cleanup(manager.CloseAll)
	for _, verb := range terminalVerbs {
		response := dispatchCall(t, cfg, manager, "terminal."+verb, `{"cols":0}`)
		if response.Error != nil && response.Error.Code == pluginapi.E_NOTFOUND {
			t.Fatalf("声明了 terminal.%s 但 dispatch 不认识", verb)
		}
	}
}
