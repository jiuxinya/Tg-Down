package telegram

import (
	"strings"
	"sync"
	"testing"
	"time"

	tdclient "github.com/zelenin/go-tdlib/client"

	"tg-down/internal/logger"
)

func TestParseProxyURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		raw      string
		server   string
		port     int32
		wantType tdclient.ProxyType
	}{
		{
			name: "socks5 无凭据", raw: "socks5://10.0.0.1:1080",
			server: "10.0.0.1", port: 1080, wantType: &tdclient.ProxyTypeSocks5{},
		},
		{
			name: "socks5 带凭据与百分号编码", raw: "socks5://user:p%40ss@10.0.0.1:1080",
			server: "10.0.0.1", port: 1080,
			wantType: &tdclient.ProxyTypeSocks5{Username: "user", Password: "p@ss"},
		},
		{
			name: "socks 别名", raw: "socks://192.168.1.3:7891",
			server: "192.168.1.3", port: 7891, wantType: &tdclient.ProxyTypeSocks5{},
		},
		{
			name: "socks5h 别名", raw: "socks5h://192.168.1.4:1080",
			server: "192.168.1.4", port: 1080, wantType: &tdclient.ProxyTypeSocks5{},
		},
		{
			name: "http CONNECT 代理", raw: "http://127.0.0.1:7890",
			server: "127.0.0.1", port: 7890, wantType: &tdclient.ProxyTypeHttp{},
		},
		{
			name: "http 带凭据", raw: "http://alice:s3cret@proxy.lan:8080",
			server: "proxy.lan", port: 8080,
			wantType: &tdclient.ProxyTypeHttp{Username: "alice", Password: "s3cret"},
		},
		{
			// https:// 写法按 HTTP CONNECT 处理（Clash 等工具的常见导出格式）
			name: "https 写法按 http CONNECT 处理", raw: "https://127.0.0.1:7890",
			server: "127.0.0.1", port: 7890, wantType: &tdclient.ProxyTypeHttp{},
		},
		{
			name: "mtproto", raw: "mtproto://DDDDDDAAAA@example.com:443",
			server: "example.com", port: 443,
			wantType: &tdclient.ProxyTypeMtproto{Secret: "DDDDDDAAAA"},
		},
		{
			name: "IPv6 字面量去方括号", raw: "socks5://[::1]:1080",
			server: "::1", port: 1080, wantType: &tdclient.ProxyTypeSocks5{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req, err := ParseProxyURL(tt.raw)
			if err != nil {
				t.Fatalf("ParseProxyURL(%q) error = %v", tt.raw, err)
			}
			if !req.Enable {
				t.Error("解析出的代理应立即启用（Enable=true）")
			}
			if req.Proxy.Server != tt.server || req.Proxy.Port != tt.port {
				t.Errorf("server/port = %s:%d, want %s:%d", req.Proxy.Server, req.Proxy.Port, tt.server, tt.port)
			}
			gotType := req.Proxy.Type
			switch want := tt.wantType.(type) {
			case *tdclient.ProxyTypeSocks5:
				got, ok := gotType.(*tdclient.ProxyTypeSocks5)
				if !ok {
					t.Fatalf("类型 = %T, want %T", gotType, want)
				}
				if got.Username != want.Username || got.Password != want.Password {
					t.Errorf("凭据 = %q/%q, want %q/%q", got.Username, got.Password, want.Username, want.Password)
				}
			case *tdclient.ProxyTypeHttp:
				got, ok := gotType.(*tdclient.ProxyTypeHttp)
				if !ok {
					t.Fatalf("类型 = %T, want %T", gotType, want)
				}
				if got.Username != want.Username || got.Password != want.Password {
					t.Errorf("凭据 = %q/%q, want %q/%q", got.Username, got.Password, want.Username, want.Password)
				}
			case *tdclient.ProxyTypeMtproto:
				got, ok := gotType.(*tdclient.ProxyTypeMtproto)
				if !ok {
					t.Fatalf("类型 = %T, want %T", gotType, want)
				}
				if got.Secret != want.Secret {
					t.Errorf("secret = %q, want %q", got.Secret, want.Secret)
				}
			}
		})
	}
}

func TestParseProxyURLRejectsInvalid(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"",
		"   ",
		"ftp://10.0.0.1:21",          // 不支持的协议
		"10.0.0.1:7890",              // 缺协议（常见笔误，必须报错而不是猜）
		"://10.0.0.1:1080",           // 残缺 URL
		"socks5://10.0.0.1",          // 缺端口
		"socks5://:1080",             // 缺主机名
		"socks5://10.0.0.1:0",        // 端口下界外
		"socks5://10.0.0.1:65536",    // 端口上界外
		"socks5://10.0.0.1:port",     // 端口非数字
		"mtproto://@example.com:443", // mtproto 缺 secret
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseProxyURL(raw); err == nil {
				t.Fatalf("ParseProxyURL(%q) 应返回错误", raw)
			}
		})
	}
}

