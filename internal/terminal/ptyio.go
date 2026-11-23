//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package terminal

import (
	"fmt"
	"syscall"
	"time"
)

// 写与关闭的等待预算。
const (
	// writeWaitTimeout 是单次等待 PTY 可写的时间；缓冲区满时 100ms 后回 ErrTimeout。
	writeWaitTimeout = 100 * time.Millisecond
	// writeAttempts 是写循环的最大轮次，防止异常 fd 上无限重试。
	writeAttempts = 64
)

// read 读取会话输出。cursor 是客户端已消费的绝对字节偏移（下次期望位置）：
//
//   - nil 或等于当前 offset：增量读取新输出（既有语义，返回空串表示暂时没有输出）；
//   - 小于 offset 且落在补读窗口内：从窗口回放未确认字节（重连续读、不重复吐已读内容）；
//   - 早于窗口下界：ErrCursorExpired（客户端必须重新对齐，例如重新 create）；
//   - 大于 offset：ErrInvalid（游标超前，说明状态不一致）。
//
// offset 单调递增且只由本方法推进，因此同一游标重复读不会产生新字节。
func (s *Session) read(cursor *uint64, maxBytes int) (ReadResult, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ReadResult{}, ErrClosed
	}
	if cursor != nil {
		if *cursor > s.offset {
			offset := s.offset
			s.mu.Unlock()
			return ReadResult{}, fmt.Errorf("%w: 读游标 %d 超过当前偏移 %d", ErrInvalid, *cursor, offset)
		}
		if *cursor < s.offset {
			result, err := s.replayLocked(*cursor, maxBytes)
			s.mu.Unlock()
			return result, err
		}
	}
	if s.eof {
		offset := s.offset
		s.mu.Unlock()
		return ReadResult{EOF: true, Cursor: offset}, nil
	}
	fd := s.fd
	s.mu.Unlock()

	data, eof, err := readPTY(fd, maxBytes)
	if err != nil {
		return ReadResult{}, err
	}
	s.mu.Lock()
	if len(data) > 0 {
		s.appendTailLocked(data)
	}
	if eof {
		s.eof = true
	}
	result := ReadResult{Data: data, Cursor: s.offset, EOF: s.eof}
	s.mu.Unlock()
	return result, nil
}

// replayLocked 从补读窗口回放 [cursor, offset) 区间（调用方持锁）。
func (s *Session) replayLocked(cursor uint64, maxBytes int) (ReadResult, error) {
	windowStart := s.offset - uint64(len(s.tail))
	if cursor < windowStart {
		offset, start := s.offset, windowStart
		return ReadResult{}, fmt.Errorf("%w: 游标 %d 早于可补读下界 %d（当前偏移 %d），请重新对齐",
			ErrCursorExpired, cursor, start, offset)
	}
	from := int(cursor - windowStart)
	end := from + maxBytes
	if end > len(s.tail) {
		end = len(s.tail)
	}
	data := append([]byte(nil), s.tail[from:end]...)
	next := cursor + uint64(len(data))
	return ReadResult{Data: data, Cursor: next, Replay: true, EOF: s.eof && next >= s.offset}, nil
}

// appendTailLocked 把新交付字节追加到补读窗口并按窗口上限裁剪（调用方持锁）。
func (s *Session) appendTailLocked(data []byte) {
	s.offset += uint64(len(data))
	if s.tailCap <= 0 {
		return
	}
	s.tail = append(s.tail, data...)
	if len(s.tail) > s.tailCap {
		s.tail = append(s.tail[:0], s.tail[len(s.tail)-s.tailCap:]...)
	}
}

// readPTY 从非阻塞 master 增量读取：select 零超时探活，无数据返回空串，EIO 表示 shell 退出。
func readPTY(fd, maxBytes int) ([]byte, bool, error) {
	buf := make([]byte, maxBytes)
	read := 0
	for read < maxBytes {
		readable, err := waitFD(fd, false, 0)
		if err != nil {
			return nil, false, fmt.Errorf("%w: 等待可读: %v", ErrIO, err)
		}
		if !readable {
			break // 当前没有输出：增量语义返回空串，不阻塞
		}
		n, rerr := syscall.Read(fd, buf[read:])
		if n > 0 {
			read += n
			continue
		}
		if rerr == syscall.EAGAIN || rerr == syscall.EWOULDBLOCK {
			break
		}
		if rerr == syscall.EIO || rerr == nil {
			return buf[:read], true, nil
		}
		return nil, false, fmt.Errorf("%w: 读取 PTY: %v", ErrIO, rerr)
	}
	return buf[:read], false, nil
}

// write 写 master（即 shell 的 stdin）：可写等待 100ms，缓冲满回 ErrTimeout。
func (s *Session) write(data []byte) (int, error) {
	s.mu.Lock()
	if s.closed || s.eof || s.exited {
		s.mu.Unlock()
		return 0, ErrClosed
	}
	fd := s.fd
	s.mu.Unlock()

	written := 0
	for attempt := 0; attempt < writeAttempts && written < len(data); attempt++ {
		writable, err := waitFD(fd, true, writeWaitTimeout)
		if err != nil {
			return written, fmt.Errorf("%w: 等待可写: %v", ErrIO, err)
		}
		if !writable {
			return written, ErrTimeout
		}
		n, werr := syscall.Write(fd, data[written:])
		if n > 0 {
			written += n
			continue
		}
		if werr == syscall.EAGAIN || werr == syscall.EWOULDBLOCK {
			continue
		}
		if werr == syscall.EIO || werr == nil {
			return written, ErrClosed
		}
		return written, fmt.Errorf("%w: 写入 PTY: %v", ErrIO, werr)
	}
	if written < len(data) {
		return written, ErrTimeout
	}
	return written, nil
}
