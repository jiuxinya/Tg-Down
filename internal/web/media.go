package web

import (
	"bytes"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"tg-down/internal/store"
)

// minithumbMaxAge 是 minithumb 的浏览器缓存时长。内容按 history id 寻址且永不改变，
// 缓存久一点可以让画廊滚动时不再回源。
const (
	minithumbMaxAge         = "private, max-age=86400"
	mediaContentSecurityCSP = "default-src 'none'; sandbox"
	attachmentMediaType     = "application/octet-stream"
)

// blockedServeSuffixes 是即使被 DB 记录也绝不外发的文件后缀。
// .json 是元数据 sidecar（含 caption、发送者 id 等），不该经媒体端点泄漏出去。
var blockedServeSuffixes = []string{".json"}

// inlineMediaTypes 只包含浏览器可直接解码、不会作为主动文档或脚本执行的格式。
// SVG、PDF、HTML、XML、JavaScript 和所有未知 MIME 都必须走附件下载。
var inlineMediaTypes = map[string]struct{}{
	"image/jpeg":       {},
	"image/png":        {},
	"image/gif":        {},
	"image/webp":       {},
	"image/avif":       {},
	"image/bmp":        {},
	"audio/aac":        {},
	"audio/flac":       {},
	"audio/m4a":        {},
	"audio/mp4":        {},
	"audio/mpeg":       {},
	"audio/ogg":        {},
	"audio/wav":        {},
	"audio/webm":       {},
	"audio/x-m4a":      {},
	"audio/x-wav":      {},
	"video/mp4":        {},
	"video/mpeg":       {},
	"video/ogg":        {},
	"video/quicktime":  {},
	"video/webm":       {},
	"video/x-matroska": {},
}

// handleHistoryFile 按 history id 返回原始文件，支持 Range（视频可拖动进度条）。
//
// 安全模型：只按库里的 id 查路径，因此可达文件集合 == 已记录的下载文件集合。
// 下载根目录里的 .tdlib-files 缓存、.thumbs 缓存、.json sidecar 从来没有 history 行，
// 因此天然不可达——这比开一个 FileServer 再逐个打补丁排除要可靠得多。
// 即便如此，仍再做一次根目录越界校验：file_path 来自 DB，而 DB 里的值最终源于
// Telegram 提供的文件名，不能假定它一定规矩。
func (s *Server) handleHistoryFile(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookupServableRecord(w, r)
	if !ok {
		return
	}
	path, ok := s.safeServePath(w, rec.FilePath)
	if !ok {
		return
	}

	f, err := os.Open(path) // #nosec G304 -- 路径来自 DB 记录并已过 safeServePath 的根目录校验
	if err != nil {
		s.writeError(w, http.StatusNotFound, "文件不存在（可能已被移动或删除）")
		return
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil || st.IsDir() {
		s.writeError(w, http.StatusNotFound, "文件不可读")
		return
	}

	setMediaResponseHeaders(w, rec.MimeType, rec.FileName)

	// ServeContent 负责 Range / If-Modified-Since / 206 等一整套语义
	http.ServeContent(w, r, rec.FileName, st.ModTime(), f)
}

// handleHistoryThumb 返回缩略图：优先已下载的缩略图文件，回退到随消息免费返回的
// minithumb（约 40x40 的 JPEG，几百字节）。两者都没有则 404，前端据此显示占位图标。
func (s *Server) handleHistoryThumb(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookupServableRecord(w, r)
	if !ok {
		return
	}

	if rec.ThumbPath != "" {
		if path, safe := s.safePathUnder(rec.ThumbPath, s.thumbsRoot()); safe {
			// .thumbs 自身也可能被替换成指向下载根外的符号链接，因此还要以
			// downloadRoot 再校验一次最终路径。
			if path, safe = s.safePathUnder(path, s.downloadRoot); safe {
				if f, err := os.Open(path); err == nil { // #nosec G304 -- 已过两层真实路径校验
					defer func() { _ = f.Close() }()
					if st, err := f.Stat(); err == nil && !st.IsDir() {
						w.Header().Set("Content-Type", "image/jpeg")
						w.Header().Set("Cache-Control", minithumbMaxAge)
						http.ServeContent(w, r, "thumb.jpg", st.ModTime(), f)
						return
					}
				}
			}
		}
	}

	if len(rec.Minithumb) > 0 {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", minithumbMaxAge)
		http.ServeContent(w, r, "thumb.jpg", rec.CreatedAt, bytes.NewReader(rec.Minithumb))
		return
	}

	s.writeError(w, http.StatusNotFound, "该媒体没有缩略图")
}

// lookupServableRecord 解析路径中的 id 并取出可外发的记录；
// 未完成下载的记录不给文件（磁盘上要么没有、要么是半截）
func (s *Server) lookupServableRecord(w http.ResponseWriter, r *http.Request) (*store.HistoryRecord, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeError(w, http.StatusBadRequest, "无效的记录 id")
		return nil, false
	}

	rec, err := s.store.GetHistoryByID(r.Context(), id)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if rec == nil {
		s.writeError(w, http.StatusNotFound, "记录不存在")
		return nil, false
	}
	if rec.Status != store.HistoryStatusCompleted {
		s.writeError(w, http.StatusNotFound, "该媒体尚未下载完成")
		return nil, false
	}
	return rec, true
}

