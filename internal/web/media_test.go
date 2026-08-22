package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"tg-down/internal/logger"
	"tg-down/internal/store"
)

// newMediaTestServer 建一个只带 store + 下载根目录的 Server：媒体端点不碰 telegram/queue。
func newMediaTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return &Server{
		store:        st,
		logger:       logger.New("error"),
		downloadRoot: root,
	}, root
}

// addRecord 写入一条 completed 记录并返回其 id
func addRecord(t *testing.T, s *Server, rec *store.HistoryRecord) int64 {
	t.Helper()
	ctx := context.Background()
	rec.Status = store.HistoryStatusQueued
	if err := s.store.UpsertHistoryStart(ctx, rec); err != nil {
		t.Fatalf("UpsertHistoryStart() error = %v", err)
	}
	if err := s.store.UpdateHistoryResult(
		ctx, rec.ChatID, rec.MessageID, store.HistoryStatusCompleted, "", rec.FilePath,
	); err != nil {
		t.Fatalf("UpdateHistoryResult() error = %v", err)
	}
	items, _, err := s.store.QueryHistory(ctx, &store.HistoryFilter{})
	if err != nil {
		t.Fatalf("QueryHistory() error = %v", err)
	}
	for _, it := range items {
		if it.ChatID == rec.ChatID && it.MessageID == rec.MessageID {
			return it.ID
		}
	}
	t.Fatal("找不到刚写入的记录")
	return 0
}

func serveFile(s *Server, id string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/history/"+id+"/file", nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.handleHistoryFile(w, r)
	return w
}

func serveThumb(s *Server, id string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/history/"+id+"/thumb", nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.handleHistoryThumb(w, r)
	return w
}

// TestHandleHistoryFile_ServesRecordedFile 校验正常路径：按库 id 取到文件并带上正确的头
func TestHandleHistoryFile_ServesRecordedFile(t *testing.T) {
	s, root := newMediaTestServer(t)

	path := filepath.Join(root, "chat_1", "photo", "a.jpg")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("JPEGDATA"), 0o600); err != nil {
		t.Fatal(err)
	}

	id := addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "photo", FileName: "a.jpg",
		FilePath: path, FileSize: 8, MimeType: "image/jpeg",
	})

	w := serveFile(s, strconv.FormatInt(id, 10))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200（%s）", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "JPEGDATA" {
		t.Errorf("响应体 = %q, want JPEGDATA", got)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if disposition := w.Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, "inline;") {
		t.Errorf("Content-Disposition = %q, want inline", disposition)
	}
	// 媒体是他人提供的内容，必须禁止浏览器按内容嗅探类型
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("媒体响应必须带 X-Content-Type-Options: nosniff")
	}
}

func TestHandleHistoryFile_ActiveContentIsAttachment(t *testing.T) {
	s, root := newMediaTestServer(t)
	tests := []struct {
		name      string
		fileName  string
		mediaType string
		content   string
	}{
		{name: "html", fileName: "page.html", mediaType: "text/html", content: "<script>alert(1)</script>"},
		{name: "javascript", fileName: "payload.js", mediaType: "application/javascript", content: "alert(1)"},
		{name: "svg", fileName: "image.svg", mediaType: "image/svg+xml", content: `<svg onload="alert(1)"/>`},
		{name: "pdf", fileName: "doc.pdf", mediaType: "application/pdf", content: "%PDF-1.7"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(root, tt.fileName)
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			id := addRecord(t, s, &store.HistoryRecord{
				ChatID: 1, MessageID: int64(i + 1), MediaType: "document",
				FileName: tt.fileName, FilePath: path, MimeType: tt.mediaType,
			})

			w := serveFile(s, strconv.FormatInt(id, 10))
			if w.Code != http.StatusOK {
				t.Fatalf("状态码 = %d, want 200", w.Code)
			}
			if got := w.Header().Get("Content-Type"); got != attachmentMediaType {
				t.Errorf("Content-Type = %q, want %q", got, attachmentMediaType)
			}
			if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
				t.Errorf("Content-Disposition = %q, want attachment", got)
			}
			if got := w.Header().Get("Content-Security-Policy"); got != mediaContentSecurityCSP {
				t.Errorf("Content-Security-Policy = %q, want %q", got, mediaContentSecurityCSP)
			}
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

func TestHandleHistoryFile_VideoRangeRemainsAvailable(t *testing.T) {
	s, root := newMediaTestServer(t)
	path := filepath.Join(root, "video.mp4")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "video", FileName: "video.mp4",
		FilePath: path, FileSize: 10, MimeType: "video/mp4",
	})
	req := httptest.NewRequest(http.MethodGet, "/api/history/1/file", nil)
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	req.Header.Set("Range", "bytes=2-5")
	w := httptest.NewRecorder()
	s.handleHistoryFile(w, req)

	if w.Code != http.StatusPartialContent || w.Body.String() != "2345" {
		t.Fatalf("Range response = %d %q, want 206 %q", w.Code, w.Body.String(), "2345")
	}
	if got := w.Header().Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Errorf("Content-Disposition = %q, want inline", got)
	}
}

