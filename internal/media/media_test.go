package media

import "testing"

// TestClassifyDir 校验媒体类型到分类子目录的映射（未知类型归入 other）
func TestClassifyDir(t *testing.T) {
	cases := map[string]string{
		Photo:     "photo",
		Document:  "document",
		Video:     "video",
		Animation: "animation",
		Audio:     "audio",
		Voice:     "voice",
		Sticker:   "sticker",
		VideoNote: "video_note",
		"webp":    Other,
		"":        Other,
	}
	for mediaType, want := range cases {
		if got := ClassifyDir(mediaType); got != want {
			t.Errorf("ClassifyDir(%q) = %q, want %q", mediaType, got, want)
		}
	}
}

// TestIsValid 校验合法类型判定：AllTypes 全部合法，其余一律非法
func TestIsValid(t *testing.T) {
	for _, mt := range AllTypes {
		if !IsValid(mt) {
			t.Errorf("IsValid(%q) = false, want true", mt)
		}
	}
	for _, mt := range []string{Other, "webp", "videonote", "", "PHOTO"} {
		if IsValid(mt) {
			t.Errorf("IsValid(%q) = true, want false", mt)
		}
	}
}

// TestDefaultTypes 锁定"下载全部"的语义：默认集必须和全部可下载类型完全一致，包含贴纸。
func TestDefaultTypes(t *testing.T) {
	if len(DefaultTypes) != len(AllTypes) {
		t.Fatalf("DefaultTypes = %v, want %v", DefaultTypes, AllTypes)
	}
	for i := range AllTypes {
		if DefaultTypes[i] != AllTypes[i] {
			t.Errorf("DefaultTypes[%d] = %q, want %q", i, DefaultTypes[i], AllTypes[i])
		}
	}
	if !contains(DefaultTypes, Sticker) {
		t.Error("默认下载类型必须包含贴纸")
	}
	if HasServerFilter(Sticker) {
		t.Error("贴纸不应声明为可服务端枚举：TDLib 没有专用搜索过滤器")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestAllTypesHaveRules 确保 AllTypes 中每一种类型都在规则表里有登记。
// 新增类型时若只加进 AllTypes 而漏配规则，此处会失败。
func TestAllTypesHaveRules(t *testing.T) {
	for _, mt := range AllTypes {
		if _, ok := serverFilterable[mt]; !ok {
			t.Errorf("类型 %q 在 AllTypes 中，但未在 serverFilterable 中登记", mt)
		}
		if ClassifyDir(mt) == Other {
			t.Errorf("类型 %q 的分类目录落到了 other，说明未被 IsValid 识别", mt)
		}
	}
}

// TestExtensionFor 校验 MIME 到扩展名的映射
func TestExtensionFor(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":               ".jpg",
		"video/mp4":                ".mp4",
		"application/pdf":          ".pdf",
		"application/x-executable": "", // 未知 MIME 返回空串
		"":                         "",
	}
	for mime, want := range cases {
		if got := ExtensionFor(mime); got != want {
			t.Errorf("ExtensionFor(%q) = %q, want %q", mime, got, want)
		}
	}
}
