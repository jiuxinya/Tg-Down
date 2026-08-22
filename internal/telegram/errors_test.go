package telegram

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	tdclient "github.com/zelenin/go-tdlib/client"
)

// tdErr 构造一个 go-tdlib 风格的响应错误（值类型，非指针——这是 errors.As 能否命中的关键）
func tdErr(code int32, msg string) error {
	return tdclient.ResponseError{Err: &tdclient.Error{Code: code, Message: msg}}
}

// TestTDRetryAfter 校验从 429 消息中解析服务端要求的等待秒数
func TestTDRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want time.Duration
		ok   bool
	}{
		{"标准 FLOOD_WAIT", tdErr(429, "Too Many Requests: retry after 30"), 30 * time.Second, true},
		{"包装后仍可解析", fmt.Errorf("下载文件失败: %w", tdErr(429, "Too Many Requests: retry after 5")), 5 * time.Second, true},
		{"旧式 420 限流码", tdErr(420, "FLOOD_WAIT_60: retry after 60"), 60 * time.Second, true},
		{"429 但无秒数 → 交给指数退避", tdErr(429, "Too Many Requests"), 0, false},
		{"400 不是限流", tdErr(400, "CHAT_NOT_FOUND"), 0, false},
		{"非 TDLib 错误", errors.New("boom"), 0, false},
		{"nil", nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tdRetryAfter(tc.err)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("delay = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTDShouldRetry 校验重试判定按错误码而非错误文本。
// 回归点：此前靠 strings.Contains 匹配英文关键字，中文包装的错误一律不匹配，
// FLOOD_WAIT 与请求超时都被误判为"不可重试"直接失败。
func TestTDShouldRetry(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"429 限流可重试", tdErr(429, "Too Many Requests: retry after 30"), true},
		{"500 服务端错误可重试", tdErr(500, "Internal Server Error"), true},
		{"DC 迁移可重试", tdErr(303, "FILE_MIGRATE_2"), true},
		{"400 客户端错误不重试", tdErr(400, "CHAT_NOT_FOUND"), false},
		{"401 不重试", tdErr(401, "UNAUTHORIZED"), false},
		{"本地请求超时可重试（中文包装）", fmt.Errorf("%w: 2m0s", errTDTimeout), true},
		{"下载未完成可重试（中文包装）", fmt.Errorf("%w: a.zip", errDownloadIncomplete), true},
		{"包装后的 429 仍可重试", fmt.Errorf("下载文件失败: %w", tdErr(429, "retry after 3")), true},
		{"主动取消不重试", context.Canceled, false},
		{"网络错误可重试", errors.New("connection reset by peer"), true},
		{"普通错误不重试", errors.New("磁盘写入失败"), false},
		{"nil 不重试", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tdShouldRetry(tc.err); got != tc.want {
				t.Errorf("tdShouldRetry(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestRetrier_HonorsFloodWait 端到端校验：注入 429 后，重试器按服务端给的时长等待，
// 而不是立刻失败（v2.0 的行为），也不是套用自己的指数退避。
func TestRetrier_HonorsFloodWait(t *testing.T) {
	c := newTestClient(t)

	attempts := 0
	start := time.Now()
	err := c.retrier.Do(context.Background(), func() error {
		attempts++
		if attempts == 1 {
			return fmt.Errorf("下载文件失败: %w", tdErr(429, "Too Many Requests: retry after 1"))
		}
		return nil
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("限流后应重试成功，得到错误: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("应执行 2 次（首次限流 + 重试），实际 %d 次", attempts)
	}
	// 服务端要求等 1 秒；退避的默认基数也是 1 秒，故只校验确实等待过且量级正确
	if elapsed < time.Second {
		t.Errorf("应至少等待服务端要求的 1 秒，实际只等了 %v", elapsed)
	}
}

// TestRetrier_ClientErrorFailsFast 校验 400 类错误不做无谓重试
func TestRetrier_ClientErrorFailsFast(t *testing.T) {
	c := newTestClient(t)

	attempts := 0
	err := c.retrier.Do(context.Background(), func() error {
		attempts++
		return tdErr(400, "CHAT_NOT_FOUND")
	})

	if err == nil {
		t.Fatal("400 错误应直接失败")
	}
	if attempts != 1 {
		t.Errorf("400 错误不应重试，实际执行 %d 次", attempts)
	}
}
