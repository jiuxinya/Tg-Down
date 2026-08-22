package telegram

import (
	"context"

	tdclient "github.com/zelenin/go-tdlib/client"
)

// tdAPI 是 Client 依赖的 TDLib 方法集合。
//
// 引入它的唯一目的是让 internal/telegram 可测：此前 Client.td 是具体类型 *tdclient.Client
// 且在 Connect 内部直接构造，导致本包（全仓最大）的任何一行业务逻辑都无法在没有真实
// TDLib 连接的情况下被触达。*tdclient.Client 结构上满足本接口，生产路径零变化。
//
// 认证相关方法（SetTdlibParameters / CheckAuthenticationCode 等）不在此列：它们由 TDLib
// 通过 AuthorizationStateHandler 回调把具体的 *tdclient.Client 传进来（签名由库固定），
// 无法接口化——认证状态机本就属于必须真账号验证的范围。
type tdAPI interface {
	// 聊天枚举
	LoadChats(ctx context.Context, req *tdclient.LoadChatsRequest) (*tdclient.Ok, error)
	GetChats(ctx context.Context, req *tdclient.GetChatsRequest) (*tdclient.Chats, error)
	GetChat(ctx context.Context, req *tdclient.GetChatRequest) (*tdclient.Chat, error)
	SearchPublicChat(ctx context.Context, req *tdclient.SearchPublicChatRequest) (*tdclient.Chat, error)
	CreatePrivateChat(ctx context.Context, req *tdclient.CreatePrivateChatRequest) (*tdclient.Chat, error)
	OpenChat(ctx context.Context, req *tdclient.OpenChatRequest) (*tdclient.Ok, error)
	CloseChat(ctx context.Context, req *tdclient.CloseChatRequest) (*tdclient.Ok, error)

	// 历史扫描与消息
	GetChatHistory(ctx context.Context, req *tdclient.GetChatHistoryRequest) (*tdclient.Messages, error)
	SearchChatMessages(ctx context.Context, req *tdclient.SearchChatMessagesRequest) (*tdclient.FoundChatMessages, error)
	GetChatMessageCount(ctx context.Context, req *tdclient.GetChatMessageCountRequest) (*tdclient.Count, error)
	GetChatMessageByDate(ctx context.Context, req *tdclient.GetChatMessageByDateRequest) (*tdclient.Message, error)
	GetMessage(ctx context.Context, req *tdclient.GetMessageRequest) (*tdclient.Message, error)
	GetMessageLinkInfo(ctx context.Context, req *tdclient.GetMessageLinkInfoRequest) (*tdclient.MessageLinkInfo, error)
	SendMessage(ctx context.Context, req *tdclient.SendMessageRequest) (*tdclient.Message, error)

	// 文件下载
	DownloadFile(ctx context.Context, req *tdclient.DownloadFileRequest) (*tdclient.File, error)
	CancelDownloadFile(ctx context.Context, req *tdclient.CancelDownloadFileRequest) (*tdclient.Ok, error)

	// 会话生命周期
	GetMe(ctx context.Context) (*tdclient.User, error)
	GetAuthorizationState(ctx context.Context) (tdclient.AuthorizationState, error)
	GetOption(req *tdclient.GetOptionRequest) (tdclient.OptionValue, error)
	LogOut(ctx context.Context) (*tdclient.Ok, error)
	Close(ctx context.Context) (*tdclient.Ok, error)
}

// 编译期断言：真实的 TDLib 客户端满足 tdAPI。
var _ tdAPI = (*tdclient.Client)(nil)
