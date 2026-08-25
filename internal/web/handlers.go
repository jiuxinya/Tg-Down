package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"tg-down/internal/config"
	"tg-down/internal/downloader"
	"tg-down/internal/queue"
	"tg-down/internal/store"
	"tg-down/internal/tgapi"
)

// routes 注册所有 HTTP 路由
func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/chats", s.handleChats)
	mux.HandleFunc("POST /api/chats/refresh", s.handleChatsRefresh)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/auth/credentials", s.handleAuthCredentials)
	mux.HandleFunc("POST /api/auth/code", s.handleAuthCode)
	mux.HandleFunc("POST /api/auth/password", s.handleAuthPassword)
	mux.HandleFunc("POST /api/auth/abort", s.handleAuthAbort)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("GET /api/settings", s.handleSettings)
	mux.HandleFunc("POST /api/settings", s.handleSettingsUpdate)
	mux.HandleFunc("POST /api/settings/classify", s.handleSettingsClassify)
	mux.HandleFunc("GET /api/tasks", s.handleTasksList)
	mux.HandleFunc("POST /api/tasks", s.handleTasksCreate)
	mux.HandleFunc("POST /api/resolve", s.handleResolve)
	mux.HandleFunc("POST /api/tasks/{id}/cancel", s.handleTaskCancel)
	mux.HandleFunc("POST /api/tasks/{id}/retry", s.handleTaskRetry)
	mux.HandleFunc("DELETE /api/tasks/{id}/history", s.handleTaskHistoryDelete)
	mux.HandleFunc("GET /api/download/settings", s.handleDownloadSettings)
	mux.HandleFunc("POST /api/download/concurrency", s.handleDownloadConcurrency)
	mux.HandleFunc("POST /api/media/{id}/pause", s.handleMediaPause)
	mux.HandleFunc("POST /api/media/{id}/resume", s.handleMediaResume)
	mux.HandleFunc("POST /api/media/pause-all", s.handleMediaPauseAll)
	mux.HandleFunc("POST /api/media/resume-all", s.handleMediaResumeAll)
	mux.HandleFunc("GET /api/history", s.handleHistoryList)
	mux.HandleFunc("GET /api/history/stats", s.handleHistoryStats)
	mux.HandleFunc("GET /api/history/export", s.handleHistoryExport)
	mux.HandleFunc("GET /api/history/{id}/file", s.handleHistoryFile)
	mux.HandleFunc("GET /api/history/{id}/thumb", s.handleHistoryThumb)
	mux.HandleFunc("DELETE /api/history/{id}", s.handleHistoryDelete)
	mux.HandleFunc("POST /api/export", s.handleExport)
	mux.HandleFunc("GET /api/schedules", s.handleSchedulesList)
	mux.HandleFunc("POST /api/schedules", s.handleSchedulesCreate)
	mux.HandleFunc("DELETE /api/schedules/{id}", s.handleScheduleDelete)
	mux.HandleFunc("POST /api/schedules/{id}/toggle", s.handleScheduleToggle)
	mux.HandleFunc("GET /api/timeline", s.handleTimeline)
	mux.HandleFunc("GET /api/timeline/file", s.handleTimelineFile)
}

// uiNotBuiltMessage 在前端产物缺失时给出的提示（而不是白屏让人一头雾水）
const uiNotBuiltMessage = `<!doctype html><meta charset="utf-8"><title>Tg-Down</title>
<body style="font-family:system-ui;padding:40px;line-height:1.6">
<h1>前端尚未构建</h1>
<p>这个二进制是在没有前端产物的情况下编译的。请执行：</p>
<pre style="background:#f2f2f7;padding:12px;border-radius:8px">make web &amp;&amp; make build</pre>
<p>API 仍然可用（<code>/api/...</code>）。</p>
</body>`

// handleIndex 提供前端：静态资源直接从内嵌的 dist 里取，其余路径回落到 index.html（SPA 路由）。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	handleUI(w, r)
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, s.snapshot())
}

