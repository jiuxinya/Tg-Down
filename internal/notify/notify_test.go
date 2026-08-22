package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	"tg-down/internal/queue"
)

const (
	testWaitTimeout = 2 * time.Second
	// testGrace 是断言"某通道不该被使用"时的观察窗口
	testGrace       = 150 * time.Millisecond
	testChatTitle   = "测试频道"
	testChatID      = int64(-1001234567890)
	testTaskID      = "task-1"
	eventTaskFinish = "task_finished"
	contentTypeJSON = "application/json"
)

// errSelfSend 模拟 Telegram 侧发送失败
var errSelfSend = errors.New("telegram 不可达")

// webhookEnvelope 对应 postWebhook 的外层包装结构
type webhookEnvelope struct {
	Event string        `json:"event"`
	Task  queue.TaskDTO `json:"task"`
}

func testDTO(status queue.Status, errMsg string) *queue.TaskDTO {
	return &queue.TaskDTO{
		ID:        testTaskID,
		Kind:      string(queue.KindHistory),
		ChatID:    testChatID,
		ChatTitle: testChatTitle,
		Status:    string(status),
		Error:     errMsg,
		CreatedAt: time.Now(),
		Stats:     downloader.Stats{Total: 6, Downloaded: 3, Skipped: 2, Failed: 1},
	}
}

// quietLogger 用 error 级别压掉 best-effort 失败路径的 Warn 噪音
func quietLogger() *logger.Logger { return logger.New(logger.LevelError) }

func mustNew(t *testing.T, selfSend func(context.Context, string) error, webhookURL string) *Notifier {
	t.Helper()
	n := New(selfSend, webhookURL, quietLogger())
	if n == nil {
		t.Fatal("New 返回 nil，测试前置条件不成立")
	}
	return n
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testWaitTimeout):
		t.Fatalf("等待 %s 超时", what)
		var zero T
		return zero
	}
}

func assertNoValue[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s 未被禁用，收到了不该发出的通知", what)
	case <-time.After(testGrace):
	}
}

func TestNew(t *testing.T) {
	t.Parallel()
	send := func(context.Context, string) error { return nil }

	tests := []struct {
		name       string
		selfSend   func(context.Context, string) error
		webhookURL string
		wantNil    bool
	}{
		{name: "两个通道都未配置时返回 nil", wantNil: true},
		{name: "仅 Telegram", selfSend: send},
		{name: "仅 webhook", webhookURL: "https://example.invalid/hook"},
		{name: "双通道", selfSend: send, webhookURL: "https://example.invalid/hook"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := New(tc.selfSend, tc.webhookURL, quietLogger())
			if tc.wantNil {
				if n != nil {
					t.Fatalf("New = %+v，want nil（未配置通道时调用方据此跳过接线）", n)
				}
				return
			}
			if n == nil {
				t.Fatal("New = nil，want 非 nil")
			}
			if (n.selfSend != nil) != (tc.selfSend != nil) {
				t.Errorf("selfSend 是否为 nil = %v，want %v", n.selfSend == nil, tc.selfSend == nil)
			}
			if n.webhookURL != tc.webhookURL {
				t.Errorf("webhookURL = %q，want %q", n.webhookURL, tc.webhookURL)
			}
			if n.httpClient == nil || n.httpClient.Timeout != notifyTimeout {
				t.Errorf("httpClient 超时 = %v，want %v", n.httpClient.Timeout, notifyTimeout)
			}
		})
	}
}

