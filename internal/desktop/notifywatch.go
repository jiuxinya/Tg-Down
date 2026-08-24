package desktop

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gen2brain/beeep"

	"tg-down/internal/logger"
)

// 初始与上限退避：断流后从 initialReconnect 起倍增，封顶 maxReconnect
const (
	initialReconnect = 2 * time.Second
	maxReconnect     = time.Minute

	sseInitBufBytes = 64 << 10 // SSE 行扫描的初始缓冲
	sseMaxLineBytes = 1 << 20  // 单行上限，超过视为异常流

	// sseDialTimeout / sseHeaderTimeout 让连不上或握手后不吐响应头的引擎在有限时间内
	// 报错重连。不设 http.Client.Timeout：那是整个请求（含读完 body）的上限，
	// 对长连接 SSE 等于定时掐断。引擎侧不发心跳，因此也没有可靠的空闲读超时可设——
	// 长时间没有事件是正常状态，不能据此判定连接已死。
	sseDialTimeout   = 5 * time.Second
	sseHeaderTimeout = 15 * time.Second
)

// TaskEvent 是 SSE task 事件载荷中本监视器关心的字段
type TaskEvent struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Title  string `json:"chat_title"`
	Done   int64  `json:"done_files"`
	Total  int64  `json:"total_files"`
}

// WatchTaskFailures 订阅 baseURL 的 /api/events SSE 流，任务到达 failed/partial 终态时弹系统通知。
// 断流按指数退避重连直到 ctx 取消；阻塞运行，调用方放入 goroutine。
//
// 桌面端只对本地引擎启用该监视：远程实例的完成/失败通知由那台机器自身的 notify 配置负责，
// 同一事件不向两处重复打扰。
func WatchTaskFailures(ctx context.Context, baseURL string, log *logger.Logger) {
	client := &http.Client{Transport: sseTransport()}
	base := strings.TrimRight(baseURL, "/")
	backoff := initialReconnect
	for ctx.Err() == nil {
		established, err := consumeEvents(ctx, client, base, log)
		if ctx.Err() != nil {
			return
		}
		// 成功建立过一次事件流就把退避清零：否则一次瞬时抖动会把重连间隔
		// 永久顶在 60s，之后每次断线都要等一分钟才恢复通知。
		if established {
			backoff = initialReconnect
		}
		log.Warn("本地事件流断开，%s 后重连: %v", backoff, err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// nextBackoff 退避倍增并封顶
func nextBackoff(cur time.Duration) time.Duration {
	if cur >= maxReconnect {
		return maxReconnect
	}
	if next := cur * 2; next < maxReconnect {
		return next
	}
	return maxReconnect
}

func sseTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: sseDialTimeout}).DialContext
	t.ResponseHeaderTimeout = sseHeaderTimeout
	return t
}

// consumeEvents 读取一次事件流直到出错。第一个返回值表示本次是否真正建立了事件流
// （拿到 200 响应），调用方据此决定要不要重置退避。
func consumeEvents(ctx context.Context, client *http.Client, base string, log *logger.Logger) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/events", http.NoBody)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return true, scanEvents(resp.Body, func(event, data string) {
		if event == "task" && data != "" {
			handleTaskEvent(data, log)
		}
	}, log)
}

// scanEvents 按 SSE 规范切帧：空行结束一帧，多条 data: 行以换行拼接（JSON 载荷本身
// 含换行时服务端就会拆成多行，覆盖式赋值会只剩最后一行，整帧再也解不出来）。
func scanEvents(r io.Reader, onFrame func(event, data string), log *logger.Logger) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, sseInitBufBytes), sseMaxLineBytes)
	var event string
	var data []string
	reset := func() { event, data = "", nil }
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			// 空行 = 帧结束
			onFrame(event, strings.Join(data, "\n"))
			reset()
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		default:
			// 注释或其他字段（如 retry:），忽略
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if log != nil {
		log.Debug("本地事件流正常结束")
	}
	return nil
}

// badFrames 累计解析失败的事件帧数。坏帧此前被静默丢弃，
// 前端明明收到了事件而桌面端一条通知都不弹时无从判断问题出在哪一层。
var badFrames atomic.Int64

func handleTaskEvent(data string, log *logger.Logger) {
	var t TaskEvent
	if err := json.Unmarshal([]byte(data), &t); err != nil {
		n := badFrames.Add(1)
		log.Warn("事件帧解析失败（累计 %d 帧）: %v", n, err)
		return
	}
	title := t.Title
	if title == "" {
		title = t.ID
	}
	var msg string
	switch t.Status {
	case "failed":
		msg = fmt.Sprintf("「%s」下载失败", title)
	case "partial":
		msg = fmt.Sprintf("「%s」部分完成（%d/%d 个文件）", title, t.Done, t.Total)
	default:
		return
	}
	log.Info("%s", msg)
	if err := beeep.Notify(AppName, msg, ""); err != nil {
		// 通知通道不可用（无通知中心/无 dbus 等）只降级为日志，不影响主流程
		log.Debug("系统通知发送失败: %v", err)
	}
}