func (s *Server) handleChats(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	chats := s.chats
	s.mu.RUnlock()
	if chats == nil {
		chats = []tgapi.ChatInfo{}
	}
	s.writeJSON(w, chats)
}

func (s *Server) handleChatsRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.requireReady(w) {
		return
	}
	s.refreshChats(r.Context())
	s.mu.RLock()
	chats := s.chats
	s.mu.RUnlock()
	if chats == nil {
		chats = []tgapi.ChatInfo{}
	}
	s.writeJSON(w, chats)
}

func (s *Server) handleAuthCredentials(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIID   int64  `json:"api_id"`
		APIHash string `json:"api_hash"`
		Phone   string `json:"phone"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	body.APIHash = strings.TrimSpace(body.APIHash)
	body.Phone = strings.TrimSpace(body.Phone)
	if !config.IsValidAPIID(body.APIID) {
		s.writeError(w, http.StatusBadRequest, "api_id 必须为有效的正整数")
		return
	}
	if !config.IsValidAPIHash(body.APIHash) {
		s.writeError(w, http.StatusBadRequest, "api_hash 必须为 32 位十六进制字符串")
		return
	}
	if !config.IsValidPhone(body.Phone) {
		s.writeError(w, http.StatusBadRequest, "手机号必须为国际格式（+ 后跟 7 到 15 位数字）")
		return
	}
	state, _ := s.currentState()
	if state != StateNeedCredentials && state != StateError {
		s.writeError(w, http.StatusConflict, "当前认证步骤不能修改 API 凭据")
		return
	}
	if !s.reserveCredentialSubmission() {
		s.writeError(w, http.StatusConflict, "登录请求处理中，请稍候")
		return
	}
	queued := false
	defer func() {
		if !queued {
			s.releaseCredentialSubmission()
		}
	}()

	s.client.SetCredentials(int(body.APIID), body.APIHash, body.Phone)
	if err := s.client.SaveConfig(); err != nil {
		s.logger.Warn("保存配置失败（不影响本次登录）: %v", err)
	} else {
		s.logger.Info("已保存 API 凭据到 config.yaml")
	}

	select {
	case s.credCh <- struct{}{}:
		queued = true
		s.writeOK(w)
	default:
		// credSlot 与 credCh 同容量，正常运行不会走到这里；保留防御性错误，避免阻塞请求。
		s.writeError(w, http.StatusInternalServerError, "登录请求入队失败")
	}
}

func (s *Server) handleAuthCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Code) == "" {
		s.writeError(w, http.StatusBadRequest, "验证码不能为空")
		return
	}
	if state, _ := s.currentState(); state != StateWaitingCode {
		s.writeError(w, http.StatusConflict, "当前不在等待验证码状态")
		return
	}
	select {
	case s.codeCh <- strings.TrimSpace(body.Code):
		s.writeOK(w)
	default:
		s.writeError(w, http.StatusConflict, "验证码提交处理中，请勿重复提交")
	}
}

func (s *Server) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.Password == "" {
		s.writeError(w, http.StatusBadRequest, "密码不能为空")
		return
	}
	if state, _ := s.currentState(); state != StateWaitingPassword {
		s.writeError(w, http.StatusConflict, "当前不在等待密码状态")
		return
	}
	select {
	case s.passCh <- body.Password:
		s.writeOK(w)
	default:
		s.writeError(w, http.StatusConflict, "密码提交处理中，请勿重复提交")
	}
}

// handleAuthAbort 中止当前登录（验证码/密码步骤的"返回上一步"），回到凭据输入页
func (s *Server) handleAuthAbort(w http.ResponseWriter, _ *http.Request) {
	if state, _ := s.currentState(); state != StateWaitingCode && state != StateWaitingPassword {
		s.writeError(w, http.StatusConflict, "当前不在等待验证码/密码状态")
		return
	}
	select {
	case s.abortCh <- struct{}{}:
		s.writeOK(w)
	default:
		s.writeError(w, http.StatusConflict, "中止请求处理中，请勿重复提交")
	}
}

// handleAuthLogout 注销当前 Telegram 会话：先取消所有活动任务，再吊销授权并销毁本地会话，
// 最后通知 runTelegram 重新进入认证循环（回到凭据输入页）
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !s.beginLogout() {
		s.writeError(w, http.StatusConflict, "Telegram 尚未就绪或正在登出")
		return
	}
	completed := false
	activeIDs, err := s.queue.BeginDrain()
	if err != nil {
		s.setState(StateReady)
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.queueDraining.Store(true)
	defer func() {
		if !completed {
			if s.queueDraining.Load() {
				if err := s.queue.EndDrain(); err != nil {
					s.logger.Error("恢复任务队列失败: %v", err)
				} else {
					s.queueDraining.Store(false)
				}
			}
			s.setState(StateReady)
		}
	}()
	for _, id := range activeIDs {
		if err := s.queue.Cancel(id); err != nil {
			// BeginDrain 也返回已到终态但尚未完成持久化/通知的任务；这类任务无需再次取消，
			// 仍必须在下面 Wait 到 done 关闭。
			s.logger.Warn("登出前取消任务 %s 失败: %v", id, err)
		}
	}
	logoutCtx, cancel := context.WithTimeout(r.Context(), logoutTaskWaitTimeout)
	defer cancel()
	for _, id := range activeIDs {
		if err := s.queue.Wait(logoutCtx, id); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			s.writeError(w, status, "等待下载任务结束失败: "+err.Error())
			return
		}
	}
	if err := s.client.Logout(logoutCtx); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	select {
	case s.logoutCh <- struct{}{}:
	default:
	}
	completed = true
	s.writeOK(w)
}

func (s *Server) handleTasksList(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, s.queue.List())
}

func (s *Server) handleTasksCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind   string `json:"kind"`
		ChatID int64  `json:"chat_id"`
		// Filters 是任务级过滤条件（仅 history 任务生效；monitor 在 v2.0 忽略过滤器）
		Filters downloader.HistoryFilters `json:"filters"`
		// MessageID 非 0 时创建单消息下载任务（来自 /api/resolve 的消息链接解析）
		MessageID int64 `json:"message_id"`
		// ChatTitle 可选；公开频道可能不在缓存聊天列表中，由解析结果直接携带标题
		ChatTitle string `json:"chat_title"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	kind := queue.Kind(body.Kind)
	if kind != queue.KindHistory && kind != queue.KindMonitor {
		s.writeError(w, http.StatusBadRequest, "kind 必须为 history 或 monitor")
		return
	}
	if body.ChatID == 0 {
		s.writeError(w, http.StatusBadRequest, "chat_id 不能为空")
		return
	}
	if body.MessageID < 0 {
		s.writeError(w, http.StatusBadRequest, "message_id 不能为负")
		return
	}
	if msg := body.Filters.Validate(); msg != "" {
		s.writeError(w, http.StatusBadRequest, msg)
		return
	}
	if kind == queue.KindMonitor && (!body.Filters.IsZero() || body.MessageID != 0) {
		s.writeError(w, http.StatusBadRequest, "monitor 任务不支持 filters 或 message_id")
		return
	}
	if !s.requireReady(w) {
		return
	}
	title := body.ChatTitle
	if title == "" {
		title = s.chatTitle(body.ChatID)
	}
	spec := &downloader.HistorySpec{ChatID: body.ChatID, Filters: body.Filters, MessageID: body.MessageID}
	dto, err := s.queue.Enqueue(kind, spec, title)
	if err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeJSON(w, dto)
}

