//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package boundary

// 异常输入（invalid input / malformed）边界：非法会话 id、超长命令、非法终端尺寸、
// 格式错误的帧。断言明确哨兵错误（ErrInvalid / ErrNotFound）与 pluginapi 帧错误，
// 不放过任何静默截断或 panic。

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestInvalidSessionIDRejected 断言非法会话 id（空、路径穿越形状、超长、错误大小写）
// 一律 ErrNotFound：id 只做 map 查找，绝不参与路径拼接，因此不能读/写任意文件。
func TestInvalidSessionIDRejected(t *testing.T) {
	manager := newManager(t, 4, terminal.HistoryOptions{})
	info := mustCreate(t, manager, testClient, "", 80, 24)

	badIDs := []string{
		"",                            // 缺失
		"t-",                          // 只有前缀
		"../etc/passwd",               // 路径穿越形状
		"t-../../etc/passwd",          // 带合法前缀的路径穿越形状
		"t-0123456789ABCDEF",          // 大写不符合 hex 形状
		strings.Repeat("a", 1<<12),    // 超长
		"t-0123456789abcdef trailing", // 尾部多余内容
	}
	for _, id := range badIDs {
		if _, err := manager.Read(bg(), testClient, id, nil, 64); !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("read id=%q 错误=%v，期望 ErrNotFound", id, err)
		}
		if written, err := manager.Write(bg(), testClient, id, []byte("x")); !errors.Is(err, terminal.ErrNotFound) || written != 0 {
			t.Errorf("write id=%q written=%d 错误=%v，期望 ErrNotFound", id, written, err)
		}
		if err := manager.Resize(bg(), testClient, id, 80, 24); !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("resize id=%q 错误=%v，期望 ErrNotFound", id, err)
		}
		if _, err := manager.Attach(bg(), testClient, id, nil); !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("attach id=%q 错误=%v，期望 ErrNotFound", id, err)
		}
		if err := manager.Close(bg(), testClient, id); !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("close id=%q 错误=%v，期望 ErrNotFound", id, err)
		}
	}
	// 合法会话不受影响，仍可正常读写。
	if _, err := manager.Write(bg(), testClient, info.SessionID, []byte("kore-still-alive\n")); err != nil {
		t.Fatalf("合法会话被非法 id 请求影响: %v", err)
	}
}

// TestOversizedCommandRejected 断言单次写入超过协议硬上限（1 MiB）时在落 PTY 前
// 返回 ErrInvalid，且一个字节都不写。
func TestOversizedCommandRejected(t *testing.T) {
	manager := newManager(t, 2, terminal.HistoryOptions{})
	info := mustCreate(t, manager, testClient, "", 80, 24)

	oversized := make([]byte, terminal.MaxWriteBytes+1)
	written, err := manager.Write(bg(), testClient, info.SessionID, oversized)
	if !errors.Is(err, terminal.ErrInvalid) {
		t.Fatalf("超长命令错误=%v，期望 ErrInvalid", err)
	}
	if written != 0 {
		t.Fatalf("超长命令写入 %d 字节，期望 0（拒绝前不得落盘）", written)
	}
	// 边界内的空写入是合法的 no-op。
	if n, err := manager.Write(bg(), testClient, info.SessionID, nil); n != 0 || err != nil {
		t.Fatalf("空写入 n=%d 错误=%v，期望 0/nil", n, err)
	}
}

