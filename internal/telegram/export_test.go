package telegram

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	tdclient "github.com/zelenin/go-tdlib/client"

	mediapkg "tg-down/internal/media"
)

// TestRenderExportHTML_EscapesUserContent 是导出最重要的一条约束：
// 消息文本、文件名、聊天标题全部来自 Telegram（他人可控）。不转义就等于把 XSS
// 直接写进用户会用浏览器打开的文件里。
func TestRenderExportHTML_EscapesUserContent(t *testing.T) {
	p := exportPayload{
		ChatID:    -100,
		ChatTitle: `<script>alert('title')</script>`,
		Count:     1,
		Messages: []exportMessage{{
			ID:        1,
			Text:      `<img src=x onerror=alert('xss')>`,
			MediaType: mediapkg.Document,
			FileName:  `"><script>alert('name')</script>.pdf`,
			FilePath:  `../chat_-100/document/evil".pdf`,
		}},
	}

	out := renderExportHTML(p)

	for _, bad := range []string{
		"<script>alert('title')</script>",
		"<img src=x onerror=alert('xss')>",
		"<script>alert('name')</script>",
	} {
		if strings.Contains(out, bad) {
			t.Errorf("导出 HTML 中出现未转义的用户内容: %q", bad)
		}
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Error("用户内容应被转义为实体，未找到 &lt;script&gt;")
	}
}

func TestWriteExportConcurrentCallsUseUniquePrivateFiles(t *testing.T) {
	c := newTestClient(t)
	spec := ExportSpec{ChatID: 42, ChatTitle: "test"}
	const count = 8
	results := make(chan *ExportResult, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.writeExport(spec, []exportMessage{{ID: 1, Text: "private"}}, false)
			if err != nil {
				errs <- err
				return
			}
			results <- res
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Errorf("writeExport() error = %v", err)
	}

	seen := make(map[string]bool, count*2)
	for res := range results {
		for _, path := range []string{res.JSONPath, res.HTMLPath} {
			if seen[path] {
				t.Errorf("并发导出复用了路径: %s", path)
			}
			seen[path] = true
			info, err := os.Stat(path)
			if err != nil {
				t.Errorf("导出文件不存在: %v", err)
				continue
			}
			if info.Mode().Perm() != exportFilePerm {
				t.Errorf("%s 权限 = %o, want %o", path, info.Mode().Perm(), exportFilePerm)
			}
		}
	}
	if len(seen) != count*2 {
		t.Fatalf("导出文件数 = %d, want %d", len(seen), count*2)
	}
}

func TestCollectExportMessagesUsesUnfilteredSearchEndSignal(t *testing.T) {
	c := newTestClient(t)
	fake := &fakeSearchAPI{
		byFilter: map[string][]*tdclient.Message{
			tdclient.ConstructorSearchMessagesFilterEmpty: {
				{Id: 30, Content: &tdclient.MessageText{Text: &tdclient.FormattedText{Text: "new"}}},
				{Id: 20, Content: &tdclient.MessageText{Text: &tdclient.FormattedText{Text: "middle"}}},
				{Id: 10, Content: &tdclient.MessageText{Text: &tdclient.FormattedText{Text: "old"}}},
			},
		},
		pageSize: 2,
	}
	msgs, truncated, err := c.collectExportMessages(context.Background(), fake, ExportSpec{ChatID: 1})
	if err != nil {
		t.Fatalf("collectExportMessages() error = %v", err)
	}
	if truncated {
		t.Error("完整导出不应标记为截断")
	}
	if fake.historyHit != 0 {
		t.Errorf("导出不应调用 GetChatHistory，实际调用 %d 次", fake.historyHit)
	}
	if len(msgs) != 3 || msgs[0].ID != 10 || msgs[2].ID != 30 {
		t.Errorf("导出消息顺序/数量错误: %+v", msgs)
	}
}

// TestRenderExportHTML_MediaRendering 校验媒体按类型渲染：
// 图片用 <img>、视频用 <video>、其余给链接、未下载的给出明确提示而不是死链
func TestRenderExportHTML_MediaRendering(t *testing.T) {
	p := exportPayload{
		ChatID: 1,
		Count:  4,
		Messages: []exportMessage{
			{ID: 1, MediaType: mediapkg.Photo, FileName: "a.jpg", FilePath: "../chat_1/photo/a.jpg"},
			{ID: 2, MediaType: mediapkg.Video, FileName: "b.mp4", FilePath: "../chat_1/video/b.mp4"},
			{ID: 3, MediaType: mediapkg.Document, FileName: "c.zip", FilePath: "../chat_1/document/c.zip"},
			{ID: 4, MediaType: mediapkg.Photo, FileName: "d.jpg"}, // 未下载：FilePath 为空
		},
	}

	out := renderExportHTML(p)

	if !strings.Contains(out, `<img loading="lazy" src="../chat_1/photo/a.jpg"`) {
		t.Error("图片应渲染为 <img>")
	}
	if !strings.Contains(out, `<video controls preload="none" src="../chat_1/video/b.mp4">`) {
		t.Error("视频应渲染为 <video>")
	}
	if !strings.Contains(out, `<a href="../chat_1/document/c.zip">c.zip</a>`) {
		t.Error("其他文件应渲染为链接")
	}
	if !strings.Contains(out, "尚未下载") {
		t.Error("未下载的媒体应明确标注，而不是留下打不开的死链")
	}
}

// TestMessageBodyText 校验正文提取：纯文本取 text，媒体消息取 caption
func TestMessageBodyText(t *testing.T) {
	cases := []struct {
		name    string
		content tdclient.MessageContent
		want    string
	}{
		{"纯文本", &tdclient.MessageText{Text: &tdclient.FormattedText{Text: "你好"}}, "你好"},
		{"文本为 nil", &tdclient.MessageText{}, ""},
		{"图片取 caption", &tdclient.MessagePhoto{Caption: &tdclient.FormattedText{Text: "说明"}}, "说明"},
		{"贴纸无正文", &tdclient.MessageSticker{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageBodyText(tc.content); got != tc.want {
				t.Errorf("messageBodyText() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReverseMessages 校验导出按时间正序（TDLib 历史是倒序返回的）
func TestReverseMessages(t *testing.T) {
	got := reverseMessages([]exportMessage{{ID: 3}, {ID: 2}, {ID: 1}})
	for i, want := range []int64{1, 2, 3} {
		if got[i].ID != want {
			t.Errorf("第 %d 条 id = %d, want %d（导出应按时间正序）", i, got[i].ID, want)
		}
	}
	if len(reverseMessages(nil)) != 0 {
		t.Error("空输入应返回空")
	}
}