// handleExport 把一个聊天导出为 JSON + 自包含 HTML。
//
// 同步执行：导出要翻完整条历史，大频道会很慢，因此请求上下文即取消信号——
// 用户关掉页面，导出随之停止，不会留下一个无人认领的后台任务。
// limit 由前端传入以便先小规模试一次，0 = 全量。
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChatID    int64  `json:"chat_id"`
		ChatTitle string `json:"chat_title"`
		Limit     int    `json:"limit"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.ChatID == 0 {
		s.writeError(w, http.StatusBadRequest, "chat_id 不能为空")
		return
	}
	if body.Limit < 0 {
		s.writeError(w, http.StatusBadRequest, "limit 不能为负")
		return
	}
	if !s.requireReady(w) {
		return
	}

	title := body.ChatTitle
	if title == "" {
		title = s.chatTitle(body.ChatID)
	}
	res, err := s.client.ExportChat(r.Context(), tgapi.ExportSpec{
		ChatID:    body.ChatID,
		ChatTitle: title,
		Limit:     body.Limit,
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, res)
}

// handleResolve 解析 t.me 链接 / @用户名为聊天与可选消息 id，供前端确认后创建任务
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Input string `json:"input"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if !s.requireReady(w) {
		return
	}
	target, err := s.client.ResolveTarget(r.Context(), body.Input)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeJSON(w, target)
}

