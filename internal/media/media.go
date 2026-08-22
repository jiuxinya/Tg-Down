// Package media 是媒体类型的唯一词汇源。
//
// 此前 telegram 与 downloader 各自定义了一套媒体类型常量，靠字面量巧合对齐；新增一种类型
// 需要同时改动分类目录、合法值校验、服务端计数过滤器、扩展名映射等多处，漏改一处即出错
// （漏改服务端过滤器 → 进度条分母缺失；漏改合法值校验 → 建任务直接 400）。
//
// 本包只放纯 Go 的词汇与规则，不依赖 TDLib 绑定——否则 downloader/queue/store 都会被 CGo
// 传染，无法在不构建 TDLib 的情况下跑单测。TDLib 的 SearchMessagesFilter 映射留在 telegram
// 包，由 TestServerFilterCoverage 强制其与本包的类型集保持同步。
package media

// 媒体类型。取值同时用于：任务过滤器（HistoryFilters.MediaTypes）、分类存储目录、
// 下载历史 media_type 列、Web 端类型筛选。
const (
	Photo     = "photo"
	Video     = "video"
	Document  = "document"
	Animation = "animation"
	Audio     = "audio"
	Voice     = "voice"
	Sticker   = "sticker"
	VideoNote = "video_note"

	// Other 是分类存储的兜底目录名，不是一种可下载类型（不出现在 AllTypes 中）。
	Other = "other"
)

// AllTypes 是全部可下载的媒体类型（用于合法性校验与前端类型列表）。
// 新增类型时只需在此登记并补齐下方的规则表。
var AllTypes = []string{Photo, Video, Document, Animation, Audio, Voice, Sticker, VideoNote}

// DefaultTypes 是未指定媒体类型时实际下载的类型集。空过滤器语义必须是真正的
// “全部类型”，因此这里包含贴纸；调用方若只想下载子集，必须显式传入该子集。
var DefaultTypes = append([]string(nil), AllTypes...)

// validTypes 是 AllTypes 的集合形式，供 IsValid 做 O(1) 判定。
var validTypes = func() map[string]bool {
	m := make(map[string]bool, len(AllTypes))
	for _, t := range AllTypes {
		m[t] = true
	}
	return m
}()

// serverFilterable 标记该类型能否用 TDLib 的 SearchMessagesFilter 在服务端枚举与计数。
//
// 贴纸是唯一的例外：TDLib 压根没有 SearchMessagesFilterSticker。它既不能服务端枚举
// （只能全量遍历历史），也拿不到服务端总数——进度条对它没有分母，前端须按未知总数渲染，
// 否则会出现"已下载 30 / 共 0"这种超过 100% 的进度。
var serverFilterable = map[string]bool{
	Photo:     true,
	Video:     true,
	Document:  true,
	Animation: true,
	Audio:     true,
	Voice:     true,
	VideoNote: true,
	Sticker:   false,
}

// mimeExtensions 是 MIME 到文件扩展名的映射，用于原始文件名缺失时合成文件名。
var mimeExtensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"video/mp4":       ".mp4",
	"video/avi":       ".avi",
	"video/mov":       ".mov",
	"video/webm":      ".webm",
	"audio/mp3":       ".mp3",
	"audio/ogg":       ".ogg",
	"application/pdf": ".pdf",
}

// IsValid 报告 t 是否为合法的可下载媒体类型。
func IsValid(t string) bool {
	return validTypes[t]
}

// HasServerFilter 报告该类型能否经 TDLib 服务端过滤器枚举/计数。
func HasServerFilter(t string) bool {
	return serverFilterable[t]
}

// ClassifyDir 返回该类型的分类存储子目录名；未知类型归入 Other。
func ClassifyDir(t string) string {
	if IsValid(t) {
		return t
	}
	return Other
}

// ExtensionFor 按 MIME 返回文件扩展名，未知 MIME 返回空串。
func ExtensionFor(mimeType string) string {
	return mimeExtensions[mimeType]
}
