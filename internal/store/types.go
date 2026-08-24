package store

import "time"

// TaskRow 表示 tasks 表中的一行下载任务记录
type TaskRow struct {
	ID              string
	Kind            string
	ChatID          int64
	ChatTitle       string
	Status          string
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
	Error           string
	Total           int
	Downloaded      int
	Failed          int
	Skipped         int
	TotalSize       int64
	DownloadedSize  int64
	ExpectedTotal   int64
	ScanCursor      int64  // 历史扫描游标（最后已扫描页的最旧 message_id），0 = 从最新开始
	Attempts        int    // 自动重试已消耗的次数
	Filters         string // 任务级过滤器 JSON（downloader.HistoryFilters），空 = 不过滤
	MessageID       int64  // 单消息下载任务的目标消息 id，0 = 整聊天历史任务
	StopAtMessageID int64  // 定时增量扫描下界，0 = 不限制
	ScheduleID      string // 触发该任务的定时计划 id，空 = 非定时任务
	RetryFailedOnly bool   // 本轮只补下失败文件，不再扫描历史
}

// 任务状态常量，取值与 internal/queue 的 Status 保持一致（queue 为唯一词汇源）
const (
	TaskStatusQueued    = "queued"
	TaskStatusRunning   = "running"
	TaskStatusCompleted = "completed"
	TaskStatusPartial   = "partial"
	TaskStatusFailed    = "failed"
	TaskStatusCanceled  = "canceled"
)

// HistoryRecord 表示 history 表中的一行下载历史记录
type HistoryRecord struct {
	ID         int64
	TaskID     string
	ChatID     int64
	ChatTitle  string
	MessageID  int64
	MediaType  string
	FileName   string
	FilePath   string
	FileSize   int64
	MimeType   string
	Status     string
	Reason     string
	CreatedAt  time.Time
	FinishedAt *time.Time
	UniqueID   string // TDLib remote file unique_id，跨聊天稳定，用于内容级去重
	AlbumID    int64  // Telegram 相册 id（media_album_id），0 = 不属于相册
	ThumbPath  string // 缩略图缓存文件路径（.thumbs/<unique_id>.jpg），无缩略图为空
	Minithumb  []byte // TDLib 随消息免费返回的极小 JPEG（约 40x40），画廊的即时占位图
	// HasMinithumb 标记该行是否存在 minithumb。列表查询不取 BLOB 本体：
	// 它每页要搬运数十 KB，而列表接口只用它算一个"有无缩略图"的布尔。
	HasMinithumb bool
}

// 下载历史状态常量，取值与 downloader.RecordStatus 保持一致
const (
	// HistoryStatusQueued 是媒体入队后的初始状态，终态到来前一直停留在此。
	HistoryStatusQueued = "queued"
	// HistoryStatusDownloading 是 v2.x 写入的在途状态，现已不再产生，
	// 仅为清扫旧库中的遗留行而保留。
	HistoryStatusDownloading = "downloading"
	HistoryStatusCompleted   = "completed"
	HistoryStatusFailed      = "failed"
	HistoryStatusSkipped     = "skipped"
)

// HistoryReasonInterrupted 是进程重启清扫写入的中断原因；恢复任务据此定位需补下的行，
// UpsertHistoryStart 对该原因的 failed 行放行重新激活
const HistoryReasonInterrupted = "interrupted"

// HistoryFilter 描述 QueryHistory/HistoryStats 的过滤与分页条件
type HistoryFilter struct {
	MediaType string
	Status    string
	Query     string // 按文件名全文匹配（FTS5）
	ChatID    int64  // 0 表示不限制
	TaskID    string // 空 = 不限制；用于任务详情下钻
	From, To  *time.Time

	// Cursor 是上一页最后一行的位置，nil = 取第一页。
	// 用游标而非 LIMIT/OFFSET：深翻页时 OFFSET 要先扫掉前面所有行，代价随页码线性增长；
	// 且 created_at 是秒级精度，同秒入库的行在无次键排序下顺序不定，翻页可能重复或漏行。
	Cursor *HistoryCursor
	Limit  int
	Sort   HistorySort
	// WithTotal 为真时才计算匹配总数。COUNT(*) 与列表查询同样要走一遍过滤，
	// 而翻页时总数不会变，因此只在筛选条件变化的那一次请求里要。
	WithTotal bool
}

// HistoryCursor 是 keyset 分页的位置，(排序键, id) 两级保证游标唯一。
// id 是必需的次键：仅凭秒级的 created_at 无法在同秒的行之间定位。
type HistoryCursor struct {
	SortValue int64
	ID        int64
}

// 排序键对应的列名。取值只在此处白名单内产生，不接受外部输入拼接。
const (
	sortColCreatedAt = "created_at"
	sortColFileSize  = "file_size"
)

// HistorySort 是历史列表的排序方式
type HistorySort string

const (
	// HistorySortCreatedDesc 是默认排序：最新下载的在前
	HistorySortCreatedDesc HistorySort = "created_desc"
	// HistorySortCreatedAsc 按下载时间升序
	HistorySortCreatedAsc HistorySort = "created_asc"
	// HistorySortSizeDesc 按文件大小降序
	HistorySortSizeDesc HistorySort = "size_desc"
	// HistorySortSizeAsc 按文件大小升序
	HistorySortSizeAsc HistorySort = "size_asc"
)

// sortColumn 返回该排序方式对应的列与方向；未知取值回落到默认排序。
// 列名不来自用户输入，只在此处的白名单中取值。
func (s HistorySort) sortColumn() (column string, descending bool) {
	switch s {
	case HistorySortCreatedAsc:
		return sortColCreatedAt, false
	case HistorySortSizeDesc:
		return sortColFileSize, true
	case HistorySortSizeAsc:
		return sortColFileSize, false
	case HistorySortCreatedDesc:
		return sortColCreatedAt, true
	default:
		return sortColCreatedAt, true
	}
}

// IsValid 报告是否为受支持的排序方式
func (s HistorySort) IsValid() bool {
	switch s {
	case HistorySortCreatedDesc, HistorySortCreatedAsc, HistorySortSizeDesc, HistorySortSizeAsc:
		return true
	default:
		return false
	}
}

// MediaTypeStat 按媒体类型聚合的下载统计
type MediaTypeStat struct {
	MediaType string
	Count     int
	TotalSize int64
	Completed int
	Failed    int
	Skipped   int
}
