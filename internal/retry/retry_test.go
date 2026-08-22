package retry

import (
	"context"
	"errors"
	"testing"
	"time"

	"tg-down/internal/logger"
)

const (
	tinyDelay = time.Millisecond
	// slowBackoff 远大于测试中使用的服务端等待时长：一旦指数退避被误用在
	// FLOOD_WAIT 路径上，用例会因为耗时/延迟值不符而立刻失败。
	slowBackoff = 250 * time.Millisecond
	floodDelay  = 2 * time.Millisecond
)

var (
	errTransient = errors.New("connection reset by peer")
	errFatal     = errors.New("FILE_REFERENCE_EXPIRED")
	errFlood     = errors.New("FLOOD_WAIT_42")
)

// testLogger 以 error 级别构造，屏蔽 Retrier 内部的 Debug/Info/Warn 输出，保持测试静默。
func testLogger() *logger.Logger {
	return logger.New(logger.LevelError)
}

// retryLog 记录 OnRetry 回调收到的重试序号与实际等待时长。
type retryLog struct {
	attempts []int
	delays   []time.Duration
}

func (l *retryLog) hook() func(int, error, time.Duration) {
	return func(attempt int, _ error, delay time.Duration) {
		l.attempts = append(l.attempts, attempt)
		l.delays = append(l.delays, delay)
	}
}

// failNTimes 返回一个前 n 次失败、之后成功的函数，并回传调用计数器。
func failNTimes(n int, err error) (fn func() error, calls *int) {
	count := 0
	return func() error {
		count++
		if count <= n {
			return err
		}
		return nil
	}, &count
}

func assertDelays(t *testing.T, got, want []time.Duration) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("OnRetry 次数 = %d, 期望 %d (delays=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 次重试等待 = %v, 期望 %v", i+1, got[i], want[i])
		}
	}
}

func TestDoAttemptCounting(t *testing.T) {
	tests := []struct {
		name        string
		failures    int
		maxRetries  int
		wantCalls   int
		wantRetries int
		wantErr     bool
	}{
		{name: "首次即成功", failures: 0, maxRetries: 3, wantCalls: 1, wantRetries: 0},
		{name: "重试一次后成功", failures: 1, maxRetries: 3, wantCalls: 2, wantRetries: 1},
		{name: "用尽全部重试后成功", failures: 3, maxRetries: 3, wantCalls: 4, wantRetries: 3},
		{name: "重试耗尽仍失败", failures: 4, maxRetries: 3, wantCalls: 4, wantRetries: 3, wantErr: true},
		{name: "不允许重试时失败即返回", failures: 1, maxRetries: 0, wantCalls: 1, wantRetries: 0, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log retryLog
			fn, calls := failNTimes(tt.failures, errTransient)
			r := New(&Config{
				MaxRetries:  tt.maxRetries,
				BaseDelay:   tinyDelay,
				MaxDelay:    10 * tinyDelay,
				ShouldRetry: func(error) bool { return true },
				OnRetry:     log.hook(),
			}, testLogger())

			err := r.Do(context.Background(), fn)

			if *calls != tt.wantCalls {
				t.Errorf("fn 调用次数 = %d, 期望 %d", *calls, tt.wantCalls)
			}
			if len(log.attempts) != tt.wantRetries {
				t.Errorf("OnRetry 回调次数 = %d, 期望 %d", len(log.attempts), tt.wantRetries)
			}
			for i, attempt := range log.attempts {
				if attempt != i+1 {
					t.Errorf("OnRetry 第 %d 次上报序号 = %d, 期望 %d", i+1, attempt, i+1)
				}
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("期望成功，实际返回错误: %v", err)
				}
				return
			}
			if !errors.Is(err, errTransient) {
				t.Fatalf("耗尽重试后应包装最后一个错误，实际: %v", err)
			}
		})
	}
}

