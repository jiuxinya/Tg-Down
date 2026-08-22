package telegram

import (
	"testing"

	tdclient "github.com/zelenin/go-tdlib/client"

	mediapkg "tg-down/internal/media"
)

func fileWithID(id int32, size int64, uniqueID string) *tdclient.File {
	return &tdclient.File{
		Id:     id,
		Size:   size,
		Remote: &tdclient.RemoteFile{UniqueId: uniqueID},
	}
}

// TestExtractMediaFile 校验各消息类型的媒体提取：类型、文件名、MIME 与去重键
func TestExtractMediaFile(t *testing.T) {
	c := &Client{}

	cases := []struct {
		name      string
		content   tdclient.MessageContent
		wantType  string
		wantName  string
		wantMime  string
		wantNil   bool
		wantUniq  string
		wantSize  int64
		wantFile  int32
		msgID     int64
		chatIDVal int64
	}{
		{
			name: "photo 取最大尺寸并合成文件名",
			content: &tdclient.MessagePhoto{Photo: &tdclient.Photo{Sizes: []*tdclient.PhotoSize{
				{Width: 100, Height: 100, Photo: fileWithID(1, 1000, "small")},
				{Width: 800, Height: 600, Photo: fileWithID(2, 9000, "large")},
			}}},
			wantType: mediapkg.Photo,
			wantName: "photo_-100_42.jpg",
			wantMime: "image/jpeg",
			wantUniq: "large",
			wantSize: 9000,
			wantFile: 2,
		},
		{
			name: "document 保留原始文件名并加消息ID前缀",
			content: &tdclient.MessageDocument{Document: &tdclient.Document{
				FileName: "archive.zip",
				MimeType: "application/zip",
				Document: fileWithID(7, 5000, "zip-uniq"),
			}},
			wantType: mediapkg.Document,
			wantName: "42_archive.zip",
			wantMime: "application/zip",
			wantUniq: "zip-uniq",
			wantSize: 5000,
			wantFile: 7,
		},
		{
			name: "document 无原始文件名时回退",
			content: &tdclient.MessageDocument{Document: &tdclient.Document{
				MimeType: "application/octet-stream",
				Document: fileWithID(8, 10, "x"),
			}},
			wantType: mediapkg.Document,
			wantName: "file_42",
			wantMime: "application/octet-stream",
			wantUniq: "x",
			wantSize: 10,
			wantFile: 8,
		},
		{
			name: "voice 合成 ogg 文件名",
			content: &tdclient.MessageVoiceNote{VoiceNote: &tdclient.VoiceNote{
				MimeType: "audio/ogg",
				Voice:    fileWithID(9, 300, "v"),
			}},
			wantType: mediapkg.Voice,
			wantName: "voice_-100_42.ogg",
			wantMime: "audio/ogg",
			wantUniq: "v",
			wantSize: 300,
			wantFile: 9,
		},
		{
			name:    "无媒体的文本消息返回 nil",
			content: &tdclient.MessageText{},
			wantNil: true,
		},
		{
			name:    "document 内容为空返回 nil",
			content: &tdclient.MessageDocument{},
			wantNil: true,
		},
		{
			// 贴纸没有 FileName/MimeType，扩展名只能由 StickerFormat 决定
			name: "sticker webp 合成文件名与 MIME",
			content: &tdclient.MessageSticker{Sticker: &tdclient.Sticker{
				Format:  &tdclient.StickerFormatWebp{},
				Sticker: fileWithID(7, 3000, "stk-webp"),
			}},
			wantType: mediapkg.Sticker,
			wantName: "sticker_-100_42.webp",
			wantMime: "image/webp",
			wantUniq: "stk-webp",
			wantSize: 3000,
			wantFile: 7,
		},
		{
			name: "sticker tgs 是 Lottie 动画而非图片",
			content: &tdclient.MessageSticker{Sticker: &tdclient.Sticker{
				Format:  &tdclient.StickerFormatTgs{},
				Sticker: fileWithID(8, 4000, "stk-tgs"),
			}},
			wantType: mediapkg.Sticker,
			wantName: "sticker_-100_42.tgs",
			wantMime: "application/x-tgsticker",
			wantUniq: "stk-tgs",
			wantSize: 4000,
			wantFile: 8,
		},
		{
			name: "sticker webm 是视频贴纸",
			content: &tdclient.MessageSticker{Sticker: &tdclient.Sticker{
				Format:  &tdclient.StickerFormatWebm{},
				Sticker: fileWithID(9, 5000, "stk-webm"),
			}},
			wantType: mediapkg.Sticker,
			wantName: "sticker_-100_42.webm",
			wantMime: "video/webm",
			wantUniq: "stk-webm",
			wantSize: 5000,
			wantFile: 9,
		},
		{
			name:    "sticker 内容为空返回 nil",
			content: &tdclient.MessageSticker{},
			wantNil: true,
		},
		{
			// 圆形视频消息同样没有 FileName/MimeType，固定 mp4
			name: "video note 合成文件名并固定 mp4",
			content: &tdclient.MessageVideoNote{VideoNote: &tdclient.VideoNote{
				Video: fileWithID(11, 6000, "vn-1"),
			}},
			wantType: mediapkg.VideoNote,
			wantName: "video_note_-100_42.mp4",
			wantMime: "video/mp4",
			wantUniq: "vn-1",
			wantSize: 6000,
			wantFile: 11,
		},
		{
			name:    "video note 内容为空返回 nil",
			content: &tdclient.MessageVideoNote{},
			wantNil: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := &tdclient.Message{Id: 42, ChatId: -100, Content: tc.content}
			got := c.extractMediaFile(msg)

			if tc.wantNil {
				if got != nil {
					t.Fatalf("期望 nil，得到 %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("期望提取到媒体，得到 nil")
			}
			if got.MediaType != tc.wantType {
				t.Errorf("MediaType = %q, want %q", got.MediaType, tc.wantType)
			}
			if got.FileName != tc.wantName {
				t.Errorf("FileName = %q, want %q", got.FileName, tc.wantName)
			}
			if got.MimeType != tc.wantMime {
				t.Errorf("MimeType = %q, want %q", got.MimeType, tc.wantMime)
			}
			if got.UniqueID != tc.wantUniq {
				t.Errorf("UniqueID = %q, want %q", got.UniqueID, tc.wantUniq)
			}
			if got.FileSize != tc.wantSize {
				t.Errorf("FileSize = %d, want %d", got.FileSize, tc.wantSize)
			}
			if got.TDFileID != tc.wantFile {
				t.Errorf("TDFileID = %d, want %d", got.TDFileID, tc.wantFile)
			}
		})
	}
}

// TestExtractMediaFile_NilSafety 校验 nil 消息/内容不会 panic
func TestExtractMediaFile_NilSafety(t *testing.T) {
	c := &Client{}
	if got := c.extractMediaFile(nil); got != nil {
		t.Errorf("nil 消息应返回 nil，得到 %+v", got)
	}
	if got := c.extractMediaFile(&tdclient.Message{Id: 1}); got != nil {
		t.Errorf("nil 内容应返回 nil，得到 %+v", got)
	}
}

// TestCaptionText 校验 caption 提取；无 caption 的类型返回空串
func TestCaptionText(t *testing.T) {
	cases := []struct {
		name    string
		content tdclient.MessageContent
		want    string
	}{
		{"photo caption", &tdclient.MessagePhoto{Caption: &tdclient.FormattedText{Text: "图片说明"}}, "图片说明"},
		{"document caption", &tdclient.MessageDocument{Caption: &tdclient.FormattedText{Text: "文档"}}, "文档"},
		{"caption 为 nil", &tdclient.MessageVideo{}, ""},
		{"文本消息无 caption 字段", &tdclient.MessageText{}, ""},
		// 贴纸与圆形视频消息在 TDLib 里根本没有 Caption 字段，caption 恒为空
		{"贴纸无 caption 字段", &tdclient.MessageSticker{}, ""},
		{"视频消息无 caption 字段", &tdclient.MessageVideoNote{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := captionText(tc.content); got != tc.want {
				t.Errorf("captionText() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLargestPhotoFile 校验取面积最大的可用尺寸，并跳过缺失 Photo 的条目
func TestLargestPhotoFile(t *testing.T) {
	photo := &tdclient.Photo{Sizes: []*tdclient.PhotoSize{
		{Width: 90, Height: 90, Photo: fileWithID(1, 0, "")},
		nil,
		{Width: 200, Height: 200, Photo: nil}, // 无文件，必须跳过
		{Width: 150, Height: 150, Photo: fileWithID(3, 0, "")},
	}}
	got := largestPhotoFile(photo)
	if got == nil || got.Id != 3 {
		t.Fatalf("期望取到 id=3 的最大尺寸文件，得到 %+v", got)
	}

	if largestPhotoFile(nil) != nil {
		t.Error("nil 照片应返回 nil")
	}
	if largestPhotoFile(&tdclient.Photo{}) != nil {
		t.Error("无尺寸的照片应返回 nil")
	}
}

// TestFileSize 校验大小未知时回退到 ExpectedSize
func TestFileSize(t *testing.T) {
	if got := fileSize(&tdclient.File{Size: 500, ExpectedSize: 900}); got != 500 {
		t.Errorf("Size 已知时应取 Size，得到 %d", got)
	}
	if got := fileSize(&tdclient.File{Size: 0, ExpectedSize: 900}); got != 900 {
		t.Errorf("Size 为 0 时应回退 ExpectedSize，得到 %d", got)
	}
	if got := fileSize(nil); got != 0 {
		t.Errorf("nil 文件应返回 0，得到 %d", got)
	}
}

// TestDocName 校验文件名带消息ID前缀（避免同名文档互相覆盖）
func TestDocName(t *testing.T) {
	if got := docName("a.zip", 7); got != "7_a.zip" {
		t.Errorf("docName = %q, want %q", got, "7_a.zip")
	}
	if got := docName("", 7); got != "file_7" {
		t.Errorf("空文件名应回退，得到 %q", got)
	}
}
