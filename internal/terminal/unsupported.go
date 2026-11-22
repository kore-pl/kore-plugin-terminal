//go:build !linux || (!amd64 && !arm64 && !riscv64 && !loong64)

// Package terminal 在无 PTY 支持的平台（非 Linux 或非 64 位小端架构）上提供
// 显式降级实现：所有会话操作返回 ErrUnsupported，不做静默降级、不伪造成功。
package terminal

import "context"

// Manager 是空实现，仅保证插件在 Tier B 平台可编译并给出明确错误码。
type Manager struct {
	opts Options
}

// New 创建空管理器。
func New(opts Options) *Manager {
	return &Manager{opts: normalizeOptions(opts)}
}

// InjectFault 在空实现上无副作用。
func (m *Manager) InjectFault(Fault) {}

// Create 在无 PTY 平台上返回 ErrUnsupported。
func (m *Manager) Create(context.Context, string, int, int) (Info, error) {
	return Info{}, ErrUnsupported
}

// Read 在无 PTY 平台上返回 ErrUnsupported。
func (m *Manager) Read(context.Context, string, int) (ReadResult, error) {
	return ReadResult{}, ErrUnsupported
}

// Write 在无 PTY 平台上返回 ErrUnsupported。
func (m *Manager) Write(context.Context, string, []byte) (int, error) {
	return 0, ErrUnsupported
}

// Resize 在无 PTY 平台上返回 ErrUnsupported。
func (m *Manager) Resize(context.Context, string, int, int) error {
	return ErrUnsupported
}

// Close 在无 PTY 平台上返回 ErrUnsupported。
func (m *Manager) Close(context.Context, string) error {
	return ErrUnsupported
}

// List 在无 PTY 平台上返回空列表。
func (m *Manager) List() []Info {
	return nil
}

// CloseAll 在空实现上无副作用。
func (m *Manager) CloseAll() {}
