package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestInstanceLockIsExclusive 第二次获取必须失败，且失败要能与"取锁出错"区分开
func TestInstanceLockIsExclusive(t *testing.T) {
	dir := t.TempDir()

	first, err := AcquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("首次取锁失败: %v", err)
	}

	if _, err := AcquireInstanceLock(dir); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("二次取锁 = %v, want ErrAlreadyRunning", err)
	}

	// 释放后应可再次取得（正常退出后重启）
	first.Release()
	second, err := AcquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("释放后重新取锁失败: %v", err)
	}
	second.Release()
}

func TestShellURLRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if err := ShowRunningInstance(dir); err == nil {
		t.Error("没有地址记录时应报错")
	}
	if err := PublishShellURL(dir, "http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, shellURLFile)); err != nil {
		t.Fatalf("地址记录未落盘: %v", err)
	}
	ClearShellURL(dir)
	if _, err := os.Stat(filepath.Join(dir, shellURLFile)); !os.IsNotExist(err) {
		t.Error("退出时应清掉地址记录，避免下次对着死端口发请求")
	}
}
