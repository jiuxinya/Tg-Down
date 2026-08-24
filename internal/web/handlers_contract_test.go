package web

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"tg-down/internal/config"
	"tg-down/internal/logger"
	"tg-down/internal/queue"
	"tg-down/internal/store"
	"tg-down/internal/tgapi"
)

const validTestAPIHash = "0123456789abcdef0123456789abcdef"

type credentialClient struct {
	tgapi.Client
	apiID     int
	apiHash   string
	phone     string
	saveCalls int
}

func (c *credentialClient) SetCredentials(apiID int, apiHash, phone string) {
	c.apiID = apiID
	c.apiHash = apiHash
	c.phone = phone
}

func (c *credentialClient) SaveConfig() error {
	c.saveCalls++
	return nil
}

func (c *credentialClient) HasCredentials() bool {
	return c.apiID > 0 && c.apiHash != "" && c.phone != ""
}

func (c *credentialClient) Phone() string { return c.phone }

func TestHandleAuthCredentialsValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "zero api id", body: `{"api_id":0,"api_hash":"` + validTestAPIHash + `","phone":"+12025550123"}`},
		{name: "negative api id", body: `{"api_id":-1,"api_hash":"` + validTestAPIHash + `","phone":"+12025550123"}`},
		{name: "overflow api id", body: `{"api_id":2147483648,"api_hash":"` + validTestAPIHash + `","phone":"+12025550123"}`},
		{name: "short hash", body: `{"api_id":1,"api_hash":"abc","phone":"+12025550123"}`},
		{name: "non hex hash", body: `{"api_id":1,"api_hash":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz","phone":"+12025550123"}`},
		{name: "phone without plus", body: `{"api_id":1,"api_hash":"` + validTestAPIHash + `","phone":"12025550123"}`},
		{name: "unicode phone", body: `{"api_id":1,"api_hash":"` + validTestAPIHash + `","phone":"+一二三四五六七"}`},
		{name: "unknown field", body: `{"api_id":1,"api_hash":"` + validTestAPIHash + `","phone":"+12025550123","api_hahs":"typo"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			req := httptest.NewRequest(http.MethodPost, "/api/auth/credentials", strings.NewReader(tt.body))
			res := httptest.NewRecorder()
			s.handleAuthCredentials(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandleAuthCredentialsAcceptsValidInput(t *testing.T) {
	log := logger.New("error")
	client := &credentialClient{}
	s := &Server{
		client: client, logger: log, state: StateNeedCredentials,
		credCh: make(chan struct{}, 1), credSlot: make(chan struct{}, 1),
	}
	body := `{"api_id":12345,"api_hash":"` + validTestAPIHash + `","phone":"+12025550123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/credentials", strings.NewReader(body))
	res := httptest.NewRecorder()
	s.handleAuthCredentials(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", res.Code, res.Body.String())
	}
	if !client.HasCredentials() || client.Phone() != "+12025550123" {
		t.Fatalf("client credentials were not updated, phone = %q", client.Phone())
	}
	if client.apiID != 12345 || client.apiHash != validTestAPIHash || client.saveCalls != 1 {
		t.Fatalf("credential update = %#v, want one complete saved update", client)
	}
}

