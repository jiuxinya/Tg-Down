package downloader

import (
	"math"
	"strings"

	mediapkg "tg-down/internal/media"
)

// HistorySpec 描述一次历史下载任务的执行参数，由 queue 组装、telegram 客户端消费。
// 定义在本包（叶子包）以避免 telegram <-> queue 的 import 环。
type HistorySpec struct {
	ChatID int64
	TaskID string
	// ChatTitle 供路径模板的 {chat_title} 使用；由 queue 提供（它已持有标题），
	// 而不是让 telegram 层再查一次 TDLib——那样标题是否已缓存会让落盘目录变得不确定。
	ChatTitle string
	// FromMessageID 是续扫游标（最后已扫描页的最旧 message_id），0 表示从最新消息开始
	FromMessageID int64
	// MessageID 非 0 时为单消息下载任务（t.me 消息链接）：只下载该消息的媒体，不扫描历史
	MessageID int64
	// Filters 是任务级媒体过滤条件（零值 = 不过滤）
	Filters HistoryFilters
	// RetryMessageIDs 是恢复任务时需优先补下的消息（进程重启清扫的中断行，
	// 比游标更新，仅靠游标续扫会永久漏掉）
	RetryMessageIDs []int64
	// RetryOnly 为真时只补下 RetryMessageIDs，跳过历史扫描。
	// 用于"任务已完整扫完、只是部分文件失败"的重试：重扫整条历史除了浪费时间不会有任何发现。
	RetryOnly bool
	// StopAtMessageID 是增量扫描的下界（不含）：扫描自最新消息向旧推进，遇到 id <= 此值
	// 即停止。定时任务据此只扫上次之后的新消息，而不是每次都重扫整条历史。
	StopAtMessageID int64
	// ScheduleID 非空表示本任务由定时计划触发，完成后需回写该计划的增量水位
	ScheduleID string
}

// HistoryResult 汇总单次历史下载运行的结果。
//
// 必须是"单次运行"而非任务累计：任务级统计跨重试累加，用它判断成败会让重试成功的任务
// 依旧因为上一轮的失败计数而被判为部分失败。
type HistoryResult struct {
	// Failed 是本次运行中下载失败的媒体数。
	Failed int64
	// MaxMessageID 是本次扫描见过的最新消息 id，作为定时任务下次增量扫描的水位。
	MaxMessageID int64
}

// HistoryFilters 是任务级媒体过滤条件；JSON 序列化后持久化在 tasks.filters 列，
// 并作为 POST /api/tasks 的 filters 字段
type HistoryFilters struct {
	// MediaTypes 是要下载的媒体类型子集（见 media.AllTypes），空 = media.DefaultTypes
	MediaTypes []string `json:"media_types,omitempty"`
	// DateFrom/DateTo 是消息日期区间（unix 秒，闭区间），0 = 不限
	DateFrom int64 `json:"date_from,omitempty"`
	DateTo   int64 `json:"date_to,omitempty"`
	// MaxFileSize 是单文件大小上限（字节），0 = 不限
	MaxFileSize int64 `json:"max_file_size,omitempty"`
	// Query 是关键词过滤：命中 caption 或文件名即通过，空 = 不限。
	// 走服务端枚举时直接交给 SearchChatMessages 在服务端过滤（不必把不匹配的消息拉回本地）；
	// Match 仍会在客户端复核一遍，因为回退的全量翻页路径没有服务端过滤。
	Query string `json:"query,omitempty"`
	// SenderID 是发送者过滤（user/chat id），0 = 不限。同样兼有服务端与客户端两条通路。
	SenderID int64 `json:"sender_id,omitempty"`
}

// IsZero 报告过滤器是否为零值（不过滤）
func (f HistoryFilters) IsZero() bool {
	return len(f.MediaTypes) == 0 && f.DateFrom == 0 && f.DateTo == 0 &&
		f.MaxFileSize == 0 && f.Query == "" && f.SenderID == 0
}

// Match 报告一个媒体项是否通过过滤（客户端复核）。
//
// 服务端枚举已经按 Query/SenderID/MediaType 过滤过一轮，但回退的全量翻页路径没有，
// 因此判定统一收敛在这里：两条路径共用同一份语义，不会出现"换条路走结果就不一样"。
func (f HistoryFilters) Match(mi *MediaInfo) bool {
	if mi == nil {
		return false
	}
	if len(f.MediaTypes) > 0 && !containsString(f.MediaTypes, mi.MediaType) {
		return false
	}
	date := mi.Date.Unix()
	if f.DateFrom != 0 && date < f.DateFrom {
		return false
	}
	if f.DateTo != 0 && date > f.DateTo {
		return false
	}
	if f.MaxFileSize > 0 && mi.FileSize > f.MaxFileSize {
		return false
	}
	if f.SenderID != 0 && mi.SenderID != f.SenderID {
		return false
	}
	if f.Query != "" && !matchesQuery(mi, f.Query) {
		return false
	}
	return true
}

// matchesQuery 大小写不敏感地在 caption 与文件名中查找关键词
func matchesQuery(mi *MediaInfo, query string) bool {
	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(mi.Caption), q) ||
		strings.Contains(strings.ToLower(mi.FileName), q)
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Validate 校验过滤器字段合法性，返回首个问题的描述（合法时为空串）
func (f HistoryFilters) Validate() string {
	for _, t := range f.MediaTypes {
		if !mediapkg.IsValid(t) {
			return "无效的媒体类型: " + t
		}
	}
	if f.DateFrom < 0 {
		return "date_from 不能为负"
	}
	if f.DateFrom > math.MaxInt32 {
		return "date_from 超出 TDLib 支持的 Unix 秒范围"
	}
	if f.DateTo < 0 {
		return "date_to 不能为负"
	}
	if f.DateTo > math.MaxInt32 {
		return "date_to 超出 TDLib 支持的 Unix 秒范围"
	}
	if f.DateFrom != 0 && f.DateTo != 0 && f.DateFrom > f.DateTo {
		return "date_from 不能晚于 date_to"
	}
	if f.MaxFileSize < 0 {
		return "max_file_size 不能为负"
	}
	return ""
}
