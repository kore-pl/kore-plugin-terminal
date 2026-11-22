//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package terminal

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// Linux 通用 ioctl 编码（_IOC 宏）下的 PTY 请求码；上述 4 种 64 位小端架构一致。
// 不用第三方库：PTY 的固定流程只有"解锁 + 取编号 + 改窗口大小"三个 ioctl。
const (
	// tiocGPTN 是 _IOR('T', 0x30, unsigned int)：读从端编号。
	tiocGPTN = 0x80045430
	// tiocSPTLCK 是 _IOW('T', 0x31, int)：0 表示解锁。
	tiocSPTLCK = 0x40045431
	// fdSetSize 是 select 的 FD_SETSIZE（Linux 固定 1024）。
	fdSetSize = 1024
)

// winsize 对应 struct winsize（TIOCSWINSZ 参数）。
type winsize struct {
	Rows   uint16
	Cols   uint16
	Xpixel uint16
	Ypixel uint16
}

// ptyPair 是一对伪终端：master 归插件，slave 交给 shell。
type ptyPair struct {
	master   *os.File
	masterFD int
	slave    *os.File
}

// openPTY 按固定流程打开 PTY：/dev/ptmx → TIOCSPTLCK 解锁 → TIOCGPTN 取编号 → /dev/pts/<n>。
func openPTY() (ptyPair, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return ptyPair{}, fmt.Errorf("打开 /dev/ptmx: %w", err)
	}
	pair := ptyPair{master: master, masterFD: int(master.Fd())}
	unlock := int32(0)
	if err := ioctl(pair.masterFD, tiocSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		pair.close()
		return ptyPair{}, fmt.Errorf("解锁 PTY: %w", err)
	}
	var number uint32
	if err := ioctl(pair.masterFD, tiocGPTN, unsafe.Pointer(&number)); err != nil {
		pair.close()
		return ptyPair{}, fmt.Errorf("读取从端编号: %w", err)
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", number)
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		pair.close()
		return ptyPair{}, fmt.Errorf("打开 %s: %w", slavePath, err)
	}
	pair.slave = slave
	// master 用非阻塞语义：读用 select 零超时探活，写用 select 等待可写。
	// 阻塞 fd 会让一次粘贴或一次无输出读取把整个会话循环卡死。
	if err := syscall.SetNonblock(pair.masterFD, true); err != nil {
		pair.close()
		return ptyPair{}, fmt.Errorf("设置 master 非阻塞: %w", err)
	}
	return pair, nil
}

// close 关闭 master 与 slave（slave 已交给子进程时 closeSlave 会置空）。
func (p ptyPair) close() {
	if p.master != nil {
		_ = p.master.Close()
	}
	if p.slave != nil {
		_ = p.slave.Close()
	}
}

// closeSlave 在 exec 成功后关闭父进程持有的从端，保证 master 能收到 EOF/EIO。
func (p ptyPair) closeSlave() {
	if p.slave != nil {
		_ = p.slave.Close()
		p.slave = nil
	}
}

// setWinsize 用 TIOCSWINSZ 设置窗口大小。
func setWinsize(fd int, cols, rows int) error {
	size := winsize{Rows: uint16(rows), Cols: uint16(cols)}
	return ioctl(fd, syscall.TIOCSWINSZ, unsafe.Pointer(&size))
}

// ioctl 是 SYS_IOCTL 的薄封装。
func ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	if fd < 0 {
		return syscall.EBADF
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// waitFD 用 select 探活 fd：timeout=0 时纯轮询，不阻塞调用方。
func waitFD(fd int, write bool, timeout time.Duration) (bool, error) {
	if fd < 0 || fd >= fdSetSize {
		return false, syscall.EBADF
	}
	var set syscall.FdSet
	set.Bits[fd/64] |= 1 << (uint(fd) % 64)
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	var (
		ready int
		err   error
	)
	if write {
		ready, err = syscall.Select(fd+1, nil, &set, nil, &tv)
	} else {
		ready, err = syscall.Select(fd+1, &set, nil, nil, &tv)
	}
	if err == syscall.EINTR {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ready > 0, nil
}
