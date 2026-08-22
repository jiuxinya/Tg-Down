package telegram

import (
	"context"
	"errors"
	"fmt"
	"time"

	tdclient "github.com/zelenin/go-tdlib/client"

	"tg-down/internal/downloader"
	mediapkg "tg-down/internal/media"
)

// scanOutcome 是一次历史扫描的结果（多条过滤器流水线时为累计值）
type scanOutcome struct {
	scannedMessages int64
	foundMedia      int64
	maxMessageID    int64 // 本次见过的最新消息 id，作为定时任务的增量水位
}

// effectiveTypes 返回任务实际要扫描的媒体类型：未指定类型（下载全部）等价于 media.DefaultTypes。
// 把"全部"展开成显式类型列表，是"下载全部"也能走服务端枚举的前提。
func effectiveTypes(types []string) []string {
	if len(types) == 0 {
		return mediapkg.DefaultTypes
	}
	return types
}

// searchFiltersFor 把媒体类型列表映射为一组 TDLib 服务端过滤器，每种类型一条。
//
// SearchChatMessages 一次只接受一个过滤器，因此多类型任务改为每种类型各跑一条流水线。
// 只要有任一类型没有专用服务端过滤器（如贴纸），调用方就改用一条
// SearchMessagesFilterEmpty 流水线枚举完整历史，并在本地筛选。
func searchFiltersFor(types []string) ([]tdclient.SearchMessagesFilter, bool) {
	filters := make([]tdclient.SearchMessagesFilter, 0, len(types))
	for _, t := range types {
		if !mediapkg.HasServerFilter(t) {
			return nil, false
		}
		f, ok := historyCountFilters[t]
		if !ok {
			return nil, false
		}
		filters = append(filters, f)
	}
	return filters, len(filters) > 0
}

// searchChatMessages 用服务端过滤器拉取一页匹配的消息，经 retrier 重试。
//
// 关键词与发送者也交给服务端过滤：不匹配的消息根本不会被传回本地。
func (c *Client) searchChatMessages(
	ctx context.Context, td tdAPI, spec *downloader.HistorySpec,
	filter tdclient.SearchMessagesFilter, fromMsgID int64, limit int32,
) (*tdclient.FoundChatMessages, error) {
	req := &tdclient.SearchChatMessagesRequest{
		ChatId:        spec.ChatID,
		FromMessageId: fromMsgID,
		Offset:        0,
		Limit:         limit,
		Filter:        filter,
		Query:         spec.Filters.Query,
	}
	if spec.Filters.SenderID != 0 {
		req.SenderId = senderFromID(spec.Filters.SenderID)
	}
	return c.searchChatMessagesRequest(ctx, td, req)
}

