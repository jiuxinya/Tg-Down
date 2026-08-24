package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestRequest 构造一个带可选 Authorization 头与 token 查询参数的请求，供令牌校验测试使用。
func newTestRequest(auth, queryToken string) *http.Request {
	target := "/api/state"
	if queryToken != "" {
		target += "?token=" + queryToken
	}
	r := httptest.NewRequest(http.MethodGet, target, http.NoBody)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"localhost:8080": true,
		"[::1]:8080":     true,
		"0.0.0.0:8080":   false,
		"192.168.1.9:80": false,
		"example.com:80": false,
	}
	for addr, want := range cases {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestHostAllowed_LoopbackBind(t *testing.T) {
	s := &Server{addr: "127.0.0.1:8080"}
	allowed := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"}
	for _, h := range allowed {
		if !s.hostAllowed(h) {
			t.Errorf("hostAllowed(%q) = false, want true", h)
		}
	}
	// DNS rebinding：攻击者域名解析到本机，但 Host 头非回环 -> 拒绝
	denied := []string{"evil.com:8080", "attacker.example:8080"}
	for _, h := range denied {
		if s.hostAllowed(h) {
			t.Errorf("hostAllowed(%q) = true, want false (rebinding must be blocked)", h)
		}
	}
}

func TestHostAllowed_NonLoopbackBindWithToken(t *testing.T) {
	// 具体绑定主机放行其自身；通配绑定 + token 放行任意 Host（交由 token 鉴权）
	s := &Server{addr: "192.168.1.9:8080", token: "secret"}
	if !s.hostAllowed("192.168.1.9:8080") {
		t.Error("bind host should be allowed")
	}
	if s.hostAllowed("other.example:8080") {
		t.Error("non-bind, non-loopback host must be denied for specific bind")
	}

	wild := &Server{addr: "0.0.0.0:8080", token: "secret"}
	if !wild.hostAllowed("anything.example:8080") {
		t.Error("wildcard bind with token should allow any host (token guards it)")
	}
	wildNoToken := &Server{addr: "0.0.0.0:8080"}
	if wildNoToken.hostAllowed("anything.example:8080") {
		t.Error("wildcard bind without token must not blanket-allow hosts")
	}
}

func TestHostAllowed_ExtraAllowedHosts(t *testing.T) {
	s := &Server{addr: "127.0.0.1:8080", allowedHosts: []string{"proxy.internal"}}
	if !s.hostAllowed("proxy.internal:443") {
		t.Error("configured allowed host should pass")
	}
}

func TestOriginAllowed(t *testing.T) {
	s := &Server{addr: "127.0.0.1:8080"}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/tasks", http.NoBody)
	req.Host = "127.0.0.1:8080"
	// 同源
	if !s.originAllowed("http://127.0.0.1:8080", req) {
		t.Error("same-origin request must be allowed")
	}
	// 跨站 CSRF
	if s.originAllowed("http://evil.com", req) {
		t.Error("cross-site origin must be denied")
	}
	// 畸形 Origin
	if s.originAllowed("not-a-url", req) {
		t.Error("malformed origin must be denied")
	}
	if s.originAllowed("ftp://127.0.0.1:8080", req) {
		t.Error("non-HTTP origin must be denied")
	}
	if s.originAllowed("https://127.0.0.1:8080", req) {
		t.Error("cross-scheme origin must be denied")
	}
}

func TestOriginAllowed_WildcardBindStillRejectsCrossSite(t *testing.T) {
	s := &Server{
		addr:         "0.0.0.0:8080",
		token:        "0123456789abcdef",
		allowedHosts: []string{"app.example", "other.example"},
	}
	req := httptest.NewRequest(http.MethodPost, "http://app.example/api/tasks", http.NoBody)
	req.Host = "app.example"
	if !s.originAllowed("http://app.example", req) {
		t.Fatal("same external host should be allowed")
	}
	for _, origin := range []string{"http://evil.example", "http://other.example"} {
		if s.originAllowed(origin, req) {
			t.Errorf("origin %q passed wildcard/allowed-host boundary", origin)
		}
	}
}

func TestTokenValid(t *testing.T) {
	s := &Server{token: "s3cret"}

	mk := func(auth, query string) bool {
		r := newTestRequest(auth, query)
		return s.tokenValid(r)
	}
	if !mk("Bearer s3cret", "") {
		t.Error("valid bearer token should pass")
	}
	if !mk("", "s3cret") {
		t.Error("valid query token should pass")
	}
	if mk("Bearer wrong", "") {
		t.Error("wrong bearer token must fail")
	}
	if mk("", "wrong") {
		t.Error("wrong query token must fail")
	}
	if mk("", "") {
		t.Error("missing token must fail")
	}
	cookieReq := newTestRequest("", "")
	cookieReq.AddCookie(&http.Cookie{Name: webTokenCookie, Value: tokenCookieValue(s.token)})
	if !s.tokenValid(cookieReq) {
		t.Error("bootstrap cookie should pass")
	}
	badCookieReq := newTestRequest("", "")
	badCookieReq.AddCookie(&http.Cookie{Name: webTokenCookie, Value: "wrong"})
	if s.tokenValid(badCookieReq) {
		t.Error("invalid bootstrap cookie must fail")
	}
}

func TestWithSecurity_TokenBootstrapAndProtectedAPI(t *testing.T) {
	s := &Server{addr: "0.0.0.0:8080", token: "s3cret"}
	handler := s.withSecurity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	assetReq := httptest.NewRequest(http.MethodGet, "http://example.test/assets/app.js", http.NoBody)
	assetReq.Host = "example.test"
	assetRes := httptest.NewRecorder()
	handler.ServeHTTP(assetRes, assetReq)
	if assetRes.Code != http.StatusNoContent {
		t.Fatalf("static asset status = %d, want 204", assetRes.Code)
	}
	assetCookies := assetRes.Result().Cookies()
	if len(assetCookies) != 1 || assetCookies[0].Name != authCapabilityCookie || assetCookies[0].HttpOnly {
		t.Fatalf("UI auth capability cookie = %#v, want one readable marker", assetCookies)
	}

	for _, path := range []string{"/api/state", "/api/events", "/api/history/1/file"} {
		req := httptest.NewRequest(http.MethodGet, "http://example.test"+path, http.NoBody)
		req.Host = "example.test"
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", path, res.Code)
		}
		if got := res.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", path, got)
		}
	}

	bootstrapReq := httptest.NewRequest(
		http.MethodGet, "http://example.test/?token=s3cret&tab=tasks", http.NoBody,
	)
	bootstrapReq.Host = "example.test"
	bootstrapRes := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapRes, bootstrapReq)
	if bootstrapRes.Code != http.StatusSeeOther {
		t.Fatalf("bootstrap status = %d, want 303", bootstrapRes.Code)
	}
	if got := bootstrapRes.Header().Get("Location"); got != "/?tab=tasks" {
		t.Errorf("bootstrap Location = %q, want /?tab=tasks", got)
	}
	cookies := bootstrapRes.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("bootstrap cookies = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != webTokenCookie || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("bootstrap cookie flags = %#v", cookie)
	}
	if strings.Contains(cookie.Value, s.token) {
		t.Error("bootstrap cookie must not contain the raw token")
	}

	apiReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/state", http.NoBody)
	apiReq.Host = "example.test"
	apiReq.AddCookie(cookie)
	apiRes := httptest.NewRecorder()
	handler.ServeHTTP(apiRes, apiReq)
	if apiRes.Code != http.StatusNoContent {
		t.Errorf("cookie-authenticated API status = %d, want 204", apiRes.Code)
	}

	badBootstrapReq := httptest.NewRequest(http.MethodGet, "http://example.test/?token=wrong", http.NoBody)
	badBootstrapReq.Host = "example.test"
	badBootstrapRes := httptest.NewRecorder()
	handler.ServeHTTP(badBootstrapRes, badBootstrapReq)
	if badBootstrapRes.Code != http.StatusSeeOther || badBootstrapRes.Header().Get("Location") != "/" {
		t.Errorf("invalid bootstrap response = %d %q, want 303 /", badBootstrapRes.Code,
			badBootstrapRes.Header().Get("Location"))
	}
	if len(badBootstrapRes.Result().Cookies()) != 0 {
		t.Error("invalid bootstrap must not set an auth cookie")
	}
}