// safeServePath 校验待外发的文件路径落在下载根目录内、且不是被禁的类型
func (s *Server) safeServePath(w http.ResponseWriter, raw string) (string, bool) {
	if raw == "" {
		s.writeError(w, http.StatusNotFound, "记录未包含文件路径")
		return "", false
	}
	if blockedServePath(raw) {
		s.writeError(w, http.StatusForbidden, "该文件类型不可访问")
		return "", false
	}
	path, ok := s.safePathUnder(raw, s.downloadRoot)
	if !ok {
		if _, err := os.Stat(raw); os.IsNotExist(err) {
			s.writeError(w, http.StatusNotFound, "文件不存在（可能已被移动或删除）")
			return "", false
		}
		s.writeError(w, http.StatusForbidden, "文件路径越界")
		return "", false
	}
	// raw 可能是看似无害的符号链接，真实目标却是元数据 sidecar。
	if blockedServePath(path) {
		s.writeError(w, http.StatusForbidden, "该文件类型不可访问")
		return "", false
	}
	return path, true
}

func blockedServePath(path string) bool {
	for _, suffix := range blockedServeSuffixes {
		if strings.HasSuffix(strings.ToLower(path), suffix) {
			return true
		}
	}
	return false
}

func setMediaResponseHeaders(w http.ResponseWriter, rawMediaType, fileName string) {
	mediaType, inline := safeInlineMediaType(rawMediaType)
	disposition := "inline"
	if !inline {
		mediaType = attachmentMediaType
		disposition = "attachment"
	}
	h := w.Header()
	h.Set("Content-Type", mediaType)
	h.Set("Content-Disposition", disposition+`; filename="`+headerSafeFileName(fileName)+`"`)
	h.Set("Content-Security-Policy", mediaContentSecurityCSP)
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
}

func safeInlineMediaType(raw string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return "", false
	}
	mediaType = strings.ToLower(mediaType)
	_, ok := inlineMediaTypes[mediaType]
	return mediaType, ok
}

// safePathUnder 报告 raw 的最终真实路径是否位于 root 的最终真实路径之内。
// filepath.Rel 只检查字符串层级；先 EvalSymlinks 才能阻止根内链接指向根外文件。
func (s *Server) safePathUnder(raw, root string) (string, bool) {
	if raw == "" || root == "" {
		return "", false
	}
	absPath, err := filepath.Abs(raw)
	if err != nil {
		return "", false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", false
	}
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return realPath, true
}

// thumbsRoot 是缩略图缓存目录
func (s *Server) thumbsRoot() string {
	return filepath.Join(s.downloadRoot, ".thumbs")
}

// headerSafeFileName 清理文件名中会破坏 Content-Disposition 响应头的字符。
// 文件名来自 Telegram（他人可控），带引号或换行就能把响应头拆开。
func headerSafeFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if name == "" {
		return "file"
	}
	return name
}