func TestHandleAuthCredentialsRejectsSecondSubmissionWithoutMutation(t *testing.T) {
	client := &credentialClient{}
	s := &Server{
		client: client, logger: logger.New("error"), state: StateNeedCredentials,
		credCh: make(chan struct{}, 1), credSlot: make(chan struct{}, 1),
	}

	first := `{"api_id":12345,"api_hash":"` + validTestAPIHash + `","phone":"+12025550123"}`
	firstRes := httptest.NewRecorder()
	s.handleAuthCredentials(firstRes, httptest.NewRequest(http.MethodPost, "/api/auth/credentials", strings.NewReader(first)))
	if firstRes.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200 (%s)", firstRes.Code, firstRes.Body.String())
	}

	secondHash := "fedcba9876543210fedcba9876543210"
	second := `{"api_id":54321,"api_hash":"` + secondHash + `","phone":"+442071838750"}`
	secondRes := httptest.NewRecorder()
	s.handleAuthCredentials(secondRes, httptest.NewRequest(http.MethodPost, "/api/auth/credentials", strings.NewReader(second)))
	if secondRes.Code != http.StatusConflict {
		t.Fatalf("second status = %d, want 409 (%s)", secondRes.Code, secondRes.Body.String())
	}
	if client.apiID != 12345 || client.apiHash != validTestAPIHash || client.phone != "+12025550123" || client.saveCalls != 1 {
		t.Fatalf("second submission mutated credentials: %#v", client)
	}
}

func TestDecodeRejectsMultipleJSONValues(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/test", strings.NewReader(`{"enabled":true} {"enabled":false}`))
	res := httptest.NewRecorder()
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if s.decode(res, req, &body) {
		t.Fatal("decode accepted multiple JSON values")
	}
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
}

func TestHistoryAPIContracts(t *testing.T) {
	s, root := newMediaTestServer(t)
	addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "photo", FileName: "a.jpg", FilePath: root + "/a.jpg",
	})
	addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 2, MediaType: "video", FileName: "b.mp4", FilePath: root + "/b.mp4",
	})

	listReq := httptest.NewRequest(http.MethodGet, "/api/history?type=photo", http.NoBody)
	listRes := httptest.NewRecorder()
	s.handleHistoryList(listRes, listReq)
	var page historyListResponse
	if err := json.NewDecoder(listRes.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if listRes.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].MediaType != "photo" {
		t.Fatalf("filtered history = %#v, status = %d", page, listRes.Code)
	}

	legacyReq := httptest.NewRequest(http.MethodGet, "/api/history?media_type=video", http.NoBody)
	legacyRes := httptest.NewRecorder()
	s.handleHistoryList(legacyRes, legacyReq)
	page = historyListResponse{}
	if err := json.NewDecoder(legacyRes.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].MediaType != "video" {
		t.Fatalf("legacy filtered history = %#v", page)
	}

	statsReq := httptest.NewRequest(http.MethodGet, "/api/history/stats?type=photo", http.NoBody)
	statsRes := httptest.NewRecorder()
	s.handleHistoryStats(statsRes, statsReq)
	statsJSON := statsRes.Body.Bytes()
	var stats historyStatsResponse
	if err := json.Unmarshal(statsJSON, &stats); err != nil {
		t.Fatal(err)
	}
	if statsRes.Code != http.StatusOK || len(stats.ByType) != 1 || stats.ByType[0].MediaType != "photo" {
		t.Fatalf("history stats = %#v, status = %d", stats, statsRes.Code)
	}
	if !bytes.Contains(statsJSON, []byte(`"by_type"`)) {
		t.Fatal("history stats response must use the by_type wrapper")
	}
}