// TestHandleHistoryFile_RejectsEscapingPath 是核心安全断言：
// file_path 最终源自 Telegram 提供的文件名，一旦它指向下载根目录之外，必须拒绝。
func TestHandleHistoryFile_RejectsEscapingPath(t *testing.T) {
	s, _ := newMediaTestServer(t)

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	id := addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "document", FileName: "secret.txt",
		FilePath: outside, FileSize: 6,
	})

	w := serveFile(s, strconv.FormatInt(id, 10))
	if w.Code != http.StatusForbidden {
		t.Errorf("状态码 = %d, want 403（越界路径必须拒绝）", w.Code)
	}
	if w.Body.String() == "SECRET" {
		t.Fatal("下载根目录之外的文件被外发了")
	}
}

func TestHandleHistoryFile_RejectsSymlinkEscape(t *testing.T) {
	s, root := newMediaTestServer(t)

	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("SYMLINK_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(root, "chat_1")
	if err := os.Symlink(outsideDir, linkedDir); err != nil {
		t.Skipf("当前文件系统不支持符号链接: %v", err)
	}

	id := addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "document", FileName: "secret.txt",
		FilePath: filepath.Join(linkedDir, "secret.txt"), FileSize: 14,
	})

	w := serveFile(s, strconv.FormatInt(id, 10))
	if w.Code != http.StatusForbidden {
		t.Errorf("状态码 = %d, want 403（符号链接越界必须拒绝）", w.Code)
	}
	if w.Body.String() == "SYMLINK_SECRET" {
		t.Fatal("通过下载根内的符号链接外发了根外文件")
	}
}

func TestHandleHistoryFile_MissingTargetReturnsNotFound(t *testing.T) {
	s, root := newMediaTestServer(t)
	id := addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "document", FileName: "missing.txt",
		FilePath: filepath.Join(root, "chat_1", "missing.txt"),
	})

	if w := serveFile(s, strconv.FormatInt(id, 10)); w.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d, want 404（不存在的目标应安全失败）", w.Code)
	}
}

// TestHandleHistoryFile_BlocksSidecar 校验元数据 sidecar（含 caption/发送者）不可经媒体端点取走
func TestHandleHistoryFile_BlocksSidecar(t *testing.T) {
	s, root := newMediaTestServer(t)

	path := filepath.Join(root, "chat_1", "a.jpg.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"caption":"私密"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	id := addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "document", FileName: "a.jpg.json", FilePath: path,
	})

	if w := serveFile(s, strconv.FormatInt(id, 10)); w.Code != http.StatusForbidden {
		t.Errorf("状态码 = %d, want 403（sidecar 不可外发）", w.Code)
	}
}

// TestHandleHistoryFile_NotCompleted 校验未完成的下载不给文件（磁盘上要么没有、要么是半截）
func TestHandleHistoryFile_NotCompleted(t *testing.T) {
	s, root := newMediaTestServer(t)
	ctx := context.Background()

	rec := &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "photo", FileName: "a.jpg",
		FilePath: filepath.Join(root, "a.jpg"), Status: store.HistoryStatusQueued,
	}
	if err := s.store.UpsertHistoryStart(ctx, rec); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.store.QueryHistory(ctx, &store.HistoryFilter{})
	if err != nil || len(items) != 1 {
		t.Fatalf("QueryHistory() = %v, err = %v", items, err)
	}

	if w := serveFile(s, strconv.FormatInt(items[0].ID, 10)); w.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d, want 404（未完成的下载不该给文件）", w.Code)
	}
}

func TestHandleHistoryFile_BadID(t *testing.T) {
	s, _ := newMediaTestServer(t)
	for _, id := range []string{"abc", "0", "-1"} {
		if w := serveFile(s, id); w.Code != http.StatusBadRequest {
			t.Errorf("id=%q 状态码 = %d, want 400", id, w.Code)
		}
	}
	if w := serveFile(s, "99999"); w.Code != http.StatusNotFound {
		t.Errorf("不存在的 id 状态码 = %d, want 404", w.Code)
	}
}