func TestParseProxyURLErrorDoesNotLeakCredentials(t *testing.T) {
	t.Parallel()
	_, err := ParseProxyURL("socks5://alice:hunter2@10.0.0.1:99999")
	if err == nil {
		t.Fatal("应返回错误")
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "alice") {
		t.Fatalf("错误信息泄露凭据: %v", err)
	}
}

func TestMaskProxyURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"带凭据", "socks5://user:p%40ss@10.0.0.1:1080", "socks5://***@10.0.0.1:1080"},
		{"无凭据保持原样", "http://127.0.0.1:7890", "http://127.0.0.1:7890"},
		{"mtproto secret 位于 userinfo", "mtproto://SECRET@host:443", "mtproto://***@host:443"},
		{"无法解析统一打码", "not a proxy url", "***"},
		{"空串打码", "", "***"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := MaskProxyURL(tt.raw); got != tt.want {
				t.Errorf("MaskProxyURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestDescribeProxyOmitsCredentials(t *testing.T) {
	t.Parallel()
	req, err := ParseProxyURL("socks5://user:secret@10.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	if got := describeProxy(req); got != "socks5://10.0.0.1:1080" {
		t.Fatalf("describeProxy() = %q, 凭据不应出现在日志描述中", got)
	}
}

func TestResolveTelegramProxy(t *testing.T) {
	t.Run("全部未配置时直连", func(t *testing.T) {
		req, source, err := resolveTelegramProxy("")
		if err != nil || req != nil || source != "" {
			t.Fatalf("= (%v, %q, %v), want 直连", req, source, err)
		}
	})

	t.Run("显式配置优先于环境变量", func(t *testing.T) {
		t.Setenv("TG_PROXY", "socks5://10.0.0.1:1080")
		req, source, err := resolveTelegramProxy("http://127.0.0.1:7890")
		if err != nil {
			t.Fatal(err)
		}
		if source != "config.yaml telegram.proxy" {
			t.Fatalf("source = %q", source)
		}
		if _, ok := req.Proxy.Type.(*tdclient.ProxyTypeHttp); !ok {
			t.Fatalf("应使用配置文件中的 http 代理, 得到 %T", req.Proxy.Type)
		}
	})

	t.Run("环境变量回退链按优先级生效", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "http://https-hop:3128")
		t.Setenv("ALL_PROXY", "socks5://all-hop:1080")
		req, source, err := resolveTelegramProxy("")
		if err != nil {
			t.Fatal(err)
		}
		if source != "环境变量 ALL_PROXY" {
			t.Fatalf("source = %q, ALL_PROXY 应优先于 HTTPS_PROXY", source)
		}
		if req.Proxy.Server != "all-hop" {
			t.Fatalf("server = %q", req.Proxy.Server)
		}
	})

	t.Run("TG_PROXY 是回退链首位", func(t *testing.T) {
		t.Setenv("TG_PROXY", "socks5://tg:1080")
		t.Setenv("HTTP_PROXY", "http://http-hop:80")
		_, source, err := resolveTelegramProxy("")
		if err != nil || source != "环境变量 TG_PROXY" {
			t.Fatalf("= (%q, %v)", source, err)
		}
	})

	t.Run("小写环境变量同样生效", func(t *testing.T) {
		t.Setenv("https_proxy", "http://lower-case:3128")
		req, source, err := resolveTelegramProxy("")
		if err != nil || source != "环境变量 HTTPS_PROXY" {
			t.Fatalf("= (%q, %v)", source, err)
		}
		if req.Proxy.Server != "lower-case" {
			t.Fatalf("server = %q", req.Proxy.Server)
		}
	})

	t.Run("direct 截断环境变量回退强制直连", func(t *testing.T) {
		t.Setenv("TG_PROXY", "direct")
		t.Setenv("ALL_PROXY", "socks5://ignored:1080")
		req, _, err := resolveTelegramProxy("")
		if err != nil || req != nil {
			t.Fatalf("= (%v, %v), want 强制直连", req, err)
		}
	})

	t.Run("配置文件的 direct 覆盖一切环境变量", func(t *testing.T) {
		t.Setenv("TG_PROXY", "socks5://ignored:1080")
		req, _, err := resolveTelegramProxy("OFF")
		if err != nil || req != nil {
			t.Fatalf("= (%v, %v), want 强制直连", req, err)
		}
	})

	t.Run("无效配置直接报错而不是静默直连", func(t *testing.T) {
		if _, _, err := resolveTelegramProxy("ftp://nope:21"); err == nil {
			t.Fatal("配置无效时应返回错误")
		}
	})

	t.Run("无效环境变量报错并标明来源", func(t *testing.T) {
		t.Setenv("TG_PROXY", "nonsense-without-port")
		_, _, err := resolveTelegramProxy("")
		if err == nil || !strings.Contains(err.Error(), "TG_PROXY") {
			t.Fatalf("err = %v, 应包含来源变量名", err)
		}
	})
}