func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.queue.Cancel(r.PathValue("id")); err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeOK(w)
}

func (s *Server) handleTaskRetry(w http.ResponseWriter, r *http.Request) {
	dto, err := s.queue.Retry(r.PathValue("id"))
	if err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeJSON(w, dto)
}

/* ---- 定时下载计划 ---- */

func (s *Server) handleSchedulesList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListSchedules(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []*store.ScheduleRow{}
	}
	s.writeJSON(w, rows)
}

func (s *Server) handleSchedulesCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChatID      int64                     `json:"chat_id"`
		IntervalMin int                       `json:"interval_min"`
		Filters     downloader.HistoryFilters `json:"filters"`
		ChatTitle   string                    `json:"chat_title"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.ChatID == 0 {
		s.writeError(w, http.StatusBadRequest, "chat_id 不能为空")
		return
	}
	if body.IntervalMin < queue.MinScheduleIntervalMin {
		s.writeError(w, http.StatusBadRequest,
			fmt.Sprintf("间隔不能小于 %d 分钟", queue.MinScheduleIntervalMin))
		return
	}
	if int64(body.IntervalMin) > queue.MaxScheduleIntervalMin {
		s.writeError(w, http.StatusBadRequest, "计划间隔过大")
		return
	}
	if msg := body.Filters.Validate(); msg != "" {
		s.writeError(w, http.StatusBadRequest, msg)
		return
	}
	title := body.ChatTitle
	if title == "" {
		title = s.chatTitle(body.ChatID)
	}
	filtersJSON := ""
	if !body.Filters.IsZero() {
		if data, err := json.Marshal(body.Filters); err == nil {
			filtersJSON = string(data)
		}
	}
	row := &store.ScheduleRow{
		ID:          fmt.Sprintf("s%d", time.Now().UnixNano()),
		ChatID:      body.ChatID,
		ChatTitle:   title,
		IntervalMin: body.IntervalMin,
		Filters:     filtersJSON,
		Enabled:     true,
		CreatedAt:   time.Now(),
	}
	if err := s.store.CreateSchedule(r.Context(), row); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, row)
}

func (s *Server) handleScheduleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSchedule(r.Context(), r.PathValue("id")); err != nil {
		s.writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.writeOK(w)
}

// handleHistoryDelete 删除一条历史下载记录（只删数据库记录，不影响磁盘文件）
func (s *Server) handleHistoryDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "无效的记录 ID")
		return
	}
	if err := s.store.DeleteHistory(r.Context(), id); err != nil {
		s.writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.writeOK(w)
}

// handleTaskHistoryDelete 删除某个任务关联的全部下载历史记录（文件保留）
func (s *Server) handleTaskHistoryDelete(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	n, err := s.store.DeleteHistoryByTask(r.Context(), taskID)
	if err != nil {
		s.writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.writeJSON(w, map[string]any{"deleted": n})
}

func (s *Server) handleScheduleToggle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		s.writeError(w, http.StatusBadRequest, "enabled 字段不能为空")
		return
	}
	if err := s.store.SetScheduleEnabled(r.Context(), r.PathValue("id"), *body.Enabled); err != nil {
		s.writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.writeOK(w)
}

func (s *Server) handleDownloadSettings(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, downloadSettingsDTO{
		MaxConcurrent: s.client.DownloadConcurrency(),
		Active:        s.client.ActiveDownloadCount(),
	})
}

func (s *Server) handleDownloadConcurrency(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MaxConcurrent int `json:"max_concurrent"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.MaxConcurrent <= 0 {
		s.writeError(w, http.StatusBadRequest, "并发数量必须大于 0")
		return
	}
	if err := s.client.SetDownloadConcurrency(body.MaxConcurrent); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, downloadSettingsDTO{
		MaxConcurrent: s.client.DownloadConcurrency(),
		Active:        s.client.ActiveDownloadCount(),
	})
}

