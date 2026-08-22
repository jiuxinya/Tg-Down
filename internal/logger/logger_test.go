package logger

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	testMsg = "hello"
	// fatalSubprocessEnv 用于把当前测试进程重新拉起为 Fatal 的被测子进程
	fatalSubprocessEnv = "TGDOWN_LOGGER_FATAL_SUBPROCESS"
)

var allLevelTags = []string{"DEBUG", "INFO", "WARN", "ERROR"}

// captureStdout 替换全局 os.Stdout 抓取输出。output/Fatal 直接用 fmt.Println 写标准输出，
// 没有可注入的 io.Writer，只能靠换全局；因此这些用例禁止 t.Parallel()。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	fn()

	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatalf("关闭管道写端失败: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读取管道失败: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("关闭管道读端失败: %v", err)
	}
	return string(data)
}

func TestNewParsesLevel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  LogLevel
	}{
		{"debug", LevelDebug, DEBUG},
		{"info", LevelInfo, INFO},
		{"warn", LevelWarn, WARN},
		{"error", LevelError, ERROR},
		{"大写不敏感", "ERROR", ERROR},
		{"混合大小写", "WaRn", WARN},
		{"空字符串回落 info", "", INFO},
		{"未知级别回落 info", "verbose", INFO},
		{"两侧空格不做裁剪，回落 info", " debug ", INFO},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := New(tt.input).level; got != tt.want {
				t.Errorf("New(%q).level = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestLevelFiltering(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		wantTags   []string
	}{
		{"debug 放行全部", LevelDebug, []string{"DEBUG", "INFO", "WARN", "ERROR"}},
		{"info 挡掉 debug", LevelInfo, []string{"INFO", "WARN", "ERROR"}},
		{"warn 只留 warn 及以上", LevelWarn, []string{"WARN", "ERROR"}},
		{"error 只留 error", LevelError, []string{"ERROR"}},
		{"未知级别按 info 过滤", "nonsense", []string{"INFO", "WARN", "ERROR"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := New(tt.configured)
			var hooked []string
			l.SetHook(func(level, _ string) { hooked = append(hooked, level) })

			out := captureStdout(t, func() {
				l.Debug(testMsg)
				l.Info(testMsg)
				l.Warn(testMsg)
				l.Error(testMsg)
			})

			for _, tag := range allLevelTags {
				want := slices.Contains(tt.wantTags, tag)
				if got := strings.Contains(out, tag+": "); got != want {
					t.Errorf("级别 %s 输出存在性 = %v, want %v；实际输出:\n%s", tag, got, want, out)
				}
			}
			if lines := nonEmptyLines(out); len(lines) != len(tt.wantTags) {
				t.Errorf("输出行数 = %d, want %d；实际输出:\n%s", len(lines), len(tt.wantTags), out)
			}
			if !slices.Equal(hooked, tt.wantTags) {
				t.Errorf("回调收到的级别 = %v, want %v", hooked, tt.wantTags)
			}
		})
	}
}

func nonEmptyLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestOutputFormat(t *testing.T) {
	l := New(LevelDebug)
	out := captureStdout(t, func() {
		l.Info("下载 %s 完成，共 %d 项", "album", 3)
	})

	want := regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\] INFO: 下载 album 完成，共 3 项\n$`)
	if !want.MatchString(out) {
		t.Errorf("输出格式不匹配 %q，实际: %q", want.String(), out)
	}
}

func TestFormatMessage(t *testing.T) {
	got := New(LevelInfo).formatMessage("WARN", testMsg)
	want := regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\] WARN: hello$`)
	if !want.MatchString(got) {
		t.Errorf("formatMessage = %q, 不匹配 %q", got, want.String())
	}
}

