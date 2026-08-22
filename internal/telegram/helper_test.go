package telegram

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"tg-down/internal/config"
	"tg-down/internal/logger"
)

// newTestClient 构造一个未连接 TDLib 的 Client。
// 这是 tdAPI 接口缝带来的能力：在此之前，Client 的任何逻辑都必须先建立真实的 TDLib 连接
// 才能触达，本包因此长期零测试。
func newTestClient(t *testing.T) *Client {
	t.Helper()
	cfg := &config.Config{}
	cfg.Download.Path = t.TempDir()
	cfg.Download.MaxConcurrent = 1
	cfg.Session.Dir = t.TempDir()
	cfg.Retry.MaxRetries = 3
	cfg.Retry.BaseDelay = 1
	cfg.Retry.MaxDelay = 30
	return newClient(cfg, logger.New("error"), 0)
}

func TestReadPasswordInput(t *testing.T) {
	t.Run("terminal uses hidden reader", func(t *testing.T) {
		var out bytes.Buffer
		called := false
		got, err := readPasswordInput(strings.NewReader("visible\n"), &out, true, func() ([]byte, error) {
			called = true
			return []byte("hidden-secret"), nil
		})
		if err != nil || got != "hidden-secret" {
			t.Fatalf("readPasswordInput() = %q, %v", got, err)
		}
		if !called {
			t.Fatal("终端输入未调用隐藏读取函数")
		}
		if !strings.HasSuffix(out.String(), "\n") {
			t.Errorf("隐藏输入后未补换行: %q", out.String())
		}
		if strings.Contains(out.String(), got) {
			t.Fatal("密码被写入终端输出")
		}
	})

	t.Run("pipe reads one token without hidden reader", func(t *testing.T) {
		var out bytes.Buffer
		got, err := readPasswordInput(strings.NewReader("pipe-secret\n"), &out, false, func() ([]byte, error) {
			t.Fatal("管道输入不应调用隐藏读取函数")
			return nil, nil
		})
		if err != nil || got != "pipe-secret" {
			t.Fatalf("readPasswordInput() = %q, %v", got, err)
		}
	})

	t.Run("hidden reader error does not expose password", func(t *testing.T) {
		var out bytes.Buffer
		sentinel := errors.New("read failed")
		_, err := readPasswordInput(nil, &out, true, func() ([]byte, error) {
			return nil, sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want sentinel", err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("错误文本泄露密码: %v", err)
		}
	})
}

func TestCopyFileUsesUniqueTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.bin")
	dst := filepath.Join(dir, "target.bin")
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 旧实现固定使用 target.bin.part，并会跟随这个链接截断外部文件。
	if err := os.Symlink(outside, dst+".part"); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile() error = %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "payload" {
		t.Fatalf("目标内容 = %q, %v", got, err)
	}
	got, err = os.ReadFile(outside)
	if err != nil || string(got) != "unchanged" {
		t.Fatalf("固定临时路径的外部目标被修改: %q, %v", got, err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	// Windows 的 Chmod 只切换只读位，Stat 不回传 POSIX 权限，跳过断言
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("复制文件权限 = %o, want 600", info.Mode().Perm())
	}
}
