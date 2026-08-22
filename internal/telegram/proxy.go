package telegram

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	tdclient "github.com/zelenin/go-tdlib/client"
)

// 代理环境变量回退链。TG_PROXY 是本项目专用入口；其后是社区通用的代理环境变量——
// 容器部署最常见的做法就是 docker -e HTTPS_PROXY=...，让它们对 Telegram 连接生效
// 正是 issue #49 的诉求（TDLib 自身不读这些变量，直连被墙的网络会永远卡在"正在连接"）。
var proxyEnvChain = []string{"TG_PROXY", "ALL_PROXY", "HTTPS_PROXY", "HTTP_PROXY"}

// proxyDisableValues 是显式禁用代理的特殊值（不区分大小写）。用于"环境里配了通用
// 代理变量、但想让 Telegram 直连"的场景：设 TG_PROXY=off 即可截断回退链。
var proxyDisableValues = map[string]struct{}{
	"direct": {},
	"off":    {},
	"none":   {},
}

const (
	// maxProxyPort 是合法代理端口上界（TCP 端口 1-65535）。
	maxProxyPort = 65535
)

// resolveTelegramProxy 解析生效的 Telegram 代理配置。
//
// 优先级：config.yaml telegram.proxy > proxyEnvChain 中的环境变量；全部为空表示直连，
// 显式的 direct/off/none 表示强制直连（截断回退链）。
//
// 返回的 source 用于日志标明代理解析自哪里（配置文件或哪个环境变量）；
// req 为 nil 表示不使用代理；配置存在但无法解析时返回错误——静默回退直连只会把问题
// 变回 issue #49 那样无限期卡在连接中。
func resolveTelegramProxy(explicit string) (req *tdclient.AddProxyRequest, source string, err error) {
	raw := strings.TrimSpace(explicit)
	if raw != "" {
		if _, disabled := proxyDisableValues[strings.ToLower(raw)]; disabled {
			return nil, "", nil
		}
		req, err := ParseProxyURL(raw)
		if err != nil {
			return nil, "", fmt.Errorf("telegram.proxy 配置无效: %w", err)
		}
		return req, "config.yaml telegram.proxy", nil
	}

	raw, envName := lookupProxyEnv()
	if raw == "" {
		return nil, "", nil // envName 非空时表示被显式禁用
	}
	req, err = ParseProxyURL(raw)
	if err != nil {
		return nil, "", fmt.Errorf("环境变量 %s 的代理地址无效: %w", envName, err)
	}
	return req, "环境变量 " + envName, nil
}

// lookupProxyEnv 按 proxyEnvChain 查找代理环境变量（大写优先于小写），返回第一个
// 非空的值及其变量名；该值为 direct/off/none 时视为显式禁用，返回空值并停止回退。
func lookupProxyEnv() (value, envName string) {
	for _, name := range proxyEnvChain {
		for _, candidate := range []string{os.Getenv(name), os.Getenv(strings.ToLower(name))} {
			if strings.TrimSpace(candidate) == "" {
				continue
			}
			if _, disabled := proxyDisableValues[strings.ToLower(strings.TrimSpace(candidate))]; disabled {
				return "", name
			}
			return strings.TrimSpace(candidate), name
		}
	}
	return "", ""
}

// 代理协议名（用于 URL scheme 判别与 describeProxy 的日志输出）
const (
	proxyKindSocks5  = "socks5"
	proxyKindHTTP    = "http"
	proxyKindMtproto = "mtproto"
)

// ParseProxyURL 把代理 URL 解析为 TDLib addProxy 请求（Enable=true，添加即启用）。
//
// 支持：
//   - socks5://[user:pass@]host:port（socks / socks5h 同义）
//   - http://[user:pass@]host:port —— HTTP CONNECT 透明转发；https:// 写法也按此处理
//     （Clash 等工具常见导出格式；TDLib 与代理之间不支持 TLS，若代理要求 TLS 会连不上）
//   - mtproto://secret@host:port
func ParseProxyURL(raw string) (*tdclient.AddProxyRequest, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("代理地址为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("无法解析 %s: %w", MaskProxyURL(raw), err)
	}

	var proxyType tdclient.ProxyType
	switch strings.ToLower(u.Scheme) {
	case proxyKindSocks5, "socks5h", "socks":
		username, password := proxyUserinfo(u.User)
		proxyType = &tdclient.ProxyTypeSocks5{Username: username, Password: password}
	case proxyKindHTTP, "https":
		username, password := proxyUserinfo(u.User)
		proxyType = &tdclient.ProxyTypeHttp{Username: username, Password: password}
	case proxyKindMtproto, "mt":
		secret := u.User.Username()
		if secret == "" {
			return nil, fmt.Errorf("MTProto 代理缺少 secret（应为 mtproto://secret@host:port）: %s", MaskProxyURL(raw))
		}
		proxyType = &tdclient.ProxyTypeMtproto{Secret: secret}
	default:
		return nil, fmt.Errorf(
			"不支持的代理协议 %q，支持 socks5://、http://、mtproto://，例如 socks5://127.0.0.1:1080: %s",
			u.Scheme, MaskProxyURL(raw))
	}

	host := u.Hostname() // 已剥去 IPv6 字面量的方括号
	if host == "" {
		return nil, fmt.Errorf("代理缺少主机名: %s", MaskProxyURL(raw))
	}
	portStr := u.Port()
	if portStr == "" {
		return nil, fmt.Errorf("代理缺少端口（如 socks5://127.0.0.1:1080）: %s", MaskProxyURL(raw))
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > maxProxyPort {
		return nil, fmt.Errorf("代理端口无效: %s", MaskProxyURL(raw))
	}

	return &tdclient.AddProxyRequest{
		Proxy: &tdclient.Proxy{
			Server: host,
			Port:   int32(port), //nolint:gosec // port 已限定在 1-65535
			Type:   proxyType,
		},
		Enable: true,
	}, nil
}

// describeProxy 生成不含凭据的代理描述（scheme://host:port），用于日志输出。
func describeProxy(req *tdclient.AddProxyRequest) string {
	if req == nil || req.Proxy == nil {
		return "(空)"
	}
	var kind string
	switch req.Proxy.Type.(type) {
	case *tdclient.ProxyTypeSocks5:
		kind = proxyKindSocks5
	case *tdclient.ProxyTypeHttp:
		kind = proxyKindHTTP
	case *tdclient.ProxyTypeMtproto:
		kind = proxyKindMtproto
	default:
		kind = "unknown"
	}
	return fmt.Sprintf("%s://%s:%d", kind, req.Proxy.Server, req.Proxy.Port)
}

// MaskProxyURL 隐藏代理 URL 中的凭据（用户名/密码/MTProto secret），用于日志与错误信息。
// 不用 url.UserPassword("***") 是因为它会把 * 百分号转义成 %2A，反而可读性差。
func MaskProxyURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" {
		return "***"
	}
	if u.User == nil {
		return u.String()
	}
	masked := *u
	masked.User = nil
	withoutUserinfo := masked.String()
	if i := strings.Index(withoutUserinfo, "://"); i >= 0 {
		return withoutUserinfo[:i+3] + "***@" + withoutUserinfo[i+3:]
	}
	return withoutUserinfo
}

// proxyUserinfo 提取 URL 中的登录凭据（百分号编码由标准库解码）；无凭据时返回空串。
func proxyUserinfo(u *url.Userinfo) (username, password string) {
	if u == nil {
		return "", ""
	}
	password, _ = u.Password()
	return u.Username(), password
}
