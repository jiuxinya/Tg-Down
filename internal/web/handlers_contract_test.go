package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

	listReq := httptest.NewRequest(http.MethodGet, "/api/history?type=photo", nil)
	listRes := httptest.NewRecorder()
	s.handleHistoryList(listRes, listReq)
	var page historyListResponse
	if err := json.NewDecoder(listRes.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if listRes.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].MediaType != "photo" {
		t.Fatalf("filtered history = %#v, status = %d", page, listRes.Code)
	}

	legacyReq := httptest.NewRequest(http.MethodGet, "/api/history?media_type=video", nil)
	legacyRes := httptest.NewRecorder()
	s.handleHistoryList(legacyRes, legacyReq)
	page = historyListResponse{}
	if err := json.NewDecoder(legacyRes.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].MediaType != "video" {
		t.Fatalf("legacy filtered history = %#v", page)
	}

	statsReq := httptest.NewRequest(http.MethodGet, "/api/history/stats?type=photo", nil)
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
	for _, query := range []string{"page=0", "page=-1", "page_size=0", "page_size=-1"} {
		t.Run(query, func(t *testing.T) {
			res := httptest.NewRecorder()
			s.handleHistoryList(res, httptest.NewRequest(http.MethodGet, "/api/history?"+query, nil))
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", res.Code, res.Body.String())
			}
		})
	}

	res := httptest.NewRecorder()
	s.handleHistoryList(res, httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/history?page_size=%d", store.MaxHistoryPageSize+1),
		nil,
	))
	var page historyListResponse
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusOK || page.PageSize != store.MaxHistoryPageSize {
		t.Fatalf("response = %d %#v, want capped page size %d", res.Code, page, store.MaxHistoryPageSize)
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
