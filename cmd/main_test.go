package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrintUsage(t *testing.T) {
	oldVersion := version
	version = "test-version"
	t.Cleanup(func() { version = oldVersion })

	var out bytes.Buffer
	printUsage(&out)
	text := out.String()
	for _, want := range []string{"tg-down test-version", "--web", "--clear-session", "--version", "--help"} {
		if !strings.Contains(text, want) {
			t.Errorf("帮助文本缺少 %q：\n%s", want, text)
		}
	}
}

func TestSelectMode(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  int
	}{
		{input: "1\n", want: ModeDownloadHistory},
		{input: "2\n", want: ModeMonitorNewMessages},
		{input: "3\n", want: ModeDownloadAndMonitor},
	} {
		t.Run(strings.TrimSpace(tt.input), func(t *testing.T) {
			var output bytes.Buffer
			got, err := selectMode(strings.NewReader(tt.input), &output)
			if err != nil {
				t.Fatalf("selectMode() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("selectMode() = %d, want %d", got, tt.want)
			}
			if !strings.Contains(output.String(), "请选择模式") {
				t.Fatalf("提示文本缺失: %q", output.String())
			}
		})
	}
}

func TestSelectModeRejectsInvalidOrMissingInput(t *testing.T) {
	for _, input := range []string{"", "0\n", "4\n", "download\n"} {
		t.Run(input, func(t *testing.T) {
			if _, err := selectMode(strings.NewReader(input), &bytes.Buffer{}); err == nil {
				t.Fatalf("selectMode(%q) accepted invalid input", input)
			}
		})
	}
}

func TestClearSessionWithoutCredentials(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("API_ID", "not-a-number")
	t.Setenv("API_HASH", "")
	t.Setenv("PHONE", "")
	t.Setenv("MAX_RETRIES", "many")
	sessionRoot := filepath.Join(root, "sessions")
	t.Setenv("SESSION_DIR", sessionRoot)
	t.Setenv("DOWNLOAD_PATH", filepath.Join(root, "downloads"))

	dbDir := filepath.Join(sessionRoot, "tdlib")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, "db.bin"), []byte("session"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := clearSession(); err != nil {
		t.Fatalf("clearSession() error = %v", err)
	}
	if _, err := os.Stat(dbDir); !os.IsNotExist(err) {
		t.Fatalf("会话目录仍存在或检查失败: %v", err)
	}
}

func TestClearSessionRejectsAncestorSymlink(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	outside := t.TempDir()
	outDB := filepath.Join(outside, "sessions", "tdlib")
	if err := os.MkdirAll(outDB, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(outDB, "db.bin")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked-parent")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	t.Setenv("SESSION_DIR", filepath.Join(link, "sessions"))
	t.Setenv("API_ID", "")
	t.Setenv("API_HASH", "")
	t.Setenv("PHONE", "")

	if err := clearSession(); err == nil {
		t.Fatal("祖先路径含符号链接时应拒绝清理会话")
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "keep" {
		t.Fatalf("外部会话文件被删除或修改: %q, %v", got, err)
	}
}
