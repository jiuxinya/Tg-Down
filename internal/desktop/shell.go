package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"tg-down/internal/logger"
)

// Shell 是桌面壳的本地 HTTP 服务：WebView 的唯一入口。
// 职责：
//   - 提供内嵌 SPA（与 Web 管理台同一份构建产物）
//   - /desktop/api/*：实例注册表、自启开关、应用信息等壳层能力
//   - /api/remote/{id}/*：反代远程 Docker 实例
//   - /api/*：反代本地引擎。WebView 的源是壳服务端口，引擎在另一个回环端口上，
//     界面对本机实例使用同源相对路径，必须由本层转发；否则请求会落入 SPA 回落
//     路由返回 index.html，界面将永远停在“正在连接”。
//
// 仅监听回环随机端口，不做令牌鉴权（与引擎的回环语义一致），但对带 Origin 的请求
// 强制同源校验：反代 Director 必须剥离 Origin 才能让引擎放行，那层 CSRF 防护因此
// 落到本层来做。
type Shell struct {
	reg        *Registry
	engineBase string // 如 http://127.0.0.1:54321
	appDir     string
	version    string
	autostart  Autostart
	log        *logger.Logger

	baseURL string
	srv     *http.Server
	onShow  func()
}

// NewShell 构造壳服务；engineBase 为本地引擎地址，appDir 用于界面展示
func NewShell(reg *Registry, engineBase, appDir, version string, a Autostart, log *logger.Logger) *Shell {
	return &Shell{
		reg:        reg,
		engineBase: strings.TrimRight(engineBase, "/"),
		appDir:     appDir,
		version:    version,
		autostart:  a,
		log:        log,
	}
}

// SetOnShow 注册“唤起主窗口”回调，供第二个实例经 POST /desktop/api/show 触发
func (s *Shell) SetOnShow(f func()) { s.onShow = f }

// Start 绑定 127.0.0.1 随机端口并在后台开始服务；返回可导航给 WebView 的根地址
func (s *Shell) Start(ctx context.Context) (string, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("绑定壳监听端口失败: %w", err)
	}
	// Origin 校验要拿本服务自身的源做对比，必须在挂载处理器之前定下来
	s.baseURL = "http://" + ln.Addr().String()

	mux := http.NewServeMux()
	mux.Handle("/", webUI())
	// 本地引擎反代：/api/remote/{id} 注册了更具体的模式，ServeMux 最长匹配优先，
	// 远程实例反代不受本条影响。
	engineURL, err := url.Parse(s.engineBase)
	if err != nil {
		_ = ln.Close()
		return "", fmt.Errorf("解析引擎地址失败: %w", err)
	}
	mux.Handle("/api/", localEngineProxy(engineURL))
	mux.HandleFunc("GET /desktop/api/info", s.handleInfo)
	mux.HandleFunc("GET /desktop/api/instances", s.handleList)
	mux.HandleFunc("POST /desktop/api/instances", s.handleAdd)
	mux.HandleFunc("PUT /desktop/api/instances/{id}", s.handleUpdate)
	mux.HandleFunc("DELETE /desktop/api/instances/{id}", s.handleDelete)
	mux.HandleFunc("POST /desktop/api/instances/{id}/test", s.handleTest)
	mux.HandleFunc("POST /desktop/api/select", s.handleSelect)
	mux.HandleFunc("GET /desktop/api/autostart", s.handleAutostartGet)
	mux.HandleFunc("PUT /desktop/api/autostart", s.handleAutostartSet)
	mux.HandleFunc("POST /desktop/api/show", s.handleShow)

	// 远程实例反代：按路径段动态分发到对应实例的 proxy
	mux.Handle(RemotePrefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.SplitN(strings.TrimPrefix(r.URL.Path, RemotePrefix), "/", 2)[0]
		if id == "" {
			writeErr(w, http.StatusNotFound, "缺少实例 ID")
			return
		}
		rp, err := newInstanceProxy(s.reg, id)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		rp.ServeHTTP(w, r)
	}))

	s.srv = &http.Server{
		Handler:           s.withOriginCheck(mux),
		ReadHeaderTimeout: readHeaderTimeout,
		// 不设 ReadTimeout/WriteTimeout：SSE 与媒体流为长连接
	}
	srv := s.srv
	// Shutdown 必须用独立的超时 ctx：此处的 ctx 已经取消（正是它触发了本次关闭），
	// 拿它去 Shutdown 会立即放弃优雅关闭，把仍在传输的 SSE 与媒体流直接掐断。
	go func() { //nolint:gosec // G118：这是关停协程，不是请求作用域
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownWait)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// 端口被占用、监听器提前失效等：不报出来的话界面只会永远停在“正在连接”
			s.logf("壳服务异常退出: %v", err)
		}
	}()
	return s.baseURL, nil
}

