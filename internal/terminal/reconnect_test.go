//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package terminal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// drainUntil 增量读到标记出现，返回累计输出与最后一次游标。
func drainUntil(t *testing.T, manager *Manager, clientID, id, want string, timeout time.Duration) (string, uint64) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got strings.Builder
	var cursor uint64
	for time.Now().Before(deadline) {
		result, err := manager.Read(context.Background(), clientID, id, nil, 64<<10)
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		got.Write(result.Data)
		cursor = result.Cursor
		if strings.Contains(got.String(), want) {
			return got.String(), cursor
		}
		if result.EOF {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s 内未读到 %q，实际=%q", timeout, want, got.String())
	return "", 0
}

// drainQuiet 读到静默 quit 时长为止，返回累计输出与最后一次游标。
func drainQuiet(t *testing.T, manager *Manager, clientID, id string, quiet time.Duration) (string, uint64) {
	t.Helper()
	var got strings.Builder
	var cursor uint64
	deadline := time.Now().Add(5 * time.Second)
	lastData := time.Now()
	for time.Now().Before(deadline) {
		result, err := manager.Read(context.Background(), clientID, id, nil, 64<<10)
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		if len(result.Data) > 0 {
			got.Write(result.Data)
			cursor = result.Cursor
			lastData = time.Now()
			continue
		}
		if time.Since(lastData) >= quiet {
			return got.String(), cursor
		}
		time.Sleep(20 * time.Millisecond)
	}
	return got.String(), cursor
}

// TestAttachCursorExpired 覆盖补读窗口不足时明确失败（E_CONFLICT 的来源）。
func TestAttachCursorExpired(t *testing.T) {
	manager := newTestManager(t, Options{ReplayBytes: 8})
	ctx := context.Background()
	info, err := manager.Create(ctx, "web", "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	if _, err := manager.Write(ctx, "web", info.SessionID, []byte("printf '0123456789abcdef\\n'\n")); err != nil {
		t.Fatalf("write 失败: %v", err)
	}
	_, cursor := drainUntil(t, manager, "web", info.SessionID, "0123456789abcdef", 5*time.Second)
	if cursor <= 8 {
		t.Fatalf("游标=%d 不足以触发窗口淘汰", cursor)
	}
	stale := uint64(0)
	if _, err := manager.Attach(ctx, "web", info.SessionID, &stale); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("attach 过期游标错误=%v，期望 ErrCursorExpired", err)
	}
	if _, err := manager.Read(ctx, "web", info.SessionID, &stale, 64); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("read 过期游标错误=%v，期望 ErrCursorExpired", err)
	}
}