func TestHandleScheduleToggleContract(t *testing.T) {
	s, _ := newMediaTestServer(t)
	row := &store.ScheduleRow{
		ID: "s1", ChatID: 1, IntervalMin: 10, Enabled: true, CreatedAt: time.Now(),
	}
	if err := s.store.CreateSchedule(context.Background(), row); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/schedules/s1/toggle", strings.NewReader(`{"enabled":false}`))
	req.SetPathValue("id", row.ID)
	res := httptest.NewRecorder()
	s.handleScheduleToggle(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"status":"ok"`) {
		t.Fatalf("toggle response = %d %s", res.Code, res.Body.String())
	}
	rows, err := s.store.ListSchedules(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Enabled {
		t.Fatalf("schedule after toggle = %#v, err = %v", rows, err)
	}
}

func TestHandleScheduleToggleRequiresEnabled(t *testing.T) {
	s, _ := newMediaTestServer(t)
	row := &store.ScheduleRow{
		ID: "s1", ChatID: 1, IntervalMin: 10, Enabled: true, CreatedAt: time.Now(),
	}
	if err := s.store.CreateSchedule(context.Background(), row); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/schedules/s1/toggle", strings.NewReader(`{}`))
	req.SetPathValue("id", row.ID)
	res := httptest.NewRecorder()
	s.handleScheduleToggle(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("toggle status = %d, want 400 (%s)", res.Code, res.Body.String())
	}
	rows, err := s.store.ListSchedules(context.Background())
	if err != nil || len(rows) != 1 || !rows[0].Enabled {
		t.Fatalf("missing enabled field changed schedule: rows=%#v err=%v", rows, err)
	}
}

func TestHandleTasksCreateRejectsInvalidCombinations(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "zero chat id", body: `{"kind":"history","chat_id":0}`},
		{name: "negative message id", body: `{"kind":"history","chat_id":1,"message_id":-1}`},
		{name: "monitor filters", body: `{"kind":"monitor","chat_id":1,"filters":{"media_types":["photo"]}}`},
		{name: "monitor message", body: `{"kind":"monitor","chat_id":1,"message_id":10}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			res := httptest.NewRecorder()
			s.handleTasksCreate(res, httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(tt.body)))
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandleSchedulesCreateRejectsOverflowingInterval(t *testing.T) {
	body := fmt.Sprintf(`{"chat_id":1,"interval_min":%d}`, queue.MaxScheduleIntervalMin+1)
	res := httptest.NewRecorder()
	(&Server{}).handleSchedulesCreate(
		res,
		httptest.NewRequest(http.MethodPost, "/api/schedules", strings.NewReader(body)),
	)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", res.Code, res.Body.String())
	}
}

func TestHistoryPaginationValidationAndCap(t *testing.T) {
	s, _ := newMediaTestServer(t)
	// limit 与旧名 page_size 都要校验；cursor 与 sort 是 v3.2 新增的参数面
	for _, query := range []string{
		"limit=0", "limit=-1", "page_size=0", "page_size=-1",
		"cursor=abc", "cursor=1", "cursor=x.1", "cursor=1.x",
		"sort=file_name", "sort=" + url.QueryEscape("created_at; DROP TABLE history"),
	} {
		t.Run(query, func(t *testing.T) {
			res := httptest.NewRecorder()
			s.handleHistoryList(res, httptest.NewRequest(http.MethodGet, "/api/history?"+query, http.NoBody))
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", res.Code, res.Body.String())
			}
		})
	}

	res := httptest.NewRecorder()
	s.handleHistoryList(res, httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/history?page_size=%d", store.MaxHistoryPageSize+1),
		http.NoBody,
	))
	var page historyListResponse
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusOK || page.Limit != store.MaxHistoryPageSize {
		t.Fatalf("response = %d %#v, want capped limit %d", res.Code, page, store.MaxHistoryPageSize)
	}
}

// TestHistoryTotalIsOptional 锁定总数的按需语义：不带 with_total 时不返回总数，
// 使翻页不再每次都付一遍 COUNT(*) 的代价。
func TestHistoryTotalIsOptional(t *testing.T) {
	s, _ := newMediaTestServer(t)

	res := httptest.NewRecorder()
	s.handleHistoryList(res, httptest.NewRequest(http.MethodGet, "/api/history", http.NoBody))
	var withoutTotal historyListResponse
	if err := json.NewDecoder(res.Body).Decode(&withoutTotal); err != nil {
		t.Fatal(err)
	}
	if withoutTotal.Total != nil {
		t.Errorf("未请求总数却返回 %d", *withoutTotal.Total)
	}

	res = httptest.NewRecorder()
	s.handleHistoryList(res, httptest.NewRequest(http.MethodGet, "/api/history?with_total=1", http.NoBody))
	var withTotal historyListResponse
	if err := json.NewDecoder(res.Body).Decode(&withTotal); err != nil {
		t.Fatal(err)
	}
	if withTotal.Total == nil {
		t.Error("请求了 with_total=1 却没有总数")
	}
}