func (s *Server) handleMediaPause(w http.ResponseWriter, r *http.Request) {
	if err := s.client.PauseMedia(r.Context(), r.PathValue("id")); err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeOK(w)
}

func (s *Server) handleMediaResume(w http.ResponseWriter, r *http.Request) {
	if err := s.client.ResumeMedia(r.PathValue("id")); err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeOK(w)
}

func (s *Server) handleMediaPauseAll(w http.ResponseWriter, r *http.Request) {
	s.client.PauseAllMedia(r.Context())
	s.logger.Info("已暂停全部媒体下载")
	s.writeOK(w)
}

func (s *Server) handleMediaResumeAll(w http.ResponseWriter, _ *http.Request) {
	s.client.ResumeAllMedia()
	s.logger.Info("已继续全部媒体下载")
	s.writeOK(w)
}

// chatTitle 在已加载的聊天列表中按 ID 查找标题，未找到返回空串
func (s *Server) chatTitle(chatID int64) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.chats {
		if c.ID == chatID {
			return c.Title
		}
	}
	return ""
}

func (s *Server) handleHistoryList(w http.ResponseWriter, r *http.Request) {
	filter, ok := s.parseHistoryFilter(w, r)
	if !ok {
		return
	}

	page, err := s.store.QueryHistory(r.Context(), &filter)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dtos := make([]historyRecordDTO, len(page.Items))
	for i, rec := range page.Items {
		dtos[i] = toHistoryRecordDTO(rec)
	}
	s.writeJSON(w, historyListResponse{
		Items:      dtos,
		NextCursor: encodeHistoryCursor(page.NextCursor),
		Total:      page.Total,
		Limit:      filter.Limit,
	})
}

func (s *Server) handleHistoryStats(w http.ResponseWriter, r *http.Request) {
	filter, ok := s.parseHistoryFilter(w, r)
	if !ok {
		return
	}
	stats, err := s.store.HistoryStats(r.Context(), &filter)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dtos := make([]mediaTypeStatDTO, len(stats))
	for i, st := range stats {
		dtos[i] = toMediaTypeStatDTO(st)
	}
	s.writeJSON(w, historyStatsResponse{ByType: dtos})
}

// maxHistoryExportRows 是单次导出的行数上限：导出走的是同步响应，
// 不设上限时一个百万行的库会把整张表读进内存再吐给浏览器。
const maxHistoryExportRows = 50_000

// historyExportFormats 是受支持的导出格式
const (
	exportFormatCSV  = "csv"
	exportFormatJSON = "json"
)

// handleHistoryExport 按当前筛选条件导出下载历史（CSV 或 JSON）。
//
// 复用列表的筛选与排序参数，但忽略分页——导出的语义是"这批筛选结果的全部"，
// 而非"当前这一页"。内部按游标分批取，避免一次性把匹配集读进内存。
func (s *Server) handleHistoryExport(w http.ResponseWriter, r *http.Request) {
	filter, ok := s.parseHistoryFilter(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = exportFormatCSV
	}
	if format != exportFormatCSV && format != exportFormatJSON {
		s.writeError(w, http.StatusBadRequest, "format 仅支持 csv 或 json")
		return
	}

	records, err := s.collectHistoryForExport(r.Context(), &filter)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	filename := "tg-down-history-" + time.Now().Format("20060102-150405") + "." + format
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	if format == exportFormatJSON {
		s.writeHistoryExportJSON(w, records)
		return
	}
	s.writeHistoryExportCSV(w, records)
}

