// Package tgapi 存放 Telegram 客户端的跨包契约：领域数据类型 + Web 层依赖的最小客户端接口。
//
// 为什么单独一个包：internal/telegram 经 go-tdlib 引入 CGo，任何 import 它的包（包括其测试）
// 都必须先编出整个 TDLib——连 store/config 这种纯 Go 包的测试都会被连累。把 web 需要的类型与接口
// 提到这个无 CGo 的叶子包后，`go test ./internal/...`（除 telegram 外）不再需要 TDLib。
//
// internal/telegram 用类型别名把这些类型再导出（`type ChatInfo = tgapi.ChatInfo`），
// 所以 telegram.ChatInfo 等既有引用保持可用。
package tgapi

import (
	"context"

	"tg-down/internal/downloader"
	"tg-down/internal/queue"
)

// CodeFunc 提供登录验证码
type CodeFunc func(ctx context.Context) (string, error)

// PasswordFunc 提供两步验证密码
type PasswordFunc func(ctx context.Context) (string, error)

// ChatInfo 聊天信息
type ChatInfo struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

// ResolvedTarget 是一次目标解析（@用户名 / t.me 链接）的结果
type ResolvedTarget struct {
	ChatID    int64  `json:"chat_id"`
	Title     string `json:"chat_title"`
	MessageID int64  `json:"message_id,omitempty"`
}

// ExportSpec 描述一次聊天导出
type ExportSpec struct {
	ChatID    int64
	ChatTitle string
	// Limit 是最多导出的消息条数（0 = 不限）。百万级频道的全量导出耗时极长，
	// 由调用方决定是否设限，而不是在这里悄悄截断。
	Limit int
}

// ExportResult 是导出产物的落盘位置与规模
type ExportResult struct {
	JSONPath     string `json:"json_path"`
	HTMLPath     string `json:"html_path"`
	Messages     int    `json:"messages"`
	MediaCount   int    `json:"media_count"`
	MediaOnDisk  int    `json:"media_on_disk"`
	Truncated    bool   `json:"truncated"`
	ChatTitle    string `json:"chat_title"`
	ExportedAtTS int64  `json:"exported_at"`
}

// Client 是 Web 层用到的 Telegram 客户端能力集合。*telegram.Client 实现它
// （telegram 包里有编译期断言钉住这一点）。
type Client interface {
	// 任务队列驱动的下载能力（queue.Manager 也只认这一组方法）
	queue.ChatDownloader

	// SendSelfMessage 向自己的 Saved Messages 发消息（任务完成通知）
	SendSelfMessage(ctx context.Context, text string) error

	// 认证与会话
	HasCredentials() bool
	SetCredentials(apiID int, apiHash, phone string)
	SaveConfig() error
	AuthenticateWith(ctx context.Context, codeFn CodeFunc, passwordFn PasswordFunc) error
	ClearSession() error
	ClearPhone() error
	Logout(ctx context.Context) error
	Close()
	Phone() string

	// 聊天
	GetChats(ctx context.Context) ([]ChatInfo, error)
	TargetChat() int64
	ResolveTarget(ctx context.Context, input string) (ResolvedTarget, error)
	ExportChat(ctx context.Context, spec ExportSpec) (*ExportResult, error)

	// 下载运行时
	Stats() downloader.Stats
	ActiveMedia() []downloader.MediaProgress
	DownloadSpeed() int64
	ConnectionState() string
	DownloadPath() string
	ClassifyByType() bool
	SetClassifyByType(on bool) error
	DownloadConcurrency() int
	SetDownloadConcurrency(n int) error
	ActiveDownloadCount() int
	AllMediaPaused() bool
	PauseMedia(ctx context.Context, id string) error
	ResumeMedia(id string) error
	PauseAllMedia(ctx context.Context)
	ResumeAllMedia()
}