func TestWithSecurity_BootstrapCookieIsSecureBehindHTTPSProxy(t *testing.T) {
	s := &Server{addr: "0.0.0.0:8080", token: "s3cret", trustProxy: true}
	handler := s.withSecurity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "http://example.test/?token=s3cret", http.NoBody)
	req.Host = "example.test"
	req.Header.Set("X-Forwarded-Proto", "https")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	cookies := res.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Fatalf("HTTPS bootstrap cookie = %#v, want Secure", cookies)
	}
}

func TestWithSecurity_DoesNotTrustForwardedProtoByDefault(t *testing.T) {
	s := &Server{addr: "0.0.0.0:8080", token: "s3cret"}
	handler := s.withSecurity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "http://example.test/?token=s3cret", http.NoBody)
	req.Host = "example.test"
	req.Header.Set("X-Forwarded-Proto", "https")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	cookies := res.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Secure {
		t.Fatalf("untrusted forwarded proto cookie = %#v, want non-Secure", cookies)
	}
}

func TestWithSecurity_BootstrapRedirectStaysLocal(t *testing.T) {
	s := &Server{addr: "0.0.0.0:8080", token: "s3cret"}
	handler := s.withSecurity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name    string
		path    string
		rawPath string
	}{
		{name: "double slash", path: "//evil.example"},
		{name: "backslash", path: `/\evil.example`},
		{name: "encoded slash", path: "///evil.example", rawPath: "/%2f%2fevil.example"},
		{name: "encoded backslash", path: `/\evil.example`, rawPath: "/%5cevil.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://example.test/", http.NoBody)
			req.Host = "example.test"
			req.URL.Path = tt.path
			req.URL.RawPath = tt.rawPath
			req.URL.RawQuery = "token=s3cret&tab=tasks"
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)

			location := res.Header().Get("Location")
			if res.Code != http.StatusSeeOther || !strings.HasPrefix(location, "/") ||
				strings.HasPrefix(location, "//") || strings.Contains(location, `\`) {
				t.Fatalf("redirect = %d %q, want a local absolute path", res.Code, location)
			}
			if strings.Contains(location, "s3cret") || strings.Contains(location, "token") {
				t.Fatalf("redirect leaked token: %q", location)
			}
			if got := res.Header().Get("Referrer-Policy"); got != "no-referrer" {
				t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
			}
		})
	}
}

func TestRunRejectsRemoteListenWithoutTokenBeforeStartingDependencies(t *testing.T) {
	s := &Server{addr: "0.0.0.0:8080"}
	err := s.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), webTokenEnv) {
		t.Fatalf("Run() error = %v, want missing %s error", err, webTokenEnv)
	}
}

func TestRunRejectsWeakTokenBeforeStartingDependencies(t *testing.T) {
	for _, s := range []*Server{
		{addr: "0.0.0.0:8080", token: "short"},
		{addr: "127.0.0.1:8080", allowedHosts: []string{"downloads.example"}},
		{addr: "127.0.0.1:8080", trustProxy: true},
	} {
		err := s.Run(t.Context())
		if err == nil || !strings.Contains(err.Error(), webTokenEnv) {
			t.Fatalf("Run() error = %v, want %s strength requirement", err, webTokenEnv)
		}
	}
}

func TestParseAllowedHosts(t *testing.T) {
	got := parseAllowedHosts(" a.com , ,b.com,  ")
	if len(got) != 2 || got[0] != "a.com" || got[1] != "b.com" {
		t.Fatalf("parseAllowedHosts = %#v, want [a.com b.com]", got)
	}
	if parseAllowedHosts("") != nil {
		t.Error("empty input should yield nil")
	}
}

func TestEnvEnabled(t *testing.T) {
	for _, value := range []string{"1", "true", " TRUE "} {
		if !envEnabled(value) {
			t.Errorf("envEnabled(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "0", "false", "yes"} {
		if envEnabled(value) {
			t.Errorf("envEnabled(%q) = true, want false", value)
		}
	}
}