// collectHistoryForExport 按游标分批取全部匹配行，上限 maxHistoryExportRows
func (s *Server) collectHistoryForExport(
	ctx context.Context, filter *store.HistoryFilter,
) ([]*store.HistoryRecord, error) {
	filter.Cursor = nil
	filter.WithTotal = false
	filter.Limit = store.MaxHistoryPageSize

	records := make([]*store.HistoryRecord, 0, store.MaxHistoryPageSize)
	for len(records) < maxHistoryExportRows {
		page, err := s.store.QueryHistory(ctx, filter)
		if err != nil {
			return nil, err
		}
		records = append(records, page.Items...)
		if page.NextCursor == nil {
			break
		}
		filter.Cursor = page.NextCursor
	}
	if len(records) > maxHistoryExportRows {
		records = records[:maxHistoryExportRows]
	}
	return records, nil
}

func (s *Server) writeHistoryExportJSON(w http.ResponseWriter, records []*store.HistoryRecord) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	dtos := make([]historyRecordDTO, len(records))
	for i, rec := range records {
		dtos[i] = toHistoryRecordDTO(rec)
	}
	if err := json.NewEncoder(w).Encode(dtos); err != nil {
		s.logger.Warn("导出历史失败: %v", err)
	}
}

// historyExportHeader 是 CSV 表头，与 historyExportRow 的字段顺序一一对应
var historyExportHeader = []string{
	"id", "task_id", "chat_id", "chat_title", "message_id", "media_type",
	"file_name", "file_path", "file_size", "mime_type", statusColumnName, "reason",
	"created_at", "finished_at", "album_id",
}

// statusColumnName 是导出 CSV 的状态列名
const statusColumnName = "status"

func historyExportRow(rec *store.HistoryRecord) []string {
	finished := ""
	if rec.FinishedAt != nil {
		finished = rec.FinishedAt.Format(time.RFC3339)
	}
	return []string{
		strconv.FormatInt(rec.ID, 10), rec.TaskID, strconv.FormatInt(rec.ChatID, 10),
		rec.ChatTitle, strconv.FormatInt(rec.MessageID, 10), rec.MediaType,
		rec.FileName, rec.FilePath, strconv.FormatInt(rec.FileSize, 10), rec.MimeType,
		rec.Status, rec.Reason, rec.CreatedAt.Format(time.RFC3339), finished,
		strconv.FormatInt(rec.AlbumID, 10),
	}
}

func (s *Server) writeHistoryExportCSV(w http.ResponseWriter, records []*store.HistoryRecord) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	// UTF-8 BOM：没有它 Excel 会把中文文件名按本地代码页解释成乱码
	if _, err := w.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(historyExportHeader); err != nil {
		s.logger.Warn("导出历史失败: %v", err)
		return
	}
	for _, rec := range records {
		if err := cw.Write(historyExportRow(rec)); err != nil {
			s.logger.Warn("导出历史失败: %v", err)
			return
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		s.logger.Warn("导出历史失败: %v", err)
	}
}

// parseHistoryFilter 解析 /api/history 与 /api/history/stats 共用的查询参数；
// from/to 接受 RFC3339 或 unix 秒两种格式
func (s *Server) parseHistoryFilter(w http.ResponseWriter, r *http.Request) (filter store.HistoryFilter, ok bool) {
	q := r.URL.Query()
	filter.MediaType = q.Get("type")
	if filter.MediaType == "" {
		filter.MediaType = q.Get("media_type") // 兼容 v3.0 早期前端
	}
	filter.Status = q.Get("status")
	filter.Query = q.Get("q")
	filter.TaskID = q.Get("task_id")

	if v := q.Get("chat_id"); v != "" {
		chatID, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "chat_id 格式错误")
			return filter, false
		}
		filter.ChatID = chatID
	}
	if !s.parseHistoryTimeRange(w, q, &filter) {
		return filter, false
	}
	if !s.parseHistoryPaging(w, q, &filter) {
		return filter, false
	}
	return filter, true
}

