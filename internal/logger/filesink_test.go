package logger

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// filesink 支撑桌面端默认开启的落盘日志（10MB 轮转 ×3），此前零覆盖。

func TestNewFileSink_RejectsEmptyPath(t *testing.T) {
	if _, err := newFileSink("", 100, 2); err == nil {
		t.Error("空路径应当报错")
	}
}

func TestNewFileSink_CreatesDirAndAppends(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	path := filepath.Join(dir, "app.log")

	s, err := newFileSink(path, 1024, 3)
	if err != nil {
		t.Fatalf("newFileSink() error = %v", err)
	}
	s.writeLine("first")
	s.close()

	// 重新打开必须追加而非截断：进程重启不应该把上次的日志清掉
	s2, err := newFileSink(path, 1024, 3)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	s2.writeLine("second")
	s2.close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("日志内容 = %q，两次写入都应保留", got)
	}
}

func TestFileSink_RotatesAtThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rot.log")
	// 阈值取得很小，写几行就触发轮转
	s, err := newFileSink(path, 32, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	for i := range 20 {
		s.writeLine(strings.Repeat("x", 20) + string(rune('a'+i%26)))
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("当前日志文件不存在: %v", err)
	}
	if _, err := os.Stat(rotName(path, 1)); err != nil {
		t.Errorf("轮转文件 .1 不存在: %v", err)
	}
}

// TestFileSink_KeepsAtMostKeepBackups 是轮转的关键约束：备份数必须有上限，
// 否则长时间运行的桌面端会把磁盘写满。
func TestFileSink_KeepsAtMostKeepBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cap.log")
	const keep = 2
	s, err := newFileSink(path, 16, keep)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	for range 50 {
		s.writeLine(strings.Repeat("y", 32))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var backups int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "cap.log.") {
			backups++
		}
	}
	if backups > keep {
		t.Errorf("备份文件 %d 个，超过上限 %d", backups, keep)
	}
	if backups == 0 {
		t.Error("没有产生任何备份文件，轮转未发生")
	}
}

func TestFileSink_DefaultsOnNonPositiveArgs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.log")
	s, err := newFileSink(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if s.max != DefaultFileLogMaxBytes {
		t.Errorf("max = %d，期望回落默认值 %d", s.max, DefaultFileLogMaxBytes)
	}
	if s.keep != DefaultFileLogKeep {
		t.Errorf("keep = %d，期望回落默认值 %d", s.keep, DefaultFileLogKeep)
	}
}

// TestFileSink_WriteAfterCloseIsSafe 校验关闭后再写不 panic：
// 日志故障不能反过来打断下载主流程。
func TestFileSink_WriteAfterCloseIsSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s, err := newFileSink(path, 1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	s.close()
	s.close() // 重复关闭同样安全
	s.writeLine("after close")
}

func TestFileSink_ConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conc.log")
	s, err := newFileSink(path, 64, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := range 25 {
				s.writeLine(strings.Repeat("z", 10) + string(rune('0'+(n+j)%10)))
			}
		}(i)
	}
	wg.Wait()
}

func TestRotName(t *testing.T) {
	if got := rotName("/tmp/a.log", 2); got != "/tmp/a.log.2" {
		t.Errorf("rotName() = %q", got)
	}
}
