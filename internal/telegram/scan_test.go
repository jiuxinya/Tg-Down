package telegram

import (
	"context"
	"strings"
	"testing"

	tdclient "github.com/zelenin/go-tdlib/client"

	"tg-down/internal/downloader"
	mediapkg "tg-down/internal/media"
)

// fakeSearchAPI 是一个只实现扫描所需方法的 tdAPI：其余方法由内嵌的 nil 接口提供，
// 一旦扫描路径意外调用了它们就会 panic，从而暴露"本该走服务端枚举却调了别的 API"这类回归。
type fakeSearchAPI struct {
	tdAPI // nil：未预期的调用直接 panic

	// byFilter 给出每种过滤器下的消息（按新到旧排列，与 TDLib 返回顺序一致）
	byFilter map[string][]*tdclient.Message
	pageSize int

	filters    []string // 记录每次搜索用的过滤器类型名
	froms      []int64  // 记录每次搜索的 FromMessageId
	historyHit int      // GetChatHistory 被调用的次数
	historyErr error    // 非 nil 时 GetChatHistory 直接报错（避免测试陷入空页退避）
	searchFn   func(*tdclient.SearchChatMessagesRequest) (*tdclient.FoundChatMessages, error)
}

func (f *fakeSearchAPI) SearchChatMessages(
	_ context.Context, req *tdclient.SearchChatMessagesRequest,
) (*tdclient.FoundChatMessages, error) {
	if f.searchFn != nil {
		return f.searchFn(req)
	}
	key := req.Filter.SearchMessagesFilterConstructor()
	f.filters = append(f.filters, key)
	f.froms = append(f.froms, req.FromMessageId)

	msgs := f.byFilter[key]
	start := 0
	if req.FromMessageId != 0 {
		for i, m := range msgs {
			if m.Id == req.FromMessageId {
				start = i + 1 // FromMessageId 本身不重复返回
				break
			}
		}
	}
	end := min(start+f.pageSize, len(msgs))
	page := msgs[start:end]

	var next int64
	if end < len(msgs) && len(page) > 0 {
		next = page[len(page)-1].Id
	}
	return &tdclient.FoundChatMessages{Messages: page, NextFromMessageId: next}, nil
}

func TestScanHistoryPages_DoesNotTreatProgressingEmptyPagesAsComplete(t *testing.T) {
	c := newTestClient(t)
	call := 0
	fake := &fakeSearchAPI{searchFn: func(req *tdclient.SearchChatMessagesRequest) (*tdclient.FoundChatMessages, error) {
		call++
		switch call {
		case 1:
			return &tdclient.FoundChatMessages{NextFromMessageId: 300}, nil
		case 2:
			if req.FromMessageId != 300 {
				t.Fatalf("第 2 页游标 = %d, want 300", req.FromMessageId)
			}
			return &tdclient.FoundChatMessages{NextFromMessageId: 200}, nil
		case 3:
			return &tdclient.FoundChatMessages{NextFromMessageId: 100}, nil
		default:
			return &tdclient.FoundChatMessages{Messages: []*tdclient.Message{{
				Id: 50, Date: 1700000000,
				Content: &tdclient.MessageSticker{Sticker: &tdclient.Sticker{
					Format: &tdclient.StickerFormatWebp{}, Sticker: &tdclient.File{Id: 5, Size: 5},
				}},
			}}}, nil
		}
	}}
	var got []*downloader.MediaInfo
	res, err := c.scanHistoryPages(
		context.Background(), fake, &downloader.HistorySpec{ChatID: 1}, collectDispatch(&got),
	)
	if err != nil {
		t.Fatalf("scanHistoryPages() error = %v", err)
	}
	if call != 4 {
		t.Fatalf("搜索调用次数 = %d, want 4", call)
	}
	if len(got) != 1 || got[0].MessageID != 50 || res.foundMedia != 1 {
		t.Fatalf("连续空页后的媒体未被扫描: got=%+v result=%+v", got, res)
	}
}