// searchChatMessagesRequest 执行一页搜索并统一应用 TDLib 错误分类与重试。
// SearchMessagesFilterEmpty 同样走这里，供贴纸/全媒体扫描与聊天导出使用。
func (c *Client) searchChatMessagesRequest(
	ctx context.Context, td tdAPI, req *tdclient.SearchChatMessagesRequest,
) (*tdclient.FoundChatMessages, error) {
	var found *tdclient.FoundChatMessages
	err := c.retrier.Do(ctx, func() error {
		var err error
		found, err = tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.FoundChatMessages, error) {
			return td.SearchChatMessages(cc, req)
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("搜索聊天消息失败: %w", err)
	}
	if found == nil {
		return nil, errors.New("搜索聊天消息失败: TDLib 返回空响应")
	}
	return found, nil
}

// senderFromID 把发送者 id 还原为 TDLib 的 MessageSender。
// Telegram 的聊天 id 为负、用户 id 为正，据此区分二者。
func senderFromID(id int64) tdclient.MessageSender {
	if id < 0 {
		return &tdclient.MessageSenderChat{ChatId: id}
	}
	return &tdclient.MessageSenderUser{UserId: id}
}

// scanBySearch 用一个服务端过滤器枚举媒体消息，把发现的媒体投入下载流水线。
//
// base 是此前各条流水线的累计值，用于让进度上报呈现全局总数而非本流水线的局部计数；
// 返回值是含本流水线在内的新累计值。trackCursor 见 scanHistoryPages 的说明。
//
// 相比全量翻页的两个决定性优势：
//   - 服务端只返回匹配的媒体消息。百万条消息的频道里只勾 video 时，此前要把全部纯文本消息
//     逐条拉回本地再丢弃；
//   - 结束条件由 NextFromMessageId == 0 明确给出，不再依赖"连续 8 个空页"的启发式——
//     那套逻辑的容忍窗口只有约 25 秒，冷缓存时会把"服务端还没回填完"误判成"扫完了"，
//     任务就此标记成功，漏下的文件无人知晓。
func (c *Client) scanBySearch(
	ctx context.Context, td tdAPI, spec *downloader.HistorySpec,
	filter tdclient.SearchMessagesFilter, fromMsgID int64, limit int32,
	dispatch func(*downloader.MediaInfo) error, base scanOutcome, trackCursor bool,
) (scanOutcome, error) {
	res := base
	lastScanLog := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		found, err := c.searchChatMessages(ctx, td, spec, filter, fromMsgID, limit)
		if err != nil {
			return res, err
		}

		msgs, reachedStop := trimAtStop(found.Messages, spec.StopAtMessageID)
		res.maxMessageID = maxMessageID(msgs, res.maxMessageID)

		media, pastDateFrom := c.extractBatchMedia(msgs, spec)
		res.scannedMessages += int64(len(msgs))
		res.foundMedia += int64(len(media))

		next := found.NextFromMessageId
		cursor := int64(0)
		if trackCursor {
			cursor = next
		}
		c.reportScanProgress(spec.TaskID, res.scannedMessages, res.foundMedia, cursor)

		c.downloader.PlanBatch(media)
		for _, m := range media {
			if err := dispatch(m); err != nil {
				return res, err
			}
		}

		switch {
		case reachedStop:
			c.logger.Info("增量扫描已追上上次水位，本条流水线提前结束")
			return res, nil
		case pastDateFrom:
			c.logger.Info("历史扫描已越过起始日期，本条流水线提前结束")
			return res, nil
		case next == 0:
			return res, nil // 服务端明确告知没有更多结果
		case next == fromMsgID:
			return res, fmt.Errorf("搜索聊天消息游标未推进: %d", next)
		}
		fromMsgID = next

		if time.Since(lastScanLog) >= scanLogInterval {
			c.logger.Info("扫描进度: 已匹配 %d 条消息，发现 %d 个媒体", res.scannedMessages, res.foundMedia)
			lastScanLog = time.Now()
		}
	}
}

// trimAtStop 在增量扫描中截断到水位线：消息按新到旧返回，遇到 id <= stopAt 即认为已追上
// 上次扫描的位置，其后的消息上次都已处理过。reached=true 表示本次增量扫描可以收尾。
func trimAtStop(msgs []*tdclient.Message, stopAt int64) (kept []*tdclient.Message, reached bool) {
	if stopAt <= 0 {
		return msgs, false
	}
	for i, m := range msgs {
		if m.Id <= stopAt {
			return msgs[:i], true
		}
	}
	return msgs, false
}

// maxMessageID 返回一页消息中最大的 id（消息按新到旧排列，故为首条）
func maxMessageID(msgs []*tdclient.Message, current int64) int64 {
	for _, m := range msgs {
		if m.Id > current {
			current = m.Id
		}
	}
	return current
}

// scanStartID 决定扫描的起点消息 id。
//
// 优先级：断点续扫的游标 > 按 DateTo 定位的日期锚点 > 0（从最新消息开始）。
// multi=true（多条流水线）时忽略游标：一个整数游标无法表达 N 条流水线各自的位置，
// 拿它作所有流水线的起点会让每条都漏掉比游标更新的消息。
func (c *Client) scanStartID(ctx context.Context, td tdAPI, spec *downloader.HistorySpec, multi bool) int64 {
	if spec.FromMessageID != 0 && !multi {
		return spec.FromMessageID
	}
	if spec.Filters.DateTo == 0 {
		return 0
	}
	return c.dateAnchor(ctx, td, spec)
}

// dateAnchor 取 DateTo 附近的消息 id 作为扫描起点，失败时返回 0（从最新消息开始）。
//
// 历史按新到旧扫描，此前"只下载 2020 年的内容"必须把 2020 年之后的所有消息全翻一遍再逐条
// 丢弃。GetChatMessageByDate 让扫描直接从目标日期落位。
func (c *Client) dateAnchor(ctx context.Context, td tdAPI, spec *downloader.HistorySpec) int64 {
	msg, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Message, error) {
		return td.GetChatMessageByDate(cc, &tdclient.GetChatMessageByDateRequest{
			ChatId: spec.ChatID,
			Date:   int32(spec.Filters.DateTo), //nolint:gosec // unix 秒，2038 年前不会溢出
		})
	})
	if err != nil || msg == nil {
		c.logger.Debug("按日期定位扫描起点失败，从最新消息开始: %v", err)
		return 0
	}
	c.logger.Info("按截止日期定位扫描起点：消息 %d", msg.Id)
	return msg.Id
}