// TestInvalidTerminalGeometryRejected 断言 cols/rows 越界（0、负数、>1000）返回 ErrInvalid，
// 而边界值 1 与 1000 必须被接受（上下界都测，防止把合法边界误杀）。
func TestInvalidTerminalGeometryRejected(t *testing.T) {
	manager := newManager(t, 4, terminal.HistoryOptions{})

	invalid := []struct{ cols, rows int }{
		{0, 24}, {80, 0}, {-1, 24}, {80, -1}, {1001, 24}, {80, 1001}, {-1, -1},
	}
	for _, size := range invalid {
		if _, err := manager.Create(bg(), testClient, "", size.cols, size.rows); !errors.Is(err, terminal.ErrInvalid) {
			t.Errorf("create cols=%d rows=%d 错误=%v，期望 ErrInvalid", size.cols, size.rows, err)
		}
	}

	// 合法边界：1x1 与 1000x1000 都必须成功。
	low := mustCreate(t, manager, testClient, "", 1, 1)
	high := mustCreate(t, manager, testClient, "", 1000, 1000)
	if low.Cols != 1 || low.Rows != 1 || high.Cols != 1000 || high.Rows != 1000 {
		t.Fatalf("边界尺寸未生效: low=%dx%d high=%dx%d", low.Cols, low.Rows, high.Cols, high.Rows)
	}

	// resize 同样校验，越界不得改变已生效尺寸。
	if err := manager.Resize(bg(), testClient, low.SessionID, 0, 24); !errors.Is(err, terminal.ErrInvalid) {
		t.Fatalf("resize 非法尺寸错误=%v，期望 ErrInvalid", err)
	}
	if err := manager.Resize(bg(), testClient, low.SessionID, 2, 2); err != nil {
		t.Fatalf("resize 合法尺寸失败: %v", err)
	}
}

// TestMalformedFrameRejected 断言格式错误的帧在读/解码阶段被明确拒绝：
// 空载荷、1 字节载荷、超限长度、截断载荷、截断长度前缀；以及非法 JSON / 未知 type /
// 缺 type 的载荷。错误必须可在分配大缓冲区之前返回。
func TestMalformedFrameRejected(t *testing.T) {
	header := func(n uint32) []byte {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, n)
		return buf
	}

	readCases := []struct {
		name  string
		input []byte
		want  error
	}{
		{"empty-payload", header(0), pluginapi.ErrEmptyPayload},
		{"one-byte-payload", header(1), pluginapi.ErrPayloadTooShort},
		{"oversize-payload", header(pluginapi.MaxFrameSize + 1), pluginapi.ErrFrameTooLarge},
		{"truncated-payload", append(header(64), make([]byte, 8)...), pluginapi.ErrShortFrame},
		{"truncated-header", []byte{0x00, 0x00}, pluginapi.ErrShortFrame},
	}
	for _, tc := range readCases {
		if _, err := pluginapi.ReadFrame(bytes.NewReader(tc.input), pluginapi.MaxFrameSize); !errors.Is(err, tc.want) {
			t.Errorf("%s: 错误=%v，期望 %v", tc.name, err, tc.want)
		}
	}
	// 帧边界上的干净关闭返回 io.EOF，而不是 ErrShortFrame。
	if _, err := pluginapi.ReadFrame(bytes.NewReader(nil), pluginapi.MaxFrameSize); !errors.Is(err, io.EOF) {
		t.Errorf("clean-eof: 错误=%v，期望 io.EOF", err)
	}

	decodeCases := []struct {
		name    string
		payload []byte
		want    error
	}{
		{"invalid-json", []byte("{"), pluginapi.ErrInvalidJSON},
		{"json-array", []byte("[]"), pluginapi.ErrInvalidJSON},
		{"missing-type", []byte("{}"), pluginapi.ErrInvalidJSON},
		{"unknown-type", []byte(`{"type":"bogus"}`), pluginapi.ErrUnknownType},
		{"wrong-field-type", []byte(`{"type":"request","id":42}`), pluginapi.ErrInvalidJSON},
		{"empty-payload", nil, pluginapi.ErrEmptyPayload},
	}
	for _, tc := range decodeCases {
		if _, err := pluginapi.DecodeFrame(tc.payload); !errors.Is(err, tc.want) {
			t.Errorf("%s: 错误=%v，期望 %v", tc.name, err, tc.want)
		}
	}
	// 合法最小请求必须能解码，证明上面的失败不是无差别拒绝。
	env, err := pluginapi.DecodeFrame([]byte(`{"type":"request","id":"r-1","method":"terminal.list"}`))
	if err != nil {
		t.Fatalf("合法请求解码失败: %v", err)
	}
	if _, ok := env.(*pluginapi.Request); !ok {
		t.Fatalf("合法请求解码类型=%T，期望 *pluginapi.Request", env)
	}
}

// bg 返回测试用上下文；单独抽出避免每处重复 context.Background()。
func bg() context.Context { return context.Background() }