// TestRunConnectHint 验证连接停滞提示：迟迟未就绪时给出代理配置建议，就绪后静默退出。
func TestRunConnectHint(t *testing.T) {
	const (
		hintDelay  = 20 * time.Millisecond
		hintRepeat = 40 * time.Millisecond
	)

	newLoggedClient := func(t *testing.T) (*Client, *logCapture) {
		t.Helper()
		c := newTestClient(t)
		// newTestClient 默认 error 级别会过滤 Warn，这里换成 debug 以捕获提示
		c.logger = logger.New("debug")
		cap := newLogCapture()
		c.logger.SetHook(cap.record)
		return c, cap
	}

	t.Run("无代理停滞时建议配置代理", func(t *testing.T) {
		c, cap := newLoggedClient(t)
		c.connState.Store(ConnectionConnecting)
		done := make(chan struct{})
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			c.runConnectHintWith(hintDelay, hintRepeat, false, done)
		}()
		time.Sleep(120 * time.Millisecond)
		close(done)
		<-finished // 与 goroutine 同步退出，避免与后续用例竞争
		msgs := cap.messages()
		if len(msgs) == 0 {
			t.Fatal("连接停滞时应输出诊断提示")
		}
		first := msgs[0]
		if !strings.Contains(first, "telegram.proxy") || !strings.Contains(first, "TG_PROXY") {
			t.Fatalf("提示缺少代理配置指引: %q", first)
		}
		if !strings.Contains(first, "连接中") {
			t.Fatalf("提示应包含当前状态: %q", first)
		}
	})

	t.Run("已配代理停滞时提示检查代理本身", func(t *testing.T) {
		c, cap := newLoggedClient(t)
		c.connState.Store(ConnectionWaitingNetwork)
		done := make(chan struct{})
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			c.runConnectHintWith(hintDelay, hintRepeat, true, done)
		}()
		time.Sleep(60 * time.Millisecond)
		close(done)
		<-finished
		msgs := cap.messages()
		if len(msgs) == 0 {
			t.Fatal("连接停滞时应输出诊断提示")
		}
		if !strings.Contains(msgs[0], "代理服务器是否可用") {
			t.Fatalf("提示应指向检查代理: %q", msgs[0])
		}
	})

	t.Run("就绪后不再提示", func(t *testing.T) {
		c, cap := newLoggedClient(t)
		c.connState.Store(ConnectionReady)
		done := make(chan struct{})
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			c.runConnectHintWith(hintDelay, hintRepeat, false, done)
		}()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			close(done)
			t.Fatal("Ready 状态下 runConnectHint 应立即退出")
		}
		if msgs := cap.messages(); len(msgs) != 0 {
			t.Fatalf("就绪后不应输出提示: %q", msgs)
		}
	})

	t.Run("Connect 返回后停止输出", func(t *testing.T) {
		c, cap := newLoggedClient(t)
		c.connState.Store(ConnectionConnecting)
		done := make(chan struct{})
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			c.runConnectHintWith(hintDelay, hintRepeat, false, done)
		}()
		time.Sleep(50 * time.Millisecond)
		close(done) // Connect 返回时关闭 done
		<-finished
		count := len(cap.messages())
		if count == 0 {
			t.Fatal("关闭前应有至少一条提示")
		}
		// goroutine 已确认退出，之后不应再有新输出
		time.Sleep(2 * hintDelay)
		if after := len(cap.messages()); after != count {
			t.Fatalf("done 关闭后仍在继续输出: %d -> %d", count, after)
		}
	})
}

// logCapture 收集 logger 输出，供断言（SetHook 回调运行于日志调用方 goroutine）。
type logCapture struct {
	mu  sync.Mutex
	msg []string
}

func newLogCapture() *logCapture { return &logCapture{} }

func (c *logCapture) record(level, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msg = append(c.msg, msg)
}

func (c *logCapture) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msg...)
}
