//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package boundary

// 资源耗尽（resource exhaustion）边界：必须真实注入，不能用 InjectFault 模拟。
// 三个真实内核边界：
//   - ulimit -n：fd 耗尽（PTY 每会话吃多个 fd），断言 create 回 ErrIO + EMFILE 原文；
//   - ulimit -v：地址空间耗尽（RLIMIT_AS），断言 exec shell 失败并回 ErrIO + ENOMEM；
//   - tmpfs 写满：user+mount namespace 内挂 64 KiB tmpfs，写满后历史落盘回 ENOSPC。
//
// 每个场景都在独立子进程中施加限额，父测试只核对子进程退出码与观测标记；
// 子进程崩溃/挂住不会污染主测试进程。

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-terminal/internal/terminal"
)

const (
	// envFDHelper 标记 fd 耗尽子进程。
	envFDHelper = "KORE_BOUNDARY_FD_HELPER"
	// envTmpfsHelper 携带 tmpfs 挂载目录，标记 tmpfs 写满子进程。
	envTmpfsHelper = "KORE_BOUNDARY_TMPFS_HELPER"
)

// TestFDExhaustionUlimitReal 用 `ulimit -n 64` 启动子进程，真实耗尽 fd。
func TestFDExhaustionUlimitReal(t *testing.T) {
	if os.Getenv(envFDHelper) != "" {
		t.Skip("helper 进程不走父用例")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制失败: %v", err)
	}
	cmd := exec.Command("/bin/sh", "-c",
		`ulimit -n 64; exec "$0" -test.run '^TestFDExhaustionHelper$' -test.v`, bin)
	cmd.Env = append(os.Environ(), envFDHelper+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fd 耗尽子进程失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "fd-exhaustion-observed") {
		t.Fatalf("子进程未报告 fd 耗尽观测:\n%s", out)
	}
}

// TestFDExhaustionHelper 在 ulimit -n 约束下循环 create，直到 PTY/exec 拿不到 fd。
func TestFDExhaustionHelper(t *testing.T) {
	if os.Getenv(envFDHelper) == "" {
		t.Skip("仅由 TestFDExhaustionUlimitReal 以 ulimit -n 启动")
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("读取 RLIMIT_NOFILE 失败: %v", err)
	}
	if limit.Cur > 128 {
		t.Fatalf("ulimit -n 未生效：soft=%d", limit.Cur)
	}

	manager := terminal.New(terminal.Options{
		MaxSessions: 64, IdleTimeout: time.Minute, Shell: catShell(t),
	})
	shell := catShell(t)
	created := 0
	var lastErr error
	for i := 0; i < 64; i++ {
		if _, err := manager.Create(context.Background(), testClient, shell, 80, 24); err != nil {
			lastErr = err
			break
		}
		created++
	}
	if lastErr == nil {
		manager.CloseAll()
		t.Fatalf("ulimit -n=%d 下创建 %d 个会话仍未触发 fd 耗尽", limit.Cur, created)
	}
	msg := strings.ToLower(lastErr.Error())
	manager.CloseAll()

	if !errors.Is(lastErr, terminal.ErrIO) {
		t.Fatalf("fd 耗尽错误=%v，期望 ErrIO", lastErr)
	}
	if !strings.Contains(msg, "too many open files") && !strings.Contains(msg, "resource temporarily unavailable") {
		t.Fatalf("fd 耗尽错误=%v，期望 EMFILE/ENFILE 原文", lastErr)
	}
	t.Logf("fd-exhaustion-observed: soft=%d created=%d err=%v", limit.Cur, created, lastErr)
}

// TestTmpfsFullEnospcReal 在 user+mount namespace 内挂 64 KiB tmpfs，真实写满后
// 断言历史落盘返回 ENOSPC；环境不支持 user namespace 时明确 skip（不静默通过）。
func TestTmpfsFullEnospcReal(t *testing.T) {
	if os.Getenv(envTmpfsHelper) != "" {
		t.Skip("helper 进程不走父用例")
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skipf("环境缺少 unshare，无法真实挂载 tmpfs: %v", err)
	}
	if err := exec.Command("unshare", "-Ur", "-m", "true").Run(); err != nil {
		t.Skipf("环境不支持 user+mount namespace，无法真实注入 tmpfs 满: %v", err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制失败: %v", err)
	}
	dir := t.TempDir()
	cmd := exec.Command("unshare", "-Ur", "-m", "sh", "-c",
		`mount -t tmpfs -o size=64k tmpfs "$1" && exec "$2" -test.run '^TestTmpfsFullHelper$' -test.v`,
		"sh", dir, bin)
	cmd.Env = append(os.Environ(), envTmpfsHelper+"="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tmpfs 写满子进程失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "tmpfs-full-observed") {
		t.Fatalf("子进程未报告 tmpfs 写满观测:\n%s", out)
	}
}

// TestTmpfsFullHelper 在已写满的 tmpfs 上持续 write，断言历史落盘回 ENOSPC 且终端输入不受影响。
func TestTmpfsFullHelper(t *testing.T) {
	dir := os.Getenv(envTmpfsHelper)
	if dir == "" {
		t.Skip("仅由 TestTmpfsFullEnospcReal 在挂载了 tmpfs 的命名空间中启动")
	}
	mounts, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		t.Fatalf("读取 /proc/self/mounts 失败: %v", err)
	}
	if !strings.Contains(string(mounts), " "+dir+" tmpfs ") {
		t.Fatalf("%s 不是已挂载的 tmpfs:\n%s", dir, mounts)
	}

	manager := terminal.New(terminal.Options{
		MaxSessions: 2, IdleTimeout: time.Minute, Shell: quietCatShell(t),
		History: terminal.HistoryOptions{Dir: dir, Debounce: time.Nanosecond},
	})
	defer manager.CloseAll()
	info, err := manager.Create(context.Background(), testClient, quietCatShell(t), 80, 24)
	if err != nil {
		t.Fatalf("tmpfs 上 create 失败: %v", err)
	}

	payload := bytes.Repeat([]byte("k"), 3500)
	for i := 0; i < 40; i++ {
		written, err := manager.Write(context.Background(), testClient, info.SessionID, payload)
		if err != nil {
			t.Fatalf("第 %d 次 write 失败（历史故障不应影响终端输入）: %v", i, err)
		}
		if written != len(payload) {
			t.Fatalf("第 %d 次 write 写了 %d/%d 字节", i, written, len(payload))
		}
	}

	_, lastErr := manager.History().Status()
	if lastErr == "" {
		t.Fatal("tmpfs 写满后 History.Status 仍为空，未披露落盘失败")
	}
	if !strings.Contains(strings.ToLower(lastErr), "no space left on device") {
		t.Fatalf("历史落盘错误=%q，期望 ENOSPC 原文", lastErr)
	}
	t.Logf("tmpfs-full-observed: %s", lastErr)
}