func TestDoNonRetryableErrorFailsImmediately(t *testing.T) {
	var log retryLog
	calls := 0
	r := New(&Config{
		MaxRetries:  3,
		BaseDelay:   slowBackoff,
		MaxDelay:    slowBackoff,
		ShouldRetry: func(err error) bool { return errors.Is(err, errTransient) },
		OnRetry:     log.hook(),
	}, testLogger())

	start := time.Now()
	err := r.Do(context.Background(), func() error {
		calls++
		return errFatal
	})
	elapsed := time.Since(start)

	if calls != 1 {
		t.Errorf("不可重试错误应只调用一次 fn，实际 %d 次", calls)
	}
	if len(log.attempts) != 0 {
		t.Errorf("不可重试错误不应触发 OnRetry，实际 %d 次", len(log.attempts))
	}
	// 不可重试路径原样返回错误（不包装），调用方可直接比较。
	if !errors.Is(err, errFatal) || errors.Unwrap(err) != nil {
		t.Errorf("期望原样返回 %v，实际 %v", errFatal, err)
	}
	if elapsed >= slowBackoff {
		t.Errorf("不可重试错误不应等待退避，耗时 %v", elapsed)
	}
}

// TestDoRetryAfterHook 钉住 FLOOD_WAIT 路径：服务端指定时长命中时直接采用该时长且无条件重试；
// 未命中/非正数时退回 ShouldRetry + 指数退避；超过 MaxRetryAfter 时就地放弃。
func TestDoRetryAfterHook(t *testing.T) {
	const hugeWait = time.Hour

	tests := []struct {
		name          string
		retryAfter    func(error) (time.Duration, bool)
		shouldRetry   bool
		maxRetryAfter time.Duration
		baseDelay     time.Duration
		wantCalls     int
		wantDelays    []time.Duration
		wantFail      bool
	}{
		{
			name:        "命中时用服务端时长而非退避",
			retryAfter:  func(error) (time.Duration, bool) { return floodDelay, true },
			shouldRetry: false, // 即使分类器判定不可重试，服务端指令仍应触发重试
			baseDelay:   slowBackoff,
			wantCalls:   2,
			wantDelays:  []time.Duration{floodDelay},
		},
		{
			name:          "未超过 MaxRetryAfter 时可等待",
			retryAfter:    func(error) (time.Duration, bool) { return floodDelay, true },
			shouldRetry:   false,
			maxRetryAfter: time.Second,
			baseDelay:     slowBackoff,
			wantCalls:     2,
			wantDelays:    []time.Duration{floodDelay},
		},
		{
			name:          "超过 MaxRetryAfter 时放弃",
			retryAfter:    func(error) (time.Duration, bool) { return hugeWait, true },
			shouldRetry:   false,
			maxRetryAfter: 5 * time.Millisecond,
			baseDelay:     slowBackoff,
			wantCalls:     1,
			wantFail:      true,
		},
		{
			// 真实配置就长这样：telegram.tdShouldRetry 对 429/420 返回 true。
			// 超限时若只是"忽略服务端时长"而不就地放弃，就会退回几秒的指数退避，
			// 去反复敲一个刚刚明确要求等一小时的服务端——限流只会被拖得更久。
			name:          "超过 MaxRetryAfter 时放弃（即使分类器判定可重试）",
			retryAfter:    func(error) (time.Duration, bool) { return hugeWait, true },
			shouldRetry:   true,
			maxRetryAfter: 5 * time.Millisecond,
			baseDelay:     slowBackoff,
			wantCalls:     1,
			wantFail:      true,
		},
		{
			name:        "非正数时长被忽略",
			retryAfter:  func(error) (time.Duration, bool) { return 0, true },
			shouldRetry: false,
			baseDelay:   slowBackoff,
			wantCalls:   1,
			wantFail:    true,
		},
		{
			name:        "未命中时退回指数退避",
			retryAfter:  func(error) (time.Duration, bool) { return 0, false },
			shouldRetry: true,
			baseDelay:   tinyDelay,
			wantCalls:   2,
			wantDelays:  []time.Duration{tinyDelay},
		},
		{
			name:        "未注入钩子时退回指数退避",
			retryAfter:  nil,
			shouldRetry: true,
			baseDelay:   tinyDelay,
			wantCalls:   2,
			wantDelays:  []time.Duration{tinyDelay},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log retryLog
			fn, calls := failNTimes(1, errFlood)
			r := New(&Config{
				MaxRetries:    3,
				BaseDelay:     tt.baseDelay,
				MaxDelay:      time.Minute,
				ShouldRetry:   func(error) bool { return tt.shouldRetry },
				RetryAfter:    tt.retryAfter,
				MaxRetryAfter: tt.maxRetryAfter,
				OnRetry:       log.hook(),
			}, testLogger())

			start := time.Now()
			err := r.Do(context.Background(), fn)
			elapsed := time.Since(start)

			if *calls != tt.wantCalls {
				t.Errorf("fn 调用次数 = %d, 期望 %d", *calls, tt.wantCalls)
			}
			assertDelays(t, log.delays, tt.wantDelays)
			if tt.wantFail && !errors.Is(err, errFlood) {
				t.Errorf("期望返回 %v，实际 %v", errFlood, err)
			}
			if !tt.wantFail && err != nil {
				t.Errorf("期望最终成功，实际 %v", err)
			}
			// 所有用例的等待都应远小于 slowBackoff——否则说明走了 BaseDelay 的指数退避。
			if elapsed >= slowBackoff {
				t.Errorf("耗时 %v，超出预期（疑似套用了指数退避）", elapsed)
			}
		})
	}
}