// parseHistoryTimeRange 解析 from/to
func (s *Server) parseHistoryTimeRange(
	w http.ResponseWriter, q url.Values, filter *store.HistoryFilter,
) bool {
	if t, err := parseHistoryTime(q.Get("from")); err != nil {
		s.writeError(w, http.StatusBadRequest, "from 格式错误，需为 RFC3339 或 unix 秒")
		return false
	} else if t != nil {
		filter.From = t
	}
	if t, err := parseHistoryTime(q.Get("to")); err != nil {
		s.writeError(w, http.StatusBadRequest, "to 格式错误，需为 RFC3339 或 unix 秒")
		return false
	} else if t != nil {
		filter.To = t
	}
	return true
}

// parseHistoryPaging 解析排序、每页条数、游标与是否要总数
func (s *Server) parseHistoryPaging(
	w http.ResponseWriter, q url.Values, filter *store.HistoryFilter,
) bool {
	filter.Sort = store.HistorySortCreatedDesc
	if v := q.Get("sort"); v != "" {
		sort := store.HistorySort(v)
		if !sort.IsValid() {
			s.writeError(w, http.StatusBadRequest, "sort 取值不受支持")
			return false
		}
		filter.Sort = sort
	}

	filter.Limit = store.DefaultHistoryPageSize
	// page_size 是 v3.1 及更早前端的参数名，与 limit 等价，一并接受
	sizeParam := q.Get("limit")
	if sizeParam == "" {
		sizeParam = q.Get("page_size")
	}
	if sizeParam != "" {
		n, err := strconv.Atoi(sizeParam)
		if err != nil || n <= 0 {
			s.writeError(w, http.StatusBadRequest, "limit 格式错误")
			return false
		}
		filter.Limit = min(n, store.MaxHistoryPageSize)
	}

	if v := q.Get("cursor"); v != "" {
		cursor, err := parseHistoryCursor(v)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "cursor 格式错误")
			return false
		}
		filter.Cursor = cursor
	}
	filter.WithTotal = q.Get("with_total") == "1"
	return true
}

// encodeHistoryCursor 把游标编码成 "<排序键>.<id>" 的不透明字符串。
// 前端只负责原样回传，不解析其含义，因此排序键换列也不影响前端。
func encodeHistoryCursor(c *store.HistoryCursor) string {
	if c == nil {
		return ""
	}
	return strconv.FormatInt(c.SortValue, 10) + "." + strconv.FormatInt(c.ID, 10)
}

// parseHistoryCursor 解析 encodeHistoryCursor 的产物
func parseHistoryCursor(v string) (*store.HistoryCursor, error) {
	sortValue, id, found := strings.Cut(v, ".")
	if !found {
		return nil, fmt.Errorf("游标缺少分隔符")
	}
	sv, err := strconv.ParseInt(sortValue, 10, 64)
	if err != nil {
		return nil, err
	}
	rowID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, err
	}
	return &store.HistoryCursor{SortValue: sv, ID: rowID}, nil
}

// parseHistoryTime 解析 RFC3339 或 unix 秒时间戳，空串返回 nil
func parseHistoryTime(v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
		t := time.Unix(sec, 0)
		return &t, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")

	ch := s.hub.add()
	defer s.hub.remove(ch)

	if data, err := json.Marshal(s.snapshot()); err == nil {
		writeSSE(w, flusher, sseMessage{Event: eventState, Data: string(data)})
	}

	for {
		select {
		case msg, alive := <-ch:
			if !alive {
				return
			}
			writeSSE(w, flusher, msg)
		case <-r.Context().Done():
			return
		}
	}
}

// --- 下载历史 DTOs ---