func TestHookReceivesFormattedMessageWithoutPrefix(t *testing.T) {
	l := New(LevelDebug)
	var gotLevel, gotMsg string
	l.SetHook(func(level, msg string) {
		gotLevel, gotMsg = level, msg
	})

	captureStdout(t, func() { l.Warn("重试 %d 次", 2) })

	if gotLevel != "WARN" {
		t.Errorf("回调级别 = %q, want %q", gotLevel, "WARN")
	}
	if gotMsg != "重试 2 次" {
		t.Errorf("回调消息 = %q, want %q（不含时间戳与级别前缀）", gotMsg, "重试 2 次")
	}
}

func TestSetHookOverridesAndClears(t *testing.T) {
	l := New(LevelInfo)
	first := 0
	l.SetHook(func(_, _ string) { first++ })

	second := 0
	l.SetHook(func(_, _ string) { second++ })
	captureStdout(t, func() { l.Info(testMsg) })

	if first != 0 {
		t.Errorf("旧回调仍被调用 %d 次, want 0", first)
	}
	if second != 1 {
		t.Errorf("新回调调用 %d 次, want 1", second)
	}

	l.SetHook(nil)
	out := captureStdout(t, func() { l.Info(testMsg) })
	if second != 1 {
		t.Errorf("清空回调后仍被调用，累计 %d 次, want 1", second)
	}
	if !strings.Contains(out, "INFO: "+testMsg) {
		t.Errorf("清空回调不应影响标准输出，实际: %q", out)
	}
}

func TestNoHookDoesNotPanic(t *testing.T) {
	out := captureStdout(t, func() { New(LevelDebug).Debug(testMsg) })
	if !strings.Contains(out, "DEBUG: "+testMsg) {
		t.Errorf("未注册回调时应正常输出，实际: %q", out)
	}
}

func TestConcurrentLogAndSetHook(t *testing.T) {
	l := New(LevelDebug)
	var mu sync.Mutex
	count := 0

	const workers = 8
	captureStdout(t, func() {
		var wg sync.WaitGroup
		wg.Add(workers * 2)
		for range workers {
			go func() {
				defer wg.Done()
				l.Info(testMsg)
			}()
			go func() {
				defer wg.Done()
				l.SetHook(func(_, _ string) {
					mu.Lock()
					count++
					mu.Unlock()
				})
			}()
		}
		wg.Wait()
	})

	// 回调注册与日志写入交错，命中次数不确定，只断言不越界且无数据竞争（-race）
	mu.Lock()
	defer mu.Unlock()
	if count > workers {
		t.Errorf("回调命中 %d 次，超过日志总数 %d", count, workers)
	}
}

func TestFatalPrintsAndExits(t *testing.T) {
	if os.Getenv(fatalSubprocessEnv) == "1" {
		l := New(LevelError)
		l.SetHook(func(level, msg string) {
			if _, err := os.Stdout.WriteString("HOOK " + level + " " + msg + "\n"); err != nil {
				panic(err)
			}
		})
		l.Fatal("崩溃于 %s", "step-1")
		// Fatal 内部 os.Exit，正常不可达
		t.Fatal("Fatal 未退出进程")
	}

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFatalPrintsAndExits$", "-test.v")
	cmd.Env = append(os.Environ(), fatalSubprocessEnv+"=1")
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("子进程应以非零码退出，err = %v，输出:\n%s", err, out)
	}
	if code := exitErr.ExitCode(); code != ExitCodeFatal {
		t.Errorf("退出码 = %d, want %d", code, ExitCodeFatal)
	}

	text := string(out)
	fatalLine := regexp.MustCompile(`\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\] FATAL: 崩溃于 step-1`)
	if !fatalLine.MatchString(text) {
		t.Errorf("未找到 FATAL 输出行，子进程输出:\n%s", text)
	}
	// Fatal 不受级别过滤影响，且退出前会触发回调
	if !strings.Contains(text, "HOOK FATAL 崩溃于 step-1") {
		t.Errorf("Fatal 未触发回调，子进程输出:\n%s", text)
	}
}
