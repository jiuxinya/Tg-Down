package config

import (
	"net/url"
	"strings"
)

// 代理地址的轻量校验与脱敏，供 Web 设置页在不动 CGo 边界（internal/telegram）的前提下
// 预检用户输入。最终以 TDLib 连接时的解析为准；scheme 集合需与 telegram.ParseProxyURL 保持一致。

// proxySchemes 是允许的代理 scheme。更新时同步 internal/telegram/proxy.go 的解析逻辑。
var proxySchemes = map[string]bool{
	"socks5": true, "socks": true, "socks5h": true,
	"http": true, "https": true,
	"mtproto": true,
}

// directProxyValues 表示强制直连的特殊值，语义同 TG_PROXY 文档
var directProxyValues = map[string]bool{"direct": true, "off": true, "none": true}

// IsValidProxy 报告代理配置是否为可接受的形态：空串（未配置）、强制直连特殊值，
// 或带允许 scheme 与主机的 URL。仅做格式预检，不发起网络请求。
func IsValidProxy(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || directProxyValues[strings.ToLower(raw)] {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if !proxySchemes[strings.ToLower(u.Scheme)] || u.Host == "" {
		return false
	}
	if u.Path != "" && u.Path != "/" {
		return false
	}
	return u.Opaque == "" && u.RawQuery == "" && u.Fragment == ""
}

// MaskProxyURL 脱敏代理地址中的用户信息（socks5://user:pass@host → socks5://user:***@host）。
// 非 URL 形态原样返回。
func MaskProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	username := u.User.Username()
	if _, hasPass := u.User.Password(); hasPass {
		u.User = url.UserPassword(username, "***")
	} else if username != "" {
		u.User = url.User("***")
	}
	return u.String()
}
