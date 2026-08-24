package desktop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"tg-down/internal/logger"
)

func testShell(t *testing.T) (*Shell, string) {
	t.Helper()
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"state":"ready"}`))
	}))
	t.Cleanup(engine.Close)

	reg, err := OpenRegistry(filepath.Join(t.TempDir(), InstancesFile))
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShell(reg, engine.URL, t.TempDir(), "test", stubAutostart{}, logger.New("error"))
	base, err := sh.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sh.Stop)
	return sh, base
}

func do(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, string(body)
}

// TestShellRejectsForeignOrigin 回归测试：反代必须删掉 Origin 才能让引擎放行，
// 引擎那层的 CSRF 防护对经壳转发的请求等于失效，跨站校验只能由壳自己做。
func TestShellRejectsForeignOrigin(t *testing.T) {
	_, base := testShell(t)

	paths := []string{"/api/state", "/api/remote/anything/api/state", "/desktop/api/instances"}
	for _, p := range paths {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+p, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "http://evil.example")
		if code, _ := do(t, req); code != http.StatusForbidden {
			t.Errorf("%s 带外站 Origin 应 403，got %d", p, code)
		}
	}
}

func TestShellAcceptsOwnOriginAndNoOrigin(t *testing.T) {
	_, base := testShell(t)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/api/state", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", base)
	if code, body := do(t, req); code != http.StatusOK {
		t.Errorf("同源 Origin 应放行，got %d %s", code, body)
	}

	req2, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/api/state", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := do(t, req2); code != http.StatusOK {
		t.Errorf("无 Origin 应放行，got %d %s", code, body)
	}
}

// TestShellOriginCheckIsPortSensitive 同一主机不同端口是不同的源
func TestShellOriginCheckIsPortSensitive(t *testing.T) {
	sh, _ := testShell(t)
	host := strings.TrimPrefix(sh.baseURL, "http://")
	hostOnly, _, _ := strings.Cut(host, ":")
	cases := map[string]bool{
		sh.baseURL:                  true,
		"http://" + hostOnly + ":1": false,
		"https://" + host:           false,
		"http://" + hostOnly:        false,
		"null":                      false,
		"":                          false,
	}
	for origin, want := range cases {
		if got := sh.originAllowed(origin); got != want {
			t.Errorf("originAllowed(%q) = %v, want %v", origin, got, want)
		}
	}
}

// TestDecodeBodyRejectsNonJSON 覆盖 text/plain 这类无需预检就能跨站发出的简单请求
func TestDecodeBodyRejectsNonJSON(t *testing.T) {
	_, base := testShell(t)

	cases := []struct {
		name, contentType, body string
		want                    int
	}{
		{"缺 Content-Type", "", `{"url":"http://x:1"}`, http.StatusUnsupportedMediaType},
		{"text/plain", "text/plain", `{"url":"http://x:1"}`, http.StatusUnsupportedMediaType},
		{"表单", "application/x-www-form-urlencoded", `url=http://x:1`, http.StatusUnsupportedMediaType},
		{"未知字段", "application/json", `{"url":"http://x:1","evil":1}`, http.StatusBadRequest},
		{"多个 JSON 值", "application/json", `{"url":"http://x:1"}{"url":"http://y:2"}`, http.StatusBadRequest},
		{"带 charset 参数", "application/json; charset=utf-8", `{"url":"http://x:1"}`, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
				base+"/desktop/api/instances", strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			if c.contentType != "" {
				req.Header.Set("Content-Type", c.contentType)
			}
			if code, body := do(t, req); code != c.want {
				t.Errorf("状态码 = %d, want %d (%s)", code, c.want, body)
			}
		})
	}
}

// TestShellUIHasSecurityHeaders 壳层挂的是 web.UIHandler，没有 withSecurity 包裹，
// 安全头必须由 UIHandler 自己补齐，否则同一份 SPA 在桌面端完全没有 CSP。
func TestShellUIHasSecurityHeaders(t *testing.T) {
	_, base := testShell(t)
	resp, err := http.Get(base + "/") //nolint:noctx // 测试内的本地请求
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("壳层 SPA 响应缺少 %s", h)
		}
	}
}

// TestShellShowEndpoint 第二个实例靠它把已运行的窗口拉到前台
func TestShellShowEndpoint(t *testing.T) {
	sh, base := testShell(t)
	called := make(chan struct{}, 1)
	sh.SetOnShow(func() { called <- struct{}{} })

	if err := PublishShellURL(sh.appDir, base); err != nil {
		t.Fatal(err)
	}
	if err := ShowRunningInstance(sh.appDir); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	default:
		t.Fatal("onShow 未被调用")
	}
}

// TestShellUpdateRejectsEmptyRename 改名成空串此前被当成"不修改"，
// 界面上点了确定却什么也没发生，也没有任何错误提示。
func TestShellUpdateRejectsEmptyRename(t *testing.T) {
	sh, base := testShell(t)
	in, err := sh.reg.Add("NAS", "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}

	put := func(body string) (int, string) {
		req, rerr := http.NewRequestWithContext(context.Background(), http.MethodPut,
			base+"/desktop/api/instances/"+in.ID, strings.NewReader(body))
		if rerr != nil {
			t.Fatal(rerr)
		}
		req.Header.Set("Content-Type", "application/json")
		return do(t, req)
	}

	if code, body := put(`{"name":""}`); code != http.StatusBadRequest {
		t.Errorf("改名为空串应 400，got %d %s", code, body)
	}
	if code, body := put(`{"token":"x"}`); code != http.StatusOK {
		t.Errorf("只改令牌应 200，got %d %s", code, body)
	}
	if got, _ := sh.reg.Get(in.ID); got.Name != "NAS" {
		t.Errorf("名称不应被改动，got %q", got.Name)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut,
		base+"/desktop/api/instances/ghost", strings.NewReader(`{"name":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if code, _ := do(t, req); code != http.StatusNotFound {
		t.Errorf("未知实例应 404，got %d", code)
	}
}

func TestShellAutostartRoundTrip(t *testing.T) {
	_, base := testShell(t)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut,
		base+"/desktop/api/autostart", strings.NewReader(`{"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	code, body := do(t, req)
	if code != http.StatusOK {
		t.Fatalf("PUT autostart = %d %s", code, body)
	}
	// stubAutostart 的 Enabled 恒为 false：响应必须回显回读结果而不是请求里的意图
	var got struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Errorf("应回显系统实际状态 false，got %s", body)
	}
}