// TestHandleHistoryThumb_FallsBackToMinithumb 校验缩略图回退链：
// 缩略图文件 → minithumb（随消息免费返回）→ 404
func TestHandleHistoryThumb_FallsBackToMinithumb(t *testing.T) {
	s, root := newMediaTestServer(t)
	ctx := context.Background()

	mini := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00} // 假 JPEG 头
	id := addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "video", FileName: "v.mp4",
		FilePath: filepath.Join(root, "v.mp4"), Minithumb: mini,
	})

	// 尚无缩略图文件 → 回退 minithumb
	w := serveThumb(s, strconv.FormatInt(id, 10))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200（应回退到 minithumb）", w.Code)
	}
	if w.Body.Len() != len(mini) {
		t.Errorf("响应体长度 = %d, want %d（minithumb）", w.Body.Len(), len(mini))
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if cache := w.Header().Get("Cache-Control"); !strings.HasPrefix(cache, "private,") || strings.Contains(cache, "public") {
		t.Errorf("Cache-Control = %q, want private browser cache", cache)
	}

	// 有缩略图文件时优先用它
	thumbPath := filepath.Join(s.thumbsRoot(), "u1.jpg")
	if err := os.MkdirAll(filepath.Dir(thumbPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumbPath, []byte("REALTHUMBDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetHistoryThumb(ctx, 1, 1, thumbPath); err != nil {
		t.Fatal(err)
	}

	w = serveThumb(s, strconv.FormatInt(id, 10))
	if w.Code != http.StatusOK || w.Body.String() != "REALTHUMBDATA" {
		t.Errorf("有缩略图文件时应优先返回它，得到 %d / %q", w.Code, w.Body.String())
	}
}

// TestHandleHistoryThumb_NoThumb 校验既无缩略图文件也无 minithumb 时返回 404
func TestHandleHistoryThumb_NoThumb(t *testing.T) {
	s, root := newMediaTestServer(t)
	id := addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "voice", FileName: "v.ogg",
		FilePath: filepath.Join(root, "v.ogg"),
	})
	if w := serveThumb(s, strconv.FormatInt(id, 10)); w.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d, want 404", w.Code)
	}
}

// TestHandleHistoryThumb_RejectsEscapingThumbPath 校验 thumb_path 同样受根目录约束
func TestHandleHistoryThumb_RejectsEscapingThumbPath(t *testing.T) {
	s, root := newMediaTestServer(t)
	ctx := context.Background()

	outside := filepath.Join(t.TempDir(), "escape.jpg")
	if err := os.WriteFile(outside, []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "video", FileName: "v.mp4",
		FilePath: filepath.Join(root, "v.mp4"),
	})
	if err := s.store.SetHistoryThumb(ctx, 1, 1, outside); err != nil {
		t.Fatal(err)
	}

	w := serveThumb(s, strconv.FormatInt(id, 10))
	if w.Body.String() == "OUTSIDE" {
		t.Fatal("缩略图目录之外的文件被外发了")
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d, want 404（越界的 thumb_path 应被忽略并回退）", w.Code)
	}
}

func TestHandleHistoryThumb_RejectsSymlinkEscape(t *testing.T) {
	s, root := newMediaTestServer(t)
	ctx := context.Background()

	outside := filepath.Join(t.TempDir(), "outside.jpg")
	if err := os.WriteFile(outside, []byte("OUTSIDE_THUMB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.thumbsRoot(), 0o750); err != nil {
		t.Fatal(err)
	}
	thumbPath := filepath.Join(s.thumbsRoot(), "linked.jpg")
	if err := os.Symlink(outside, thumbPath); err != nil {
		t.Skipf("当前文件系统不支持符号链接: %v", err)
	}

	id := addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "video", FileName: "v.mp4",
		FilePath: filepath.Join(root, "v.mp4"),
	})
	if err := s.store.SetHistoryThumb(ctx, 1, 1, thumbPath); err != nil {
		t.Fatal(err)
	}

	w := serveThumb(s, strconv.FormatInt(id, 10))
	if w.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d, want 404（越界缩略图应被忽略）", w.Code)
	}
	if w.Body.String() == "OUTSIDE_THUMB" {
		t.Fatal("通过缩略图符号链接外发了根外文件")
	}
}

// TestHeaderSafeFileName 校验文件名不能把 Content-Disposition 响应头拆开
func TestHeaderSafeFileName(t *testing.T) {
	cases := map[string]string{
		`normal.jpg`:         `normal.jpg`,
		`a"b.jpg`:            `a_b.jpg`,
		"a\r\nX-Evil: 1.jpg": "a__X-Evil: 1.jpg",
		`back\slash.jpg`:     `back_slash.jpg`,
		``:                   `file`,
		"中文名.jpg":            "中文名.jpg",
	}
	for in, want := range cases {
		if got := headerSafeFileName(in); got != want {
			t.Errorf("headerSafeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