func TestSearchChatMessagesRejectsNilResponseAndStalledCursor(t *testing.T) {
	c := newTestClient(t)
	t.Run("nil response", func(t *testing.T) {
		fake := &fakeSearchAPI{searchFn: func(*tdclient.SearchChatMessagesRequest) (*tdclient.FoundChatMessages, error) {
			return nil, nil
		}}
		_, err := c.searchChatMessagesRequest(context.Background(), fake, &tdclient.SearchChatMessagesRequest{})
		if err == nil {
			t.Fatal("TDLib 空响应应返回错误")
		}
	})

	t.Run("stalled cursor", func(t *testing.T) {
		fake := &fakeSearchAPI{searchFn: func(*tdclient.SearchChatMessagesRequest) (*tdclient.FoundChatMessages, error) {
			return &tdclient.FoundChatMessages{NextFromMessageId: 99}, nil
		}}
		_, err := c.scanBySearch(
			context.Background(), fake, &downloader.HistorySpec{ChatID: 1},
			&tdclient.SearchMessagesFilterEmpty{}, 99, 10, collectDispatch(nil), scanOutcome{}, true,
		)
		if err == nil || !strings.Contains(err.Error(), "游标未推进") {
			t.Fatalf("停滞游标错误 = %v", err)
		}
	})
}

func (f *fakeSearchAPI) GetChatHistory(
	_ context.Context, _ *tdclient.GetChatHistoryRequest,
) (*tdclient.Messages, error) {
	f.historyHit++
	return &tdclient.Messages{}, f.historyErr
}

// videoMessages 生成 n 条视频消息，id 从 n*10 递减到 10（新到旧）
func videoMessages(n int) []*tdclient.Message {
	msgs := make([]*tdclient.Message, 0, n)
	for i := n; i >= 1; i-- {
		msgs = append(msgs, &tdclient.Message{
			Id:   int64(i) * 10,
			Date: 1700000000,
			Content: &tdclient.MessageVideo{
				Video: &tdclient.Video{
					FileName: "v.mp4",
					MimeType: "video/mp4",
					Video:    &tdclient.File{Id: int32(i), Size: 1024},
				},
			},
		})
	}
	return msgs
}

// videoFake 构造一个只含 n 条视频消息的 fake
func videoFake(n, pageSize int) *fakeSearchAPI {
	return &fakeSearchAPI{
		byFilter: map[string][]*tdclient.Message{
			tdclient.ConstructorSearchMessagesFilterVideo: videoMessages(n),
		},
		pageSize: pageSize,
	}
}

// collectDispatch 收集扫描分发出的媒体而不真正下载
func collectDispatch(out *[]*downloader.MediaInfo) func(*downloader.MediaInfo) error {
	return func(mi *downloader.MediaInfo) error {
		*out = append(*out, mi)
		return nil
	}
}

// TestScanHistoryPages_UsesServerSearch 断言单一媒体类型走服务端枚举而非全量翻页，
// 并且结束条件取自 NextFromMessageId == 0（不再依赖"连续空页"启发式）。
func TestScanHistoryPages_UsesServerSearch(t *testing.T) {
	c := newTestClient(t)
	fake := videoFake(25, 10)

	spec := &downloader.HistorySpec{
		ChatID:  1,
		Filters: downloader.HistoryFilters{MediaTypes: []string{mediapkg.Video}},
	}
	var got []*downloader.MediaInfo
	res, err := c.scanHistoryPages(context.Background(), fake, spec, collectDispatch(&got))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	if fake.historyHit != 0 {
		t.Errorf("单类型任务不应回退到 GetChatHistory，实际调用 %d 次", fake.historyHit)
	}
	if len(got) != 25 {
		t.Errorf("发现媒体数 = %d，期望 25", len(got))
	}
	if res.scannedMessages != 25 {
		t.Errorf("扫描消息数 = %d，期望 25", res.scannedMessages)
	}
	if res.maxMessageID != 250 {
		t.Errorf("水位 = %d，期望 250（最新消息 id）", res.maxMessageID)
	}
	want := []string{
		tdclient.ConstructorSearchMessagesFilterVideo,
		tdclient.ConstructorSearchMessagesFilterVideo,
		tdclient.ConstructorSearchMessagesFilterVideo,
	}
	if len(fake.filters) != len(want) {
		t.Fatalf("搜索页数 = %d，期望 3", len(fake.filters))
	}
	for i, f := range fake.filters {
		if f != want[i] {
			t.Errorf("第 %d 页过滤器 = %s，期望 %s", i, f, want[i])
		}
	}
}