func TestDoContextCancellation(t *testing.T) {
	t.Run("启动前已取消则不执行 fn", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		calls := 0
		r := New(&Config{
			MaxRetries:  3,
			BaseDelay:   tinyDelay,
			MaxDelay:    tinyDelay,
			ShouldRetry: func(error) bool { return true },
		}, testLogger())

		err := r.Do(ctx, func() error {
			calls++
			return nil
		})

		if calls != 0 {
			t.Errorf("已取消的上下文不应执行 fn，实际调用 %d 次", calls)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("期望 context.Canceled，实际 %v", err)
		}
	})

	t.Run("退避等待期间取消立即返回", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		calls := 0
		r := New(&Config{
			MaxRetries:  3,
			BaseDelay:   slowBackoff,
			MaxDelay:    slowBackoff,
			ShouldRetry: func(error) bool { return true },
		}, testLogger())

		start := time.Now()
		err := r.Do(ctx, func() error {
			calls++
			cancel() // 第一次失败后立即取消：Do 应在退避 select 中命中 ctx.Done
			return errTransient
		})
		elapsed := time.Since(start)

		if calls != 1 {
			t.Errorf("取消后不应继续重试，fn 调用 %d 次", calls)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("期望 context.Canceled，实际 %v", err)
		}
		if elapsed >= slowBackoff {
			t.Errorf("取消应立即中断退避等待，实际耗时 %v", elapsed)
		}
	})

	t.Run("退避等待期间超时返回 DeadlineExceeded", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()

		r := New(&Config{
			MaxRetries:  3,
			BaseDelay:   slowBackoff,
			MaxDelay:    slowBackoff,
			ShouldRetry: func(error) bool { return true },
		}, testLogger())

		start := time.Now()
		err := r.Do(ctx, func() error { return errTransient })
		elapsed := time.Since(start)

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("期望 context.DeadlineExceeded，实际 %v", err)
		}
		if elapsed >= slowBackoff {
			t.Errorf("超时应立即中断退避等待，实际耗时 %v", elapsed)
		}
	})
}

func TestCalculateDelayExponentialGrowth(t *testing.T) {
	tests := []struct {
		name      string
		baseDelay time.Duration
		maxDelay  time.Duration
		attempt   int
		want      time.Duration
	}{
		{name: "首次退避等于 base", baseDelay: time.Second, maxDelay: 30 * time.Second, attempt: 0, want: time.Second},
		{name: "第二次翻倍", baseDelay: time.Second, maxDelay: 30 * time.Second, attempt: 1, want: 2 * time.Second},
		{name: "第三次四倍", baseDelay: time.Second, maxDelay: 30 * time.Second, attempt: 2, want: 4 * time.Second},
		{name: "达到上限后被钳制", baseDelay: time.Second, maxDelay: 30 * time.Second, attempt: 5, want: 30 * time.Second},
		{name: "远超上限仍被钳制", baseDelay: time.Second, maxDelay: 30 * time.Second, attempt: 20, want: 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New(&Config{
				BaseDelay:    tt.baseDelay,
				MaxDelay:     tt.maxDelay,
				JitterFactor: 0, // 关闭抖动才能断言精确值
			}, testLogger())

			if got := r.calculateDelay(tt.attempt); got != tt.want {
				t.Errorf("calculateDelay(%d) = %v, 期望 %v", tt.attempt, got, tt.want)
			}
		})
	}
}