func TestFormatMessage(t *testing.T) {
	t.Parallel()

	noTitle := testDTO(queue.StatusCompleted, "")
	noTitle.ChatTitle = ""

	tests := []struct {
		name string
		dto  *queue.TaskDTO
		want string
	}{
		{
			name: "完成",
			dto:  testDTO(queue.StatusCompleted, ""),
			want: "✅ Tg-Down 任务完成：测试频道\n下载 3，跳过 2，失败 1",
		},
		{
			name: "完成且无会话标题时回落到 chat ID",
			dto:  noTitle,
			want: "✅ Tg-Down 任务完成：ID -1001234567890\n下载 3，跳过 2，失败 1",
		},
		{
			name: "失败",
			dto:  testDTO(queue.StatusFailed, "连接超时"),
			want: "❌ Tg-Down 任务失败：测试频道\n连接超时",
		},
		{
			// partial 是独立终态（历史已扫完，只是部分文件失败），不能报成彻底失败——
			// 那会把已下载/已跳过的数量一并丢掉
			name: "部分完成单独成一档",
			dto:  testDTO(queue.StatusPartial, "1 个文件下载失败"),
			want: "⚠️ Tg-Down 任务部分完成：测试频道\n下载 3，跳过 2，失败 1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := formatMessage(tc.dto); got != tc.want {
				t.Errorf("formatMessage() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestPostWebhookPayload(t *testing.T) {
	t.Parallel()

	type capture struct {
		method      string
		contentType string
		body        []byte
	}
	got := make(chan capture, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		got <- capture{method: r.Method, contentType: r.Header.Get("Content-Type"), body: body}
	}))
	t.Cleanup(srv.Close)

	dto := testDTO(queue.StatusCompleted, "")
	if err := mustNew(t, nil, srv.URL).postWebhook(context.Background(), dto); err != nil {
		t.Fatalf("postWebhook 返回错误: %v", err)
	}

	req := waitFor(t, got, "webhook 请求")
	if req.method != http.MethodPost {
		t.Errorf("method = %s，want %s", req.method, http.MethodPost)
	}
	if req.contentType != contentTypeJSON {
		t.Errorf("Content-Type = %q，want %q", req.contentType, contentTypeJSON)
	}
	var env webhookEnvelope
	if err := json.Unmarshal(req.body, &env); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v（body=%s）", err, req.body)
	}
	if env.Event != eventTaskFinish {
		t.Errorf("event = %q，want %q", env.Event, eventTaskFinish)
	}
	if env.Task.ID != dto.ID || env.Task.ChatID != dto.ChatID || env.Task.Status != dto.Status {
		t.Errorf("task = %+v，want id/chat_id/status 与 %+v 一致", env.Task, dto)
	}
	if env.Task.Stats != dto.Stats {
		t.Errorf("task.stats = %+v，want %+v", env.Task.Stats, dto.Stats)
	}
}

func TestPostWebhookStatusCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "200", status: http.StatusOK},
		{name: "201", status: http.StatusCreated},
		{name: "204", status: http.StatusNoContent},
		{name: "300 起视为失败", status: http.StatusMultipleChoices, wantErr: true},
		{name: "400", status: http.StatusBadRequest, wantErr: true},
		{name: "404", status: http.StatusNotFound, wantErr: true},
		{name: "500", status: http.StatusInternalServerError, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)

			err := mustNew(t, nil, srv.URL).postWebhook(context.Background(), testDTO(queue.StatusCompleted, ""))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("postWebhook 返回 nil，want 错误（HTTP %d）", tc.status)
				}
				if !strings.Contains(err.Error(), "webhook 返回") {
					t.Errorf("错误信息 = %q，want 含 %q", err.Error(), "webhook 返回")
				}
				return
			}
			if err != nil {
				t.Fatalf("postWebhook 返回错误 %v，want nil（HTTP %d）", err, tc.status)
			}
		})
	}
}

func TestPostWebhookContextEnded(t *testing.T) {
	t.Parallel()

	t.Run("context 已取消", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := mustNew(t, nil, srv.URL).postWebhook(ctx, testDTO(queue.StatusCompleted, "")); err == nil {
			t.Fatal("postWebhook 返回 nil，want context 取消错误")
		}
	})

	t.Run("context 超时", func(t *testing.T) {
		t.Parallel()
		// 处理器挂起到客户端断开为止，让 ctx deadline 先到期。必须先读完请求体：net/http 只有在请求体
		// 读到 EOF 后才启动后台读，否则服务端感知不到客户端断开，r.Context() 永不取消，Close 会死锁。
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(func() {
			close(release)
			srv.Close()
		})

		ctx, cancel := context.WithTimeout(context.Background(), testGrace)
		defer cancel()
		err := mustNew(t, nil, srv.URL).postWebhook(ctx, testDTO(queue.StatusCompleted, ""))
		if err == nil {
			t.Fatal("postWebhook 返回 nil，want 超时错误")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("错误 = %v，want 包裹 context.DeadlineExceeded", err)
		}
	})
}

