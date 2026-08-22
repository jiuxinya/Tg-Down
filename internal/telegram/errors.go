package telegram

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	tdclient "github.com/zelenin/go-tdlib/client"
)

// TDLib/Telegram 错误码
const (
	// tdTooManyRequests 是限流（FLOOD_WAIT），错误消息形如 "Too Many Requests: retry after 30"
	tdTooManyRequests = 429
	// tdFlood 是旧式限流码
	tdFlood = 420
	// tdServerErrorFloor 及以上为服务端内部错误，重试有意义
	tdServerErrorFloor = 500
)

// errTDTimeout 表示单次 TDLib 请求超出本地时限（由 tdCall 产生，非服务端错误）。
// 用哨兵而非错误字符串标识：此前重试判定靠 strings.Contains 匹配英文关键字，
// 而本项目的错误消息是中文，导致超时错误从未被判定为可重试。
var errTDTimeout = errors.New("TDLib 请求超时")

// errDownloadIncomplete 表示 TDLib 返回了文件但未标记下载完成。
var errDownloadIncomplete = errors.New("下载未完成")

// floodWaitPattern 从 429 的错误消息中提取服务端要求的等待秒数。
var floodWaitPattern = regexp.MustCompile(`(?i)retry after (\d+)`)

// tdResponseError 从错误链中提取 TDLib 的结构化错误。
// 注意 go-tdlib 返回的是值类型 ResponseError 而非指针，errors.As 的目标必须是值变量，
// 写成 *tdclient.ResponseError 会永远匹配失败。
func tdResponseError(err error) (*tdclient.Error, bool) {
	var re tdclient.ResponseError
	if errors.As(err, &re) && re.Err != nil {
		return re.Err, true
	}
	return nil, false
}

// tdRetryAfter 提取服务端指定的限流等待时长。
//
// go-tdlib 的 Error 结构没有 RetryAfter 字段（该字段只存在于发消息失败等无关场景），
// 限流秒数只能从消息文本解析。无法解析时返回 false，由指数退避兜底。
func tdRetryAfter(err error) (time.Duration, bool) {
	tdErr, ok := tdResponseError(err)
	if !ok || (tdErr.Code != tdTooManyRequests && tdErr.Code != tdFlood) {
		return 0, false
	}
	m := floodWaitPattern.FindStringSubmatch(tdErr.Message)
	if m == nil {
		return 0, false
	}
	secs, convErr := strconv.Atoi(m[1])
	if convErr != nil || secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// tdShouldRetry 判定错误是否值得重试。
//
// 取代原先的字符串子串匹配（retry.DefaultShouldRetry）：那套逻辑只认英文关键字
// （connection/timeout/network/...），而本项目的错误消息经中文包装后几乎全不匹配，
// 结果是 FLOOD_WAIT 与超时都被当成不可重试而直接失败。此处改为按 TDLib 错误码判定。
func tdShouldRetry(err error) bool {
	if err == nil {
		return false
	}
	// 主动取消不是故障
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// 本地超时与下载未完成：值得再试一次
	if errors.Is(err, errTDTimeout) || errors.Is(err, errDownloadIncomplete) {
		return true
	}

	if tdErr, ok := tdResponseError(err); ok {
		switch {
		case tdErr.Code == tdTooManyRequests || tdErr.Code == tdFlood:
			return true // 限流：等待后重试（时长由 tdRetryAfter 给出）
		case tdErr.Code >= tdServerErrorFloor:
			return true // 服务端内部错误
		case strings.Contains(tdErr.Message, "_MIGRATE"):
			return true // DC 迁移
		default:
			return false // 400/401/403 等客户端错误，重试没有意义
		}
	}

	// 非 TDLib 错误（本地 IO、网络栈），沿用通用启发式
	return defaultShouldRetry(err)
}

// defaultShouldRetry 是对非 TDLib 错误的兜底判定：网络类可重试，其余不重试。
func defaultShouldRetry(err error) bool {
	errStr := strings.ToLower(err.Error())
	for _, kw := range []string{"connection", "timeout", "network", "temporary", "broken pipe", "reset by peer"} {
		if strings.Contains(errStr, kw) {
			return true
		}
	}
	return false
}
