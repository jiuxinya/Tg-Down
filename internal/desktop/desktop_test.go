package desktop

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://192.168.1.10:8080", "http://192.168.1.10:8080/"},
		{"http://192.168.1.10:8080/", "http://192.168.1.10:8080/"},
		{"  https://nas.example.com  ", "https://nas.example.com/"},
		{"192.168.1.10:8080", "http://192.168.1.10:8080/"},
		{"", ""},
		{"ftp://x", ""},
		{"/no/scheme/host", ""},
		{"http://host/path", ""},
	}
	for _, c := range cases {
		if got := NormalizeBaseURL(c.in); got != c.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRegistryCRUDAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), InstancesFile)
	reg, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Selected() != LocalInstanceID {
		t.Fatalf("初始选中项 = %q, want local", reg.Selected())
	}

	in, err := reg.Add("NAS", "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Update(in.ID, "", "", strPtr("secret-token")); err != nil {
		t.Fatal(err)
	}
	if err := reg.Select(in.ID); err != nil {
		t.Fatal(err)
	}

	// 保留 local 伪实例应被拒绝：Select 对未知 ID 报错；Add 的 ID 随机不可能撞 local
	if err := reg.Select("no-such-id"); err == nil {
		t.Fatal("Select 未知 ID 应报错")
	}
	if err := reg.Select(LocalInstanceID); err != nil {
		t.Fatalf("Select(local) 应合法: %v", err)
	}
	if err := reg.Select(in.ID); err != nil {
		t.Fatal(err)
	}

	// 重开验证持久化：token、selected 均还原
	reg2, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reg2.Get(in.ID)
	if !ok || got.Token != "secret-token" || got.URL != "http://127.0.0.1:1/" {
		t.Fatalf("持久化读取不符: %+v ok=%v", got, ok)
	}
	if reg2.Selected() != in.ID {
		t.Fatalf("重开后选中项 = %q, want %q", reg2.Selected(), in.ID)
	}

	// 删除选中项后回落 local
	if err := reg2.Delete(in.ID); err != nil {
		t.Fatal(err)
	}
	if reg2.Selected() != LocalInstanceID {
		t.Fatalf("删除选中项后应回落 local，got %q", reg2.Selected())
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("注册表文件权限 = %v, want 0600", fi.Mode().Perm())
	}
}

func strPtr(s string) *string { return &s }

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		cand, cur string
		want      bool
	}{
		{"3.1.0", "3.0.0", true},
		{"v3.1.0", "3.1.0", false},
		{"3.0.0", "dev", true},
		{"dev", "3.0.0", false},
		{"3.0.0-beta1", "2.9.9", true},
		{"abc", "1.0.0", false},
	}
	for _, c := range cases {
		if got := NewerVersion(c.cand, c.cur); got != c.want {
			t.Errorf("NewerVersion(%q, %q) = %v, want %v", c.cand, c.cur, got, c.want)
		}
	}
}

func TestProxyDirectorRewritesRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), InstancesFile)
	reg, _ := OpenRegistry(path)
	in, err := reg.Add("NAS", "http://remote.example:9000")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Update(in.ID, "", "", strPtr("tok-1234")); err != nil {
		t.Fatal(err)
	}

	rp, err := newInstanceProxy(reg, in.ID)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://shell.local/api/remote/"+in.ID+"/api/history/7/thumb?token=localjunk&x=1", nil)
	req.Header.Set("Origin", "http://shell.local")
	req.Header.Set("Cookie", "tg_down_web_auth=zzz")
	rp.Director(req)

	if req.URL.Scheme != "http" || req.URL.Host != "remote.example:9000" {
		t.Fatalf("目标改写错误: %s://%s", req.URL.Scheme, req.URL.Host)
	}
	if req.Host != "remote.example:9000" {
		t.Fatalf("Host 头 = %q", req.Host)
	}
	if req.URL.Path != "/api/history/7/thumb" {
		t.Fatalf("路径未剥离前缀: %q", req.URL.Path)
	}
	if q := req.URL.Query(); q.Get("token") != "tok-1234" {
		t.Fatalf("查询参数令牌注入错误: %v", q)
	}
	if q := req.URL.Query().Get("x"); q != "1" {
		t.Fatalf("无关查询参数被误删: %v", req.URL.RawQuery)
	}
	if req.Header.Get("Authorization") != "Bearer tok-1234" {
		t.Fatalf("Authorization 注入错误: %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("Origin") != "" || req.Header.Get("Cookie") != "" {
		t.Fatal("Origin/Cookie 应被剥离")
	}
}

func TestProxyUnknownInstanceFails(t *testing.T) {
	reg, _ := OpenRegistry(filepath.Join(t.TempDir(), InstancesFile))
	if _, err := newInstanceProxy(reg, "ghost"); err == nil {
		t.Fatal("未知实例应返回错误")
	}
}

func TestLocalEngineProxyDirector(t *testing.T) {
	target, err := url.Parse("http://127.0.0.1:54321")
	if err != nil {
		t.Fatal(err)
	}
	rp := localEngineProxy(target).(*httputil.ReverseProxy)

	req := httptest.NewRequest(http.MethodPost, "http://shell.local/api/settings?token=eng-token", nil)
	req.Header.Set("Origin", "http://shell.local")
	req.Header.Set("Referer", "http://shell.local/settings")
	req.Header.Set("Cookie", "tg_down_web_auth=zzz")
	rp.Director(req)

	if req.URL.Scheme != "http" || req.URL.Host != "127.0.0.1:54321" || req.URL.Path != "/api/settings" {
		t.Fatalf("目标/路径改写错误: %s://%s%s", req.URL.Scheme, req.URL.Host, req.URL.Path)
	}
	if req.Host != "127.0.0.1:54321" {
		t.Fatalf("Host 头 = %q", req.Host)
	}
	if req.Header.Get("Origin") != "" || req.Header.Get("Referer") != "" || req.Header.Get("Cookie") != "" {
		t.Fatal("Origin/Referer/Cookie 应被剥离")
	}
	if q := req.URL.Query().Get("token"); q != "eng-token" {
		t.Fatalf("引擎自身的 token 查询参数应原样保留: %q", req.URL.RawQuery)
	}
}

// TestShellRoutesLocalAPIToEngine 回归测试：WebView 的源是壳服务端口，本机实例的
// /api/* 同源请求必须由壳层转发到本地引擎。若缺失该路由，请求会落入 SPA 回落
// 路由返回 index.html，界面将永远停在“正在连接”并不断提示重连。
func TestShellRoutesLocalAPIToEngine(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/state" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"state":"ready","version":"stub"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer engine.Close()

	reg, err := OpenRegistry(filepath.Join(t.TempDir(), InstancesFile))
	if err != nil {
		t.Fatal(err)
	}
	shell := NewShell(reg, engine.URL, t.TempDir(), "test", stubAutostart{})
	base, err := shell.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Stop()

	resp, err := http.Get(base + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"state":"ready"`) {
		t.Fatalf("/api/state 未转发到引擎: HTTP %d, body=%s", resp.StatusCode, body)
	}
}

type stubAutostart struct{}

func (stubAutostart) Enabled() (bool, error) { return false, nil }
func (stubAutostart) Enable() error          { return nil }
func (stubAutostart) Disable() error         { return nil }