type historyRecordDTO struct {
	ID         int64  `json:"id"`
	TaskID     string `json:"task_id,omitempty"`
	ChatID     int64  `json:"chat_id"`
	ChatTitle  string `json:"chat_title,omitempty"`
	MessageID  int64  `json:"message_id"`
	MediaType  string `json:"media_type"`
	FileName   string `json:"file_name"`
	FilePath   string `json:"file_path"`
	FileSize   int64  `json:"file_size"`
	MimeType   string `json:"mime_type,omitempty"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	FinishedAt *int64 `json:"finished_at,omitempty"`
	// AlbumID 让前端能把同一相册的媒体聚成一组。库里一直有这一列、文件也一直按
	// album_<id> 分目录，唯独 DTO 把它丢了，画廊因此拼不出相册。
	AlbumID int64 `json:"album_id,omitempty"`
	// HasThumb 为真时 GET /api/history/{id}/thumb 有内容可返回
	HasThumb bool `json:"has_thumb"`
}

func toHistoryRecordDTO(rec *store.HistoryRecord) historyRecordDTO {
	dto := historyRecordDTO{
		ID:        rec.ID,
		TaskID:    rec.TaskID,
		ChatID:    rec.ChatID,
		ChatTitle: rec.ChatTitle,
		MessageID: rec.MessageID,
		MediaType: rec.MediaType,
		FileName:  rec.FileName,
		FilePath:  rec.FilePath,
		FileSize:  rec.FileSize,
		MimeType:  rec.MimeType,
		Status:    rec.Status,
		Reason:    rec.Reason,
		CreatedAt: rec.CreatedAt.Unix(),
		AlbumID:   rec.AlbumID,
		HasThumb:  rec.ThumbPath != "" || len(rec.Minithumb) > 0,
	}
	if rec.FinishedAt != nil {
		sec := rec.FinishedAt.Unix()
		dto.FinishedAt = &sec
	}
	return dto
}

type historyListResponse struct {
	Items []historyRecordDTO `json:"items"`
	// NextCursor 是下一页的不透明游标，空串表示已到末页。
	// 前端原样回传即可，不需要理解其内部结构。
	NextCursor string `json:"next_cursor,omitempty"`
	// Total 仅在请求带 with_total=1 时有值：COUNT(*) 要走一遍完整匹配集，
	// 而翻页时总数不变，因此只在筛选条件变化的那一次请求里要。
	Total *int `json:"total,omitempty"`
	Limit int  `json:"limit"`
}

type mediaTypeStatDTO struct {
	MediaType string `json:"media_type"`
	Count     int    `json:"count"`
	TotalSize int64  `json:"total_size"`
	Completed int    `json:"completed"`
	Failed    int    `json:"failed"`
	Skipped   int    `json:"skipped"`
}

func toMediaTypeStatDTO(st store.MediaTypeStat) mediaTypeStatDTO {
	return mediaTypeStatDTO{
		MediaType: st.MediaType,
		Count:     st.Count,
		TotalSize: st.TotalSize,
		Completed: st.Completed,
		Failed:    st.Failed,
		Skipped:   st.Skipped,
	}
}

type historyStatsResponse struct {
	ByType []mediaTypeStatDTO `json:"by_type"`
}

// --- helpers ---

func (s *Server) requireReady(w http.ResponseWriter) bool {
	if state, _ := s.currentState(); state != StateReady {
		s.writeError(w, http.StatusConflict, "Telegram 尚未就绪")
		return false
	}
	return true
}

func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Error("写响应失败: %v", err)
	}
}

func (s *Server) writeOK(w http.ResponseWriter) {
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		s.writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			s.writeError(w, http.StatusBadRequest, "请求体只能包含一个 JSON 值")
		} else {
			s.writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		}
		return false
	}
	return true
}

func writeSSE(w http.ResponseWriter, f http.Flusher, msg sseMessage) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Event, msg.Data)
	f.Flush()
}

// --- SSE hub ---

type sseMessage struct {
	Event string
	Data  string
}

type sseHub struct {
	mu   sync.Mutex
	subs map[chan sseMessage]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{subs: make(map[chan sseMessage]struct{})}
}

func (h *sseHub) add() chan sseMessage {
	ch := make(chan sseMessage, sseBufferSize)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) remove(ch chan sseMessage) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// broadcast 非阻塞地向所有订阅者推送（缓冲满则丢弃该条）
func (h *sseHub) broadcast(msg sseMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}
