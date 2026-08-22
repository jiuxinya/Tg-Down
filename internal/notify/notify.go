// Package notify 在任务终结（完成/最终失败）时向 Telegram Saved Messages
// 和/或 webhook 发送通知；全部 best-effort，失败仅记日志，绝不影响任务流程。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"tg-down/internal/logger"
	"tg-down/internal/queue"
)

// notifyTimeout 是单次通知（Telegram/webhook）的超时上限
const notifyTimeout = 10 * time.Second

// Notifier 任务终结通知器；两个通道均可选
type Notifier struct {
	selfSend   func(ctx context.Context, text string) error // nil = 不发 Telegram
	webhookURL string                                       // 空 = 不发 webhook
	logger     *logger.Logger
	httpClient *http.Client
}

// New 创建通知器；selfSend 为 nil 且 webhookURL 为空时返回 nil（调用方据此跳过接线）
func New(selfSend func(ctx context.Context, text string) error, webhookURL string, log *logger.Logger) *Notifier {
	if selfSend == nil && webhookURL == "" {
		return nil
	}
	return &Notifier{
		selfSend:   selfSend,
		webhookURL: webhookURL,
		logger:     log,
		httpClient: &http.Client{Timeout: notifyTimeout},
	}
}

// TaskFinished 异步发送任务终结通知（按任务粒度，绝不按文件）
func (n *Notifier) TaskFinished(dto *queue.TaskDTO) {
	if dto == nil {
		return
	}
	snapshot := *dto
	if n.selfSend != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
			defer cancel()
			if err := n.selfSend(ctx, formatMessage(&snapshot)); err != nil {
				n.logger.Warn("Telegram 通知发送失败: %v", err)
			}
		}()
	}
	if n.webhookURL != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
			defer cancel()
			if err := n.postWebhook(ctx, &snapshot); err != nil {
				n.logger.Warn("webhook 通知发送失败: %v", err)
			}
		}()
	}
}

// formatMessage 生成 Saved Messages 通知文本
func formatMessage(dto *queue.TaskDTO) string {
	title := dto.ChatTitle
	if title == "" {
		title = fmt.Sprintf("ID %d", dto.ChatID)
	}
	switch dto.Status {
	case string(queue.StatusCompleted):
		return fmt.Sprintf("✅ Tg-Down 任务完成：%s\n下载 %d，跳过 %d，失败 %d",
			title, dto.Stats.Downloaded, dto.Stats.Skipped, dto.Stats.Failed)
	case string(queue.StatusPartial):
		// partial 也是终态（历史扫完了，只是部分文件失败），queue 同样会触发通知。
		// 落进 default 分支会把"下了 900 个、失败 100 个"报成彻底失败，还把成功数丢了。
		return fmt.Sprintf("⚠️ Tg-Down 任务部分完成：%s\n下载 %d，跳过 %d，失败 %d",
			title, dto.Stats.Downloaded, dto.Stats.Skipped, dto.Stats.Failed)
	default:
		return fmt.Sprintf("❌ Tg-Down 任务失败：%s\n%s", title, dto.Error)
	}
}

// postWebhook 向 webhookURL POST 任务 JSON（外层包 event 字段）
func (n *Notifier) postWebhook(ctx context.Context, dto *queue.TaskDTO) error {
	payload, err := json.Marshal(map[string]any{
		"event": "task_finished",
		"task":  dto,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("webhook 返回 %s", resp.Status)
	}
	return nil
}