// TestScanHistoryPages_DownloadAllUsesOneUnfilteredPipeline 断言"下载全部"使用一条无过滤
// 搜索流水线。默认类型含贴纸，拆成专用过滤器会漏贴纸；单条完整历史流水线也不会重复消息。
func TestScanHistoryPages_DownloadAllUsesOneUnfilteredPipeline(t *testing.T) {
	c := newTestClient(t)
	fake := &fakeSearchAPI{
		byFilter: map[string][]*tdclient.Message{
			tdclient.ConstructorSearchMessagesFilterEmpty: {
				{
					Id: 30, Date: 1700000000,
					Content: &tdclient.MessageVideo{Video: &tdclient.Video{
						FileName: "v.mp4", MimeType: "video/mp4", Video: &tdclient.File{Id: 3, Size: 30},
					}},
				},
				{
					Id: 20, Date: 1700000000,
					Content: &tdclient.MessageSticker{Sticker: &tdclient.Sticker{
						Format: &tdclient.StickerFormatWebp{}, Sticker: &tdclient.File{Id: 2, Size: 20},
					}},
				},
				{Id: 10, Date: 1700000000, Content: &tdclient.MessageText{}},
			},
		},
		pageSize: 2,
	}

	spec := &downloader.HistorySpec{ChatID: 1} // 无 MediaTypes = 下载全部
	var got []*downloader.MediaInfo
	res, err := c.scanHistoryPages(context.Background(), fake, spec, collectDispatch(&got))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	if fake.historyHit != 0 {
		t.Errorf("下载全部不应调用 GetChatHistory，实际调用 %d 次", fake.historyHit)
	}
	if len(fake.filters) != 2 { // 三条消息分两页
		t.Fatalf("搜索页数 = %d，期望 2", len(fake.filters))
	}
	for _, filter := range fake.filters {
		if filter != tdclient.ConstructorSearchMessagesFilterEmpty {
			t.Errorf("下载全部使用了 %s，期望无过滤搜索", filter)
		}
	}
	if len(got) != 2 || res.foundMedia != 2 || res.scannedMessages != 3 {
		t.Fatalf("发现媒体/扫描消息 = %d/%d/%d，期望 2/2/3", len(got), res.foundMedia, res.scannedMessages)
	}
	if got[0].MessageID != 30 || got[1].MessageID != 20 {
		t.Errorf("分发消息 = [%d %d]，期望视频 30、贴纸 20 各一次", got[0].MessageID, got[1].MessageID)
	}
}

// TestScanHistoryPages_IncrementalStopsAtWatermark 是增量扫描的核心回归：
// 定时计划第二次触发时带上上次的水位，扫描必须在追上水位处收手，而不是把整条历史重扫一遍。
func TestScanHistoryPages_IncrementalStopsAtWatermark(t *testing.T) {
	c := newTestClient(t)
	fake := videoFake(25, 10)

	// 上次扫到 id=200（即 25 条中最新的 5 条是新增的）
	spec := &downloader.HistorySpec{
		ChatID:          1,
		Filters:         downloader.HistoryFilters{MediaTypes: []string{mediapkg.Video}},
		StopAtMessageID: 200,
	}
	var got []*downloader.MediaInfo
	res, err := c.scanHistoryPages(context.Background(), fake, spec, collectDispatch(&got))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	if len(got) != 5 {
		t.Errorf("发现媒体数 = %d，期望 5（只有水位之上的新消息）", len(got))
	}
	if res.scannedMessages != 5 {
		t.Errorf("扫描消息数 = %d，期望 5", res.scannedMessages)
	}
	if res.maxMessageID != 250 {
		t.Errorf("新水位 = %d，期望 250", res.maxMessageID)
	}
	if len(fake.filters) != 1 {
		t.Errorf("搜索页数 = %d，期望 1（追上水位即停，不再翻页）", len(fake.filters))
	}
	for _, m := range got {
		if m.MessageID <= 200 {
			t.Errorf("消息 %d 已在上次扫描中处理过，不应重复分发", m.MessageID)
		}
	}
}

