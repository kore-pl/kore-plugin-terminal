//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package boundary

// 并发（concurrency）边界：真实 goroutine 同时调用同一 Manager，在 -race 下断言
// 无数据竞争、无丢失更新、无残留会话。三个场景对应会话生命周期的三个竞争面：
// 并发创建/销毁、同一会话并发写、并发 attach/read/resize。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
)

// TestConcurrentCreateAndClose 断言 8 个 goroutine 并发创建/关闭各自会话时：
// 每次 create 都成功、归属正确、close 都成功，结束后 List 为空（无泄漏会话）。
func TestConcurrentCreateAndClose(t *testing.T) {
	const workers, rounds = 8, 6
	manager := newManager(t, 64, terminal.HistoryOptions{})
	shell := catShell(t)

	var wg sync.WaitGroup
	errCh := make(chan error, workers*rounds)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			clientID := fmt.Sprintf("creator-%d", w)
			for i := 0; i < rounds; i++ {
				info, err := manager.Create(context.Background(), clientID, shell, 80, 24)
				if err != nil {
					errCh <- fmt.Errorf("worker %d 第 %d 次 create: %w", w, i, err)
					return
				}
				if info.ClientID != clientID {
					errCh <- fmt.Errorf("worker %d 会话归属=%q，期望 %q", w, info.ClientID, clientID)
					return
				}
				if err := manager.Close(context.Background(), clientID, info.SessionID); err != nil {
					errCh <- fmt.Errorf("worker %d 第 %d 次 close: %w", w, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if list := manager.List(""); len(list) != 0 {
		t.Fatalf("并发创建/关闭后残留 %d 个会话: %+v", len(list), list)
	}
}

// TestConcurrentWriteSameSession 断言同一会话被 8 个 goroutine 并发写入时：
// 每次写入返回完整字节数，且全部 token 都出现在输出中（不丢写、不串写）。
func TestConcurrentWriteSameSession(t *testing.T) {
	const writers, perWriter = 8, 16
	manager := newManager(t, 2, terminal.HistoryOptions{})
	info := mustCreate(t, manager, testClient, "", 80, 24)

	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	tokens := make([]string, 0, writers*perWriter)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				token := fmt.Sprintf("kore-w-%02d-%02d", w, i)
				data := []byte(token + "\n")
				written, err := manager.Write(context.Background(), testClient, info.SessionID, data)
				if err != nil {
					errCh <- fmt.Errorf("worker %d 第 %d 次 write: %w", w, i, err)
					return
				}
				if written != len(data) {
					errCh <- fmt.Errorf("worker %d 第 %d 次 write 写了 %d/%d 字节", w, i, written, len(data))
					return
				}
			}
		}(w)
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			tokens = append(tokens, fmt.Sprintf("kore-w-%02d-%02d", w, i))
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	output := readUntil(t, manager, testClient, info.SessionID, 10*time.Second, func(s string) bool {
		for _, token := range tokens {
			if !strings.Contains(s, token) {
				return false
			}
		}
		return true
	})
	missing := make([]string, 0)
	for _, token := range tokens {
		if !strings.Contains(output, token) {
			missing = append(missing, token)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d 个并发写 token 丢失: %v", len(missing), missing[:min(5, len(missing))])
	}
}

// TestConcurrentAttachDetach 断言 8 个 goroutine 并发 attach / read / resize，
// 同时另一组 goroutine 持续 write 时：全部调用只返回 nil 错误，游标单调不减。
func TestConcurrentAttachDetach(t *testing.T) {
	const readers, rounds = 8, 20
	manager := newManager(t, 2, terminal.HistoryOptions{})
	info := mustCreate(t, manager, testClient, "", 80, 24)

	var wg sync.WaitGroup
	errCh := make(chan error, readers+1)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			var last uint64
			for i := 0; i < rounds; i++ {
				attached, err := manager.Attach(context.Background(), testClient, info.SessionID, nil)
				if err != nil {
					errCh <- fmt.Errorf("reader %d 第 %d 次 attach: %w", r, i, err)
					return
				}
				if attached.Cursor < last {
					errCh <- fmt.Errorf("reader %d 游标回退 %d -> %d", r, last, attached.Cursor)
					return
				}
				last = attached.Cursor
				if _, err := manager.Read(context.Background(), testClient, info.SessionID, nil, 1024); err != nil {
					errCh <- fmt.Errorf("reader %d 第 %d 次 read: %w", r, i, err)
					return
				}
				cols, rows := 80, 24
				if i%2 == 1 {
					cols, rows = 120, 40
				}
				if err := manager.Resize(context.Background(), testClient, info.SessionID, cols, rows); err != nil {
					errCh <- fmt.Errorf("reader %d 第 %d 次 resize: %w", r, i, err)
					return
				}
			}
		}(r)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			if _, err := manager.Write(context.Background(), testClient, info.SessionID, []byte("kore-attach-ping\n")); err != nil {
				errCh <- fmt.Errorf("writer 第 %d 次 write: %w", i, err)
				return
			}
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

// min 返回较小值（Go 1.21 内置 min 的本地别名，避免依赖工具链版本行为）。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
