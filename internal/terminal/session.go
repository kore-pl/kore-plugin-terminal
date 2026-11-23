//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 关闭等待预算。
const (
	// closeWaitTimeout 是 kill 后等待进程退出的时间，超时再补一刀。
	closeWaitTimeout = 3 * time.Second
	// closeKillTimeout 是补刀后的最终等待时间。
	closeKillTimeout = time.Second
)

// Session 是一个 PTY 会话：master fd 归本进程，slave fd 交给登录 shell。
//
// 会话绑定创建者（owner/client_id）：读写/关闭/attach 必须携带同一标识，
// 越权一律 ErrForbidden（实现位置见 checkOwner 与 Manager.session）。
type Session struct {
	// id / owner 在创建后不可变，无需加锁读取。
	id    string
	owner string
	shell string
	pid   int
	pgid  int
	fd    int

	master *os.File

	readMu sync.Mutex // 串行化读取，保证 offset/tail 与 PTY 读一致

	mu         sync.Mutex
	cols       int
	rows       int
	createdAt  time.Time
	lastActive time.Time
	offset     uint64 // 已交付字节总数，单调递增，作为读游标
	tail       []byte // 最近交付字节（重连补读窗口）
	tailCap    int
	eof        bool
	exited     bool
	exitCode   int
	closed     bool

	done      chan struct{}
	closeOnce sync.Once
}

// startSession 启动登录 shell 并接管 PTY master。
func startSession(id, owner, shellPath string, pair ptyPair, cols, rows, replayBytes int, now time.Time) (*Session, error) {
	if err := setWinsize(pair.masterFD, cols, rows); err != nil {
		pair.close()
		return nil, fmt.Errorf("%w: 设置窗口大小: %v", ErrIO, err)
	}
	cmd := exec.Command(shellPath, shellArgs(shellPath)...)
	cmd.Stdin = pair.slave
	cmd.Stdout = pair.slave
	cmd.Stderr = pair.slave
	cmd.Env = childEnv(id, cols, rows)
	// Setsid 新建会话，Setctty 把 stdin（从端）设为控制终端；此后子进程组号 = pid。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		pair.close()
		return nil, fmt.Errorf("%w: 启动 shell %s: %v", ErrIO, shellPath, err)
	}
	pair.closeSlave()
	session := &Session{
		id: id, owner: owner, shell: shellPath, pid: cmd.Process.Pid, pgid: cmd.Process.Pid,
		fd: pair.masterFD, master: pair.master, tailCap: replayBytes,
		cols: cols, rows: rows, createdAt: now, lastActive: now,
		done: make(chan struct{}),
	}
	go session.wait(cmd)
	return session, nil
}

// childEnv 构造子 shell 环境：剔除协议 token，补终端相关变量。
func childEnv(id string, cols, rows int) []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "KORE_PLUGIN_TOKEN=") {
			continue // token 是插件与内核之间的凭据，不进入交互式 shell
		}
		env = append(env, item)
	}
	return append(env,
		"TERM=xterm-256color",
		"KORE_TERMINAL_SESSION="+id,
		"COLUMNS="+strconv.Itoa(cols),
		"LINES="+strconv.Itoa(rows),
	)
}

// wait 等待 shell 退出并记录退出码；每个会话只有一个等待者，不轮询。
func (s *Session) wait(cmd *exec.Cmd) {
	_ = cmd.Wait()
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	s.mu.Lock()
	s.exited = true
	s.exitCode = code
	s.mu.Unlock()
	close(s.done)
}

// checkOwner 校验调用方是否为会话创建者；不一致返回 ErrForbidden（越权 attach 的落点）。
func (s *Session) checkOwner(clientID string) error {
	if s.owner == clientID {
		return nil
	}
	return fmt.Errorf("%w（会话 %s 归属 %q）", ErrForbidden, s.id, s.owner)
}

// info 返回会话快照。
func (s *Session) info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.infoLocked()
}

// infoLocked 在持锁路径构造快照。
func (s *Session) infoLocked() Info {
	return Info{
		SessionID: s.id, ClientID: s.owner, Shell: s.shell, PID: s.pid, Cols: s.cols, Rows: s.rows,
		CreatedAt: s.createdAt.Format(time.RFC3339Nano), LastActive: s.lastActive.Format(time.RFC3339Nano),
		Cursor: s.offset, EOF: s.eof,
	}
}

// markActive 刷新最近活跃时间（空闲回收依据）。
func (s *Session) markActive(now time.Time) {
	s.mu.Lock()
	s.lastActive = now
	s.mu.Unlock()
}

// lastActiveTime 返回最近活跃时间。
func (s *Session) lastActiveTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActive
}

// cursorState 返回当前游标与补读窗口下界（重连续读的判据）。
func (s *Session) cursorState() (offset uint64, windowStart uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offset, s.offset - uint64(len(s.tail))
}

// attach 刷新活跃时间并返回 attach 快照；游标可恢复性在 Manager.Attach 校验。
func (s *Session) attach(now time.Time) Attached {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActive = now
	return Attached{
		Info: s.infoLocked(), Cursor: s.offset, ReplayBytes: len(s.tail),
	}
}

// resize 调整窗口尺寸并记录生效值。
func (s *Session) resize(cols, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := setWinsize(s.fd, cols, rows); err != nil {
		return fmt.Errorf("%w: 调整窗口: %v", ErrIO, err)
	}
	s.cols = cols
	s.rows = rows
	return nil
}

// markEOF 标记输出已读完。
func (s *Session) markEOF() {
	s.mu.Lock()
	s.eof = true
	s.mu.Unlock()
}

// isEOF 返回会话是否已读完退出。
func (s *Session) isEOF() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eof
}

// Close 关闭会话：kill(-pgid) 保证子进程树全退，然后关 fd。
// 用 sync.Once 保证重复调用/并发调用只执行一次。
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		pid := s.pid
		master := s.master
		s.mu.Unlock()

		killGroup(pid)
		select {
		case <-s.done:
		case <-time.After(closeWaitTimeout):
			killGroup(pid) // 3s 未退：再杀一次进程组
			select {
			case <-s.done:
			case <-time.After(closeKillTimeout):
			}
		}
		if master != nil {
			_ = master.Close()
		}
		s.markEOF()
	})
}

// killGroup 先杀进程组（Setsid 后 pgid = pid），失败再按 pid 兜底。
func killGroup(pid int) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