func TestCalculateDelayJitter(t *testing.T) {
	const iterations = 200
	r := New(&Config{
		BaseDelay:    100 * time.Millisecond,
		MaxDelay:     time.Second,
		JitterFactor: DefaultJitterFactor,
	}, testLogger())

	lo := 90 * time.Millisecond
	hi := 110 * time.Millisecond
	seen := make(map[time.Duration]struct{}, iterations)
	for i := 0; i < iterations; i++ {
		got := r.calculateDelay(0)
		if got < lo || got > hi {
			t.Fatalf("抖动后的等待 %v 超出 [%v, %v]", got, lo, hi)
		}
		seen[got] = struct{}{}
	}
	if len(seen) < 2 {
		t.Errorf("抖动未产生随机性，仅出现 %d 种取值", len(seen))
	}
}

// TestCalculateDelayJitterAppliesAfterClamp 钉住当前实现：抖动在 MaxDelay 钳制之后叠加，
// 因此实际等待可以略微超过 MaxDelay（最多 JitterFactor 比例）。
func TestCalculateDelayJitterAppliesAfterClamp(t *testing.T) {
	maxDelay := 100 * time.Millisecond
	r := New(&Config{
		BaseDelay:    10 * time.Millisecond,
		MaxDelay:     maxDelay,
		JitterFactor: DefaultJitterFactor,
	}, testLogger())

	lo := 90 * time.Millisecond
	hi := 110 * time.Millisecond
	for i := 0; i < 100; i++ {
		got := r.calculateDelay(10) // 指数部分早已超过 MaxDelay
		if got < lo || got > hi {
			t.Fatalf("钳制后的等待 %v 超出 [%v, %v]", got, lo, hi)
		}
	}
}