func TestPostWebhookTransportErrors(t *testing.T) {
	t.Parallel()

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	tests := []struct {
		name string
		url  string
	}{
		{name: "服务端不可达", url: closedURL},
		{name: "非法 URL", url: "://not-a-url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := mustNew(t, nil, tc.url).postWebhook(context.Background(), testDTO(queue.StatusCompleted, "")); err == nil {
				t.Fatal("postWebhook 返回 nil，want 错误")
			}
		})
	}
}

func TestTaskFinishedRoutesByConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		withTelegam bool
		withWebhook bool
	}{
		{name: "仅 Telegram", withTelegam: true},
		{name: "仅 webhook", withWebhook: true},
		{name: "双通道", withTelegam: true, withWebhook: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			hooked := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				hooked <- body
			}))
			t.Cleanup(srv.Close)

			sent := make(chan string, 1)
			var selfSend func(context.Context, string) error
			if tc.withTelegam {
				selfSend = func(_ context.Context, text string) error {
					sent <- text
					return nil
				}
			}
			webhookURL := ""
			if tc.withWebhook {
				webhookURL = srv.URL
			}

			dto := testDTO(queue.StatusCompleted, "")
			mustNew(t, selfSend, webhookURL).TaskFinished(dto)

			if tc.withTelegam {
				if got := waitFor(t, sent, "Telegram 通知"); got != formatMessage(dto) {
					t.Errorf("Telegram 文本 = %q，want %q", got, formatMessage(dto))
				}
			}
			if tc.withWebhook {
				var env webhookEnvelope
				if err := json.Unmarshal(waitFor(t, hooked, "webhook 请求"), &env); err != nil {
					t.Fatalf("请求体不是合法 JSON: %v", err)
				}
				if env.Event != eventTaskFinish || env.Task.ID != dto.ID {
					t.Errorf("payload = %+v，want event=%s task.id=%s", env, eventTaskFinish, dto.ID)
				}
				return
			}
			assertNoValue(t, hooked, "webhook 通道")
		})
	}
}

func TestTaskFinishedFailuresAreLoggedNotPropagated(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	warns := make(chan string, 4)
	log := logger.New(logger.LevelWarn)
	log.SetHook(func(level, msg string) {
		if level == "WARN" {
			warns <- msg
		}
	})

	n := New(func(context.Context, string) error { return errSelfSend }, srv.URL, log)
	if n == nil {
		t.Fatal("New 返回 nil，测试前置条件不成立")
	}
	n.TaskFinished(testDTO(queue.StatusFailed, "连接超时")) // 两个通道都会失败，但不得 panic / 阻塞调用方

	var telegramWarned, webhookWarned bool
	for range 2 {
		msg := waitFor(t, warns, "失败告警日志")
		switch {
		case strings.Contains(msg, "Telegram 通知发送失败"):
			telegramWarned = true
		case strings.Contains(msg, "webhook 通知发送失败"):
			webhookWarned = true
		default:
			t.Errorf("非预期告警: %q", msg)
		}
	}
	if !telegramWarned || !webhookWarned {
		t.Errorf("告警覆盖 telegram=%v webhook=%v，want 两者皆 true", telegramWarned, webhookWarned)
	}
}

func TestTaskFinishedChannelsDoNotBlockEachOther(t *testing.T) {
	t.Parallel()

	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	selfStarted := make(chan struct{})
	selfSend := func(context.Context, string) error {
		close(selfStarted)
		<-release
		return nil
	}

	n := mustNew(t, selfSend, srv.URL)
	n.TaskFinished(testDTO(queue.StatusCompleted, ""))

	select {
	case <-selfStarted:
	case <-time.After(testWaitTimeout):
		t.Fatal("Telegram 通知未启动")
	}
	select {
	case <-received:
	case <-time.After(testWaitTimeout):
		t.Fatal("Telegram 通知阻塞时 webhook 也未发送")
	}
}
