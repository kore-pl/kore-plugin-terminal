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
	"strconv"
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
	// 不写死 `ulimit -n 64`：绝对预算会随继承 fd 漂移，helper 改为按自身 fd 表收紧。
	cmd := exec.Command(bin, "-test.run", "^TestFDExhaustionHelper$", "-test.v")
	cmd.Env = append(os.Environ(), envFDHelper+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fd 耗尽子进程失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "fd-exhaustion-observed") {
		t.Fatalf("子进程未报告 fd 耗尽观测:\n%s", out)
	}
}

// dropInheritedFDs 关闭从外层 shell / make 链继承来的 fd（fd ≥ 3 且未置 FD_CLOEXEC）。
//
// 预算按「自身已开 fd + headroom」计算，继承 fd 虽已计入总数，却会挤占 Go runtime /
// race detector 可能懒创建的余量；外层嵌套越深（make check → sh → make → go test），
// 继承 fd 越多，余量越不可预测。关掉之后 fd 表只由本进程决定，预算与宿主继承 fd、
// shell 层数彻底解耦。Go 自己开的 fd 一律带 FD_CLOEXEC，不会被误关。
func dropInheritedFDs(t *testing.T) {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("读取自身 fd 表失败: %v", err)
	}
	dropped := 0
	for _, ent := range ents {
		fd, convErr := strconv.Atoi(ent.Name())
		if convErr != nil || fd < 3 {
			continue
		}
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno != 0 || flags&syscall.FD_CLOEXEC != 0 {
			continue
		}
		if closeErr := syscall.Close(fd); closeErr == nil {
			dropped++
		}
	}
	t.Logf("关闭继承 fd %d 个（预算只按本进程 fd 表计算）", dropped)
}

// TestFDExhaustionHelper 在 ulimit -n 约束下循环 create，直到 PTY/exec 拿不到 fd。
func TestFDExhaustionHelper(t *testing.T) {
	if os.Getenv(envFDHelper) == "" {
		t.Skip("仅由 TestFDExhaustionUlimitReal 以 ulimit -n 启动")
	}
	// 预算相对自身已开 fd（不写死绝对数）：每个会话要吃掉 pty 主从 + 管道 + socketpair，
	// 余量取 32 才能真的创建出几个会话再撞 EMFILE，而不是一开始就 0 个。
	dropInheritedFDs(t)
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &orig); err != nil {
		t.Fatalf("读取 RLIMIT_NOFILE 失败: %v", err)
	}
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("读取自身 fd 表失败: %v", err)
	}
	const headroom = 32
	budget := uint64(len(ents)) + headroom
	if budget > orig.Max {
		t.Fatalf("预算 %d 超过外层硬限 %d：环境把 RLIMIT_NOFILE 压得过低", budget, orig.Max)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: budget, Max: orig.Max}); err != nil {
		t.Fatalf("收紧 RLIMIT_NOFILE 到 %d 失败: %v", budget, err)
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("读取 RLIMIT_NOFILE 失败: %v", err)
	}
	if limit.Cur != budget {
		t.Fatalf("RLIMIT_NOFILE 回读=%d，期望预算 %d（预算未生效）", limit.Cur, budget)
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