func TestResolvedTargetJSONContract(t *testing.T) {
	data, err := json.Marshal(tgapi.ResolvedTarget{ChatID: 1, Title: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"chat_title":"Example"`)) || bytes.Contains(data, []byte(`"title"`)) {
		t.Fatalf("resolved target JSON = %s", data)
	}
}

func TestMaskPhonePreservesUTF8(t *testing.T) {
	got := maskPhone("+一二三四五六")
	if !utf8.ValidString(got) {
		t.Fatalf("maskPhone returned invalid UTF-8: %q", got)
	}
	if got != "+一二**五六" {
		t.Fatalf("maskPhone = %q, want %q", got, "+一二**五六")
	}
}

// TestHistoryExportContract 覆盖历史导出：两种格式、筛选生效、附件头正确。
func TestHistoryExportContract(t *testing.T) {
	s, root := newMediaTestServer(t)
	addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "photo", FileName: "照片.jpg", FilePath: root + "/a.jpg",
	})
	addRecord(t, s, &store.HistoryRecord{
		ChatID: 1, MessageID: 2, MediaType: "video", FileName: "b.mp4", FilePath: root + "/b.mp4",
	})

	t.Run("csv", func(t *testing.T) {
		res := httptest.NewRecorder()
		s.handleHistoryExport(res, httptest.NewRequest(http.MethodGet, "/api/history/export", http.NoBody))
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", res.Code, res.Body.String())
		}
		if cd := res.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
			t.Errorf("Content-Disposition = %q，导出应作为附件下载", cd)
		}
		body := res.Body.Bytes()
		// UTF-8 BOM：缺了它 Excel 会把中文文件名按本地代码页解释成乱码
		if !bytes.HasPrefix(body, []byte{0xEF, 0xBB, 0xBF}) {
			t.Error("CSV 缺少 UTF-8 BOM")
		}
		rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF}))).ReadAll()
		if err != nil {
			t.Fatalf("CSV 解析失败: %v", err)
		}
		if len(rows) != 3 { // 表头 + 2 行
			t.Fatalf("CSV 行数 = %d，期望表头加两行", len(rows))
		}
		if rows[0][0] != "id" || rows[0][6] != "file_name" {
			t.Errorf("CSV 表头 = %v", rows[0])
		}
	})

	t.Run("json 且筛选生效", func(t *testing.T) {
		res := httptest.NewRecorder()
		s.handleHistoryExport(res, httptest.NewRequest(
			http.MethodGet, "/api/history/export?format=json&type=video", http.NoBody))
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", res.Code, res.Body.String())
		}
		var dtos []historyRecordDTO
		if err := json.NewDecoder(res.Body).Decode(&dtos); err != nil {
			t.Fatal(err)
		}
		if len(dtos) != 1 || dtos[0].MediaType != "video" {
			t.Fatalf("导出结果 = %#v，期望只含 video", dtos)
		}
	})

	t.Run("拒绝未知格式", func(t *testing.T) {
		res := httptest.NewRecorder()
		s.handleHistoryExport(res, httptest.NewRequest(
			http.MethodGet, "/api/history/export?format=xlsx", http.NoBody))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("status = %d，期望 400", res.Code)
		}
	})
}

// TestHistoryTaskIDFilter 覆盖任务详情下钻：按 task_id 过滤历史。
func TestHistoryTaskIDFilter(t *testing.T) {
	s, root := newMediaTestServer(t)
	addRecord(t, s, &store.HistoryRecord{
		TaskID: "task-a", ChatID: 1, MessageID: 1, MediaType: "photo",
		FileName: "a.jpg", FilePath: root + "/a.jpg",
	})
	addRecord(t, s, &store.HistoryRecord{
		TaskID: "task-b", ChatID: 1, MessageID: 2, MediaType: "photo",
		FileName: "b.jpg", FilePath: root + "/b.jpg",
	})

	res := httptest.NewRecorder()
	s.handleHistoryList(res, httptest.NewRequest(http.MethodGet, "/api/history?task_id=task-a", http.NoBody))
	var page historyListResponse
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].TaskID != "task-a" {
		t.Fatalf("按 task_id 过滤 = %#v，期望只含 task-a 的记录", page.Items)
	}
}

// TestHistoryCursorPaginationWalksAllRows 校验游标翻页不重不漏。
func TestHistoryCursorPaginationWalksAllRows(t *testing.T) {
	s, root := newMediaTestServer(t)
	const rows = 7
	for i := 1; i <= rows; i++ {
		addRecord(t, s, &store.HistoryRecord{
			ChatID: 1, MessageID: int64(i), MediaType: "photo",
			FileName: fmt.Sprintf("f%d.jpg", i), FilePath: root + "/a.jpg",
		})
	}

	seen := map[int64]bool{}
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		q := "/api/history?limit=3"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		res := httptest.NewRecorder()
		s.handleHistoryList(res, httptest.NewRequest(http.MethodGet, q, http.NoBody))
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", res.Code, res.Body.String())
		}
		var page historyListResponse
		if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			if seen[it.ID] {
				t.Errorf("id=%d 跨页重复", it.ID)
			}
			seen[it.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != rows {
		t.Errorf("游标遍历得到 %d 行，期望 %d", len(seen), rows)
	}
}

// pathTemplateClient 记录路径模板的设置调用，校验非法模板不会落到 downloader。
type pathTemplateClient struct {
	tgapi.Client
	tpl string
}

func (c *pathTemplateClient) PathTemplate() string { return c.tpl }

// settingsSnapshot 会读下面这些运行态，桩件补齐即可，取值本身与本用例无关
func (c *pathTemplateClient) DownloadPath() string     { return "/tmp" }
func (c *pathTemplateClient) ClassifyByType() bool     { return false }
func (c *pathTemplateClient) SaveMetadata() bool       { return false }
func (c *pathTemplateClient) DownloadConcurrency() int { return 1 }
func (c *pathTemplateClient) ActiveDownloadCount() int { return 0 }

func (c *pathTemplateClient) SetPathTemplate(tpl string) error {
	if !strings.Contains(tpl, "{chat_id}") {
		return errors.New("模板必须包含 {chat_id}")
	}
	c.tpl = tpl
	return nil
}

// TestSettingsPathTemplateRejectsInvalid 锁定路径模板的校验语义：
// downloader.SetPathTemplate 对非法模板会静默回退默认布局，接口层必须拦成 400，
// 否则表现为"用户改了个错模板、界面无提示、布局悄悄变回默认"。
func TestSettingsPathTemplateRejectsInvalid(t *testing.T) {
	client := &pathTemplateClient{tpl: "chat_{chat_id}/{name}"}
	s := &Server{client: client, logger: logger.New("error"), cfg: &config.Config{}}

	res := httptest.NewRecorder()
	body := `{"path_template":"没有占位符"}`
	s.handleSettingsUpdate(res, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body)))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400 (%s)", res.Code, res.Body.String())
	}
	if client.tpl != "chat_{chat_id}/{name}" {
		t.Errorf("非法模板被写入: %q", client.tpl)
	}

	res = httptest.NewRecorder()
	body = `{"path_template":"{chat_id}/{date}/{name}"}`
	s.handleSettingsUpdate(res, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200 (%s)", res.Code, res.Body.String())
	}
	if client.tpl != "{chat_id}/{date}/{name}" {
		t.Errorf("合法模板未写入: %q", client.tpl)
	}
}
