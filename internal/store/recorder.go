package store

import (
	"context"
	"fmt"
	"log"

	"tg-down/internal/downloader"
)

// NewRecorder 返回一个与 downloader.RecordFunc 兼容的回调，将每个下载事件
// 持久化为一条历史记录；CLI 模式直接使用，internal/queue.Manager 会在外层
// 包装它以额外更新内存中的任务统计。
//
// 权衡：本包不依赖 logger（保持持久层无外部依赖），且下载流程不能因历史记录
// 写入失败而失败，因此错误交给可选回调报告；未传回调时写入标准日志，不 panic、不重试。
//
// 终态记录一律使用 context.Background() 落盘：调用方传入的 ctx 在取消/关停时可能已失效，
// 若沿用会使被取消下载的 RecordFailed 写入被 database/sql 直接中止，导致 history 行永久停在
// "downloading"。终态持久化必须不受请求 ctx 取消影响。
func NewRecorder(s *Store, errorHandlers ...func(error)) func(context.Context, *downloader.RecordEvent) {
	reportError := func(err error) {
		log.Printf("下载历史持久化失败: %v", err)
	}
	if len(errorHandlers) > 0 && errorHandlers[0] != nil {
		reportError = errorHandlers[0]
	}
	return func(_ context.Context, evt *downloader.RecordEvent) {
		if evt == nil || evt.Media == nil {
			return
		}
		ctx := context.Background()
		recordError := func(err error) {
			if err != nil {
				reportError(fmt.Errorf("记录 %s 事件失败: %w", evt.Status, err))
			}
		}

		// 每个媒体恰好写两次：入队时插入 queued 行，终态时更新为 completed/failed/skipped。
		// 缩略图路径并进终态那条 UPDATE——它与终态同时可知，单独再写一次等于白付一次事务提交。
		switch evt.Status {
		case downloader.RecordQueued:
			recordError(s.UpsertHistoryStart(ctx, &HistoryRecord{
				TaskID:    evt.Media.TaskID,
				ChatID:    evt.Media.ChatID,
				ChatTitle: evt.Media.ChatTitle,
				MessageID: evt.Media.MessageID,
				MediaType: evt.Media.MediaType,
				FileName:  evt.Media.FileName,
				FilePath:  evt.FilePath,
				FileSize:  evt.Media.FileSize,
				MimeType:  evt.Media.MimeType,
				Status:    HistoryStatusQueued,
				UniqueID:  evt.Media.UniqueID,
				AlbumID:   evt.Media.AlbumID,
				Minithumb: evt.Media.Minithumb,
			}))
		case downloader.RecordCompleted:
			recordError(s.UpdateHistoryResultWithThumb(ctx, evt.Media.ChatID, evt.Media.MessageID,
				HistoryStatusCompleted, "", evt.FilePath, evt.ThumbPath))
		case downloader.RecordFailed:
			recordError(s.UpdateHistoryResult(ctx, evt.Media.ChatID, evt.Media.MessageID, HistoryStatusFailed, evt.Reason, evt.FilePath))
		case downloader.RecordSkipped:
			recordError(s.UpdateHistoryResult(ctx, evt.Media.ChatID, evt.Media.MessageID, HistoryStatusSkipped, evt.Reason, evt.FilePath))
		}
	}
}