func TestDefaultShouldRetry(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil 不重试", err: nil, want: false},
		{name: "连接错误可重试", err: errors.New("connection refused"), want: true},
		{name: "超时可重试", err: errors.New("i/o timeout"), want: true},
		{name: "网络错误可重试", err: errors.New("network is unreachable"), want: true},
		{name: "临时错误可重试", err: errors.New("temporary failure"), want: true},
		{name: "服务端内部错误可重试", err: errors.New("INTERNAL_SERVER_ERROR"), want: true},
		{name: "NETWORK_MIGRATE 可重试", err: errors.New("NETWORK_MIGRATE_2"), want: true},
		{name: "PHONE_MIGRATE 可重试", err: errors.New("PHONE_MIGRATE_4"), want: true},
		{name: "FILE_MIGRATE 可重试", err: errors.New("FILE_MIGRATE_1"), want: true},
		{name: "USER_MIGRATE 可重试", err: errors.New("USER_MIGRATE_5"), want: true},
		{name: "STATS_MIGRATE 可重试", err: errors.New("STATS_MIGRATE_3"), want: true},
		{name: "FLOOD_WAIT 默认不重试", err: errFlood, want: false},
		{name: "业务错误不重试", err: errFatal, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultShouldRetry(tt.err); got != tt.want {
				t.Errorf("DefaultShouldRetry(%v) = %v, 期望 %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNewWithNilConfigUsesDefaults(t *testing.T) {
	for _, r := range []*Retrier{New(nil, testLogger()), NewDefault(testLogger())} {
		if r.config.MaxRetries != DefaultMaxRetries {
			t.Errorf("MaxRetries = %d, 期望 %d", r.config.MaxRetries, DefaultMaxRetries)
		}
		if r.config.BaseDelay != DefaultBaseDelay {
			t.Errorf("BaseDelay = %v, 期望 %v", r.config.BaseDelay, DefaultBaseDelay)
		}
		if r.config.MaxDelay != DefaultMaxDelay {
			t.Errorf("MaxDelay = %v, 期望 %v", r.config.MaxDelay, DefaultMaxDelay)
		}
		if r.config.JitterFactor != DefaultJitterFactor {
			t.Errorf("JitterFactor = %v, 期望 %v", r.config.JitterFactor, DefaultJitterFactor)
		}
		if r.config.ShouldRetry == nil || r.config.OnRetry == nil {
			t.Error("默认配置应提供 ShouldRetry 与 OnRetry")
		}
		if r.config.RetryAfter != nil || r.config.MaxRetryAfter != 0 {
			t.Error("默认配置不应感知协议特定的 RetryAfter")
		}
	}
}

func TestWithBuildersReturnCopies(t *testing.T) {
	base := NewDefault(testLogger())

	t.Run("WithMaxRetries", func(t *testing.T) {
		got := base.WithMaxRetries(7)
		if got.config.MaxRetries != 7 {
			t.Errorf("MaxRetries = %d, 期望 7", got.config.MaxRetries)
		}
		if base.config.MaxRetries != DefaultMaxRetries {
			t.Errorf("原 Retrier 被修改: MaxRetries = %d", base.config.MaxRetries)
		}
	})

	t.Run("WithBaseDelay", func(t *testing.T) {
		got := base.WithBaseDelay(5 * time.Second)
		if got.config.BaseDelay != 5*time.Second {
			t.Errorf("BaseDelay = %v, 期望 5s", got.config.BaseDelay)
		}
		if base.config.BaseDelay != DefaultBaseDelay {
			t.Errorf("原 Retrier 被修改: BaseDelay = %v", base.config.BaseDelay)
		}
	})

	t.Run("WithMaxDelay", func(t *testing.T) {
		got := base.WithMaxDelay(9 * time.Second)
		if got.config.MaxDelay != 9*time.Second {
			t.Errorf("MaxDelay = %v, 期望 9s", got.config.MaxDelay)
		}
		if base.config.MaxDelay != DefaultMaxDelay {
			t.Errorf("原 Retrier 被修改: MaxDelay = %v", base.config.MaxDelay)
		}
	})

	t.Run("WithClassifier 注入分类器", func(t *testing.T) {
		retryAfter := func(error) (time.Duration, bool) { return floodDelay, true }
		got := base.WithClassifier(func(error) bool { return true }, retryAfter, time.Minute)

		if !got.config.ShouldRetry(errFatal) {
			t.Error("ShouldRetry 未被替换")
		}
		if d, ok, abort := got.serverRetryAfter(errFlood); !ok || abort || d != floodDelay {
			t.Errorf("serverRetryAfter = (%v, %v, %v), 期望 (%v, true, false)", d, ok, abort, floodDelay)
		}
		if got.config.MaxRetryAfter != time.Minute {
			t.Errorf("MaxRetryAfter = %v, 期望 1m", got.config.MaxRetryAfter)
		}
		if base.config.RetryAfter != nil {
			t.Error("原 Retrier 被修改: RetryAfter 非空")
		}
	})

	t.Run("WithClassifier 传 nil 保留原值", func(t *testing.T) {
		src := base.WithClassifier(func(error) bool { return true }, func(error) (time.Duration, bool) {
			return floodDelay, true
		}, time.Minute)

		// 仅 MaxRetryAfter 无条件覆盖：传 0 会关闭上限检查。
		got := src.WithClassifier(nil, nil, 0)

		if !got.config.ShouldRetry(errFatal) {
			t.Error("传 nil 时应保留原 ShouldRetry")
		}
		if d, ok, abort := got.serverRetryAfter(errFlood); !ok || abort || d != floodDelay {
			t.Errorf("传 nil 时应保留原 RetryAfter, 实际 (%v, %v, %v)", d, ok, abort)
		}
		if got.config.MaxRetryAfter != 0 {
			t.Errorf("MaxRetryAfter = %v, 期望被覆盖为 0", got.config.MaxRetryAfter)
		}
	})
}

// TestNewDefaultIntegration 走一遍默认配置的完整链路（DefaultShouldRetry + 默认 OnRetry 日志），
// 仅把 BaseDelay 缩短到毫秒级以保证测试速度。
func TestNewDefaultIntegration(t *testing.T) {
	r := NewDefault(testLogger()).WithBaseDelay(tinyDelay).WithMaxDelay(10 * tinyDelay)

	fn, calls := failNTimes(2, errTransient)
	if err := r.Do(context.Background(), fn); err != nil {
		t.Fatalf("期望重试后成功，实际 %v", err)
	}
	if *calls != 3 {
		t.Errorf("fn 调用次数 = %d, 期望 3", *calls)
	}

	always, calls := failNTimes(DefaultMaxRetries+1, errTransient)
	err := r.Do(context.Background(), always)
	if !errors.Is(err, errTransient) {
		t.Fatalf("期望包装最后一个错误，实际 %v", err)
	}
	if *calls != DefaultMaxRetries+1 {
		t.Errorf("fn 调用次数 = %d, 期望 %d", *calls, DefaultMaxRetries+1)
	}
}