// Stop 优雅关闭壳服务
func (s *Shell) Stop() {
	if s.srv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownWait)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}
}

func (s *Shell) logf(format string, args ...any) {
	if s.log != nil {
		s.log.Error(format, args...)
	}
}

// withOriginCheck 对带 Origin 头的请求强制同源。
//
// 浏览器对跨站 fetch / 表单 POST 一定会带 Origin，而壳层的反代必须删掉该头才能
// 让引擎的同源校验放行（壳端口 ≠ 引擎端口），引擎那层的 CSRF 防护对经壳转发的
// 请求等于失效。因此 /api/、/api/remote/、/desktop/api/ 一律在进入路由前校验：
// 只有恰好等于壳自身源的 Origin 才放行，缺失 Origin 的同源导航与 Go 客户端不受影响。
func (s *Shell) withOriginCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin) {
			writeErr(w, http.StatusForbidden, "跨域请求被拒绝")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed 判断 Origin 是否与壳服务自身严格同源（含端口）
func (s *Shell) originAllowed(origin string) bool {
	if s.baseURL == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	self, err := url.Parse(s.baseURL)
	if err != nil {
		return false
	}
	return u.Scheme == self.Scheme && strings.EqualFold(u.Host, self.Host)
}

// localEngineProxy 返回把 /api/* 原样转发给本地引擎的反向代理。
//   - 路径与查询串（含引擎自身的 ?token= 鉴权语义）不动；
//   - 剥离本机页面附带的 Origin/Referer/Cookie：引擎按严格同源校验 Origin，
//     而壳端口 ≠ 引擎端口，不剥离会让所有 POST 被 403（跨站请求已在
//     withOriginCheck 拦下，剥离的只是同源请求的头）；
//   - FlushInterval=-1 逐字节冲刷，保证 SSE 事件流不经过缓冲。
func localEngineProxy(target *url.URL) http.Handler {
	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Transport:     proxyTransport(),
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.Header.Del("Origin")
			req.Header.Del("Referer")
			req.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, "无法连接到本地下载引擎: "+sanitizeErr(err))
		},
	}
	return rp
}

const (
	readHeaderTimeout = 10 * time.Second
	shutdownWait      = 3 * time.Second

	// maxRequestBodyBytes 限制壳层 API 的请求体大小
	maxRequestBodyBytes = 64 << 10
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// decodeBody 解析壳层 API 的 JSON 请求体，语义与 Web 端的 Server.decode 对齐：
// 强制 application/json（顺带挡掉 text/plain 这类无需预检的简单跨站请求）、
// 拒绝未知字段、且只接受恰好一个 JSON 值。
func decodeBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var body T
	if !requireJSONContentType(w, r) {
		return body, false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return body, false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			writeErr(w, http.StatusBadRequest, "请求体只能包含一个 JSON 值")
		} else {
			writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		}
		return body, false
	}
	return body, true
}

func requireJSONContentType(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		writeErr(w, http.StatusUnsupportedMediaType, "缺少 Content-Type: application/json")
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || mt != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "Content-Type 必须为 application/json")
		return false
	}
	return true
}