// TestScanHistoryPages_UsesUnfilteredSearchWithoutServerFilter 断言贴纸任务使用无过滤搜索，
// 再在本地筛选；不再依赖 GetChatHistory 的重复空页启发式结束条件。
func TestScanHistoryPages_UsesUnfilteredSearchWithoutServerFilter(t *testing.T) {
	c := newTestClient(t)
	fake := &fakeSearchAPI{
		byFilter: map[string][]*tdclient.Message{
			tdclient.ConstructorSearchMessagesFilterEmpty: {{
				Id: 10, Date: 1700000000,
				Content: &tdclient.MessageSticker{Sticker: &tdclient.Sticker{
					Format: &tdclient.StickerFormatWebp{}, Sticker: &tdclient.File{Id: 1, Size: 10},
				}},
			}},
		},
		pageSize: 10,
	}

	spec := &downloader.HistorySpec{
		ChatID:  1,
		Filters: downloader.HistoryFilters{MediaTypes: []string{"sticker"}},
	}
	var got []*downloader.MediaInfo
	_, err := c.scanHistoryPages(context.Background(), fake, spec, collectDispatch(&got))
	if err != nil {
		t.Fatalf("扫描贴纸失败: %v", err)
	}
	if fake.historyHit != 0 {
		t.Errorf("不应调用 GetChatHistory，实际调用 %d 次", fake.historyHit)
	}
	if len(fake.filters) != 1 || fake.filters[0] != tdclient.ConstructorSearchMessagesFilterEmpty {
		t.Errorf("搜索过滤器 = %v，期望一条无过滤搜索", fake.filters)
	}
	if len(got) != 1 || got[0].MediaType != mediapkg.Sticker {
		t.Errorf("贴纸分发结果 = %+v，期望一条贴纸", got)
	}
}

// TestSearchFiltersFor 覆盖服务端过滤器的映射边界
func TestSearchFiltersFor(t *testing.T) {
	tests := []struct {
		name  string
		types []string
		want  int // 期望的过滤器条数；0 = 无法走服务端枚举
	}{
		{"单一 video", []string{mediapkg.Video}, 1},
		{"photo+video", []string{mediapkg.Photo, mediapkg.Video}, 2},
		{"默认类型集含贴纸，需完整历史", mediapkg.DefaultTypes, 0},
		{"含贴纸（无服务端过滤器）", []string{mediapkg.Photo, mediapkg.Sticker}, 0},
		{"空类型列表", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filters, ok := searchFiltersFor(tt.types)
			if ok != (tt.want > 0) {
				t.Fatalf("ok = %v，期望 %v", ok, tt.want > 0)
			}
			if len(filters) != tt.want {
				t.Errorf("过滤器条数 = %d，期望 %d", len(filters), tt.want)
			}
		})
	}
}

// TestEffectiveTypes 断言"未指定类型"展开为真正的全部类型集（包括贴纸）。
func TestEffectiveTypes(t *testing.T) {
	if got := effectiveTypes(nil); len(got) != len(mediapkg.DefaultTypes) {
		t.Errorf("未指定类型时展开为 %v，期望默认类型集 %v", got, mediapkg.DefaultTypes)
	}
	if got := effectiveTypes([]string{mediapkg.Video}); len(got) != 1 {
		t.Errorf("显式类型不应被改写，实际 %v", got)
	}
}

func TestTrimAtStop(t *testing.T) {
	msgs := []*tdclient.Message{{Id: 50}, {Id: 40}, {Id: 30}, {Id: 20}, {Id: 10}}
	tests := []struct {
		name    string
		stopAt  int64
		wantLen int
		reached bool
	}{
		{"无水位", 0, 5, false},
		{"水位在页中", 30, 2, true},
		{"水位低于整页", 5, 5, false},
		{"水位高于整页", 100, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kept, reached := trimAtStop(msgs, tt.stopAt)
			if len(kept) != tt.wantLen {
				t.Errorf("保留 %d 条，期望 %d", len(kept), tt.wantLen)
			}
			if reached != tt.reached {
				t.Errorf("reached = %v，期望 %v", reached, tt.reached)
			}
		})
	}
}