// TestReadCursorAheadRejected 断言游标超前（状态不一致）回 ErrInvalid。
func TestReadCursorAheadRejected(t *testing.T) {
	manager := newTestManager(t, Options{})
	ctx := context.Background()
	info, err := manager.Create(ctx, "web", "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	ahead := uint64(1 << 20)
	if _, err := manager.Read(ctx, "web", info.SessionID, &ahead, 64); !errors.Is(err, ErrInvalid) {
		t.Fatalf("超前游标 read 错误=%v，期望 ErrInvalid", err)
	}
	if _, err := manager.Attach(ctx, "web", info.SessionID, &ahead); !errors.Is(err, ErrInvalid) {
		t.Fatalf("超前游标 attach 错误=%v，期望 ErrInvalid", err)
	}
}

// TestReconnectAfterIdleReclaimFails 断言超时回收后重连明确失败（会话不可恢复）。
func TestReconnectAfterIdleReclaimFails(t *testing.T) {
	manager := newTestManager(t, Options{IdleTimeout: 300 * time.Millisecond})
	ctx := context.Background()
	info, err := manager.Create(ctx, "web", "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	// attach 本身就是惰性回收的触发点：先摘除超时会话，再返回 ErrNotFound。
	if _, err := manager.Attach(ctx, "web", info.SessionID, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("超时后 attach 错误=%v，期望 ErrNotFound", err)
	}
	if _, err := manager.Read(ctx, "web", info.SessionID, nil, 64); !errors.Is(err, ErrNotFound) {
		t.Fatalf("超时后 read 错误=%v，期望 ErrNotFound", err)
	}
	waitPidGone(t, info.PID, 3*time.Second)
}

// TestAttachSnapshotSemantics 断言 attach 快照带存活语义（空闲回收时刻、可补读窗口）。
func TestAttachSnapshotSemantics(t *testing.T) {
	manager := newTestManager(t, Options{IdleTimeout: time.Minute})
	ctx := context.Background()
	info, err := manager.Create(ctx, "web", "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	attached, err := manager.Attach(ctx, "web", info.SessionID, nil)
	if err != nil {
		t.Fatalf("attach 失败: %v", err)
	}
	if attached.Info.SessionID != info.SessionID || attached.Info.ClientID != "web" {
		t.Fatalf("attach 快照不符: %+v", attached.Info)
	}
	if attached.IdleExpiresAt == "" || attached.ReplayBytes < 0 {
		t.Fatalf("attach 存活语义字段缺失: %+v", attached)
	}
	expires, err := time.Parse(time.RFC3339Nano, attached.IdleExpiresAt)
	if err != nil {
		t.Fatalf("idle_expires_at 不是 RFC3339: %v", err)
	}
	if time.Until(expires) <= 0 || time.Until(expires) > 2*time.Minute {
		t.Fatalf("idle_expires_at=%s 超出预期窗口", attached.IdleExpiresAt)
	}
}

// TestClientOwnershipRejected 断言越权 attach/read/write/resize/close 全被拒绝，且不影响归属方。
func TestClientOwnershipRejected(t *testing.T) {
	manager := newTestManager(t, Options{})
	ctx := context.Background()
	info, err := manager.Create(ctx, "alice", "", 80, 24)
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	if _, err := manager.Write(ctx, "bob", info.SessionID, []byte("x")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("越权 write 错误=%v，期望 ErrForbidden", err)
	}
	if _, err := manager.Read(ctx, "bob", info.SessionID, nil, 64); !errors.Is(err, ErrForbidden) {
		t.Fatalf("越权 read 错误=%v，期望 ErrForbidden", err)
	}
	if _, err := manager.Attach(ctx, "bob", info.SessionID, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("越权 attach 错误=%v，期望 ErrForbidden", err)
	}
	if err := manager.Resize(ctx, "bob", info.SessionID, 100, 40); !errors.Is(err, ErrForbidden) {
		t.Fatalf("越权 resize 错误=%v，期望 ErrForbidden", err)
	}
	if err := manager.Close(ctx, "bob", info.SessionID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("越权 close 错误=%v，期望 ErrForbidden", err)
	}
	// 越权失败不得影响归属方：会话仍在且可读写。
	if listed := manager.List("alice"); len(listed) != 1 {
		t.Fatalf("越权操作后归属方会话数=%d，期望 1", len(listed))
	}
	if listed := manager.List("bob"); len(listed) != 0 {
		t.Fatalf("bob 不应看到他人会话: %+v", listed)
	}
	if _, err := manager.Write(ctx, "alice", info.SessionID, []byte("echo owner-ok\n")); err != nil {
		t.Fatalf("归属方 write 失败: %v", err)
	}
	drainUntil(t, manager, "alice", info.SessionID, "owner-ok", 5*time.Second)
	if err := manager.Close(ctx, "alice", info.SessionID); err != nil {
		t.Fatalf("归属方 close 失败: %v", err)
	}
}

// TestClientIDValidation 断言 client_id 的字符集与长度校验在起进程之前拦下。
func TestClientIDValidation(t *testing.T) {
	manager := newTestManager(t, Options{})
	ctx := context.Background()
	bad := []string{"bad id", "a/b", strings.Repeat("x", 65), "a\nb"}
	for _, clientID := range bad {
		if _, err := manager.Create(ctx, clientID, "", 80, 24); !errors.Is(err, ErrInvalid) {
			t.Fatalf("client_id=%q 错误=%v，期望 ErrInvalid", clientID, err)
		}
	}
	if listed := manager.List(""); len(listed) != 0 {
		t.Fatalf("非法 client_id 不应创建会话: %+v", listed)
	}
}
