package web

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// UIHandler 返回内嵌前端的静态处理器。桌面壳与 Web 端共用同一份 Vite 构建产物：
// 带扩展名的路径按静态资源直出（assets/ 附一年不可变缓存），其余路径回落 index.html。
// 无服务器状态依赖，可在任意 mux 上挂载。
//
// 外部挂载方（桌面壳）没有 withSecurity 包裹，CSP / nosniff / X-Frame-Options 等
// 响应头必须在此补齐，否则同一份 SPA 在壳里运行时完全没有这层防护。
// Web 服务器自身走 handleUI 直调，安全头已由 withSecurity 施加，不重复经过这里。
func UIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		handleUI(w, r)
	})
}

func handleUI(w http.ResponseWriter, r *http.Request) {
	if !uiBuilt() {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(uiNotBuiltMessage))
		return
	}

	// 带扩展名的路径当作静态资源；命中就直接返回（带上长缓存——Vite 的文件名里有内容哈希）
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name != "" && path.Ext(name) != "" {
		if f, err := uiFS.Open(name); err == nil {
			_ = f.Close()
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.FileServerFS(uiFS).ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	// 其余一律回 index.html，交给前端路由
	data, err := fs.ReadFile(uiFS, "index.html")
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("读取前端产物失败"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
