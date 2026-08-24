//go:build windows

package desktop

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// LockFileEx 的锁随句柄关闭或进程退出自动释放，崩溃后不会留下需要人工清理的陈旧锁。
func lockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return ErrAlreadyRunning
	}
	return err
}

func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
