//go:build unix

package desktop

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// flock 的锁随文件描述符关闭或进程退出自动释放，崩溃后不会留下需要人工清理的陈旧锁。
func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return ErrAlreadyRunning
	}
	return err
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
