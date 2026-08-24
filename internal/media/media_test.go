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

// TestDefaultTypes 锁定默认类型集的两条性质：不含贴纸，且其余类型全部可服务端枚举。
//
// 后者是本组断言的要害：只要默认集里混进一个无服务端过滤器的类型，未指定类型的任务
// 就会整体退回全量翻页并丢失进度分母。用 HasServerFilter 逐项校验而非硬编码类型名单，
// 将来新增不可枚举的类型时这条会直接失败，而不是等到线上发现扫描变慢。
func TestDefaultTypes(t *testing.T) {
	if contains(DefaultTypes, Sticker) {
		t.Error("默认下载类型不应包含贴纸：它会使整个任务退化为全量翻页且总数未知")
	}
	if len(DefaultTypes) != len(AllTypes)-1 {
		t.Errorf("DefaultTypes 应为 AllTypes 去掉贴纸，得到 %v", DefaultTypes)
	}
	for _, mt := range DefaultTypes {
		if !IsValid(mt) {
			t.Errorf("DefaultTypes 含非法类型 %q", mt)
		}
		if !HasServerFilter(mt) {
			t.Errorf("默认类型 %q 不可服务端枚举，会拖垮未指定类型的任务", mt)
		}
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
