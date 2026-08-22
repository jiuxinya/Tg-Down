package downloader

import (
	"math"
	"testing"
	"time"

	mediapkg "tg-down/internal/media"
)

func mediaAt(mediaType string, date time.Time, size int64) *MediaInfo {
	return &MediaInfo{MediaType: mediaType, Date: date, FileSize: size}
}

// TestHistoryFilters_Match 覆盖客户端复核的全部维度。
//
// 服务端枚举已按类型/关键词/发送者过滤过一轮，但回退的全量翻页路径没有，
// 因此这套判定必须自洽——否则"换条扫描路径结果就不一样"。
func TestHistoryFilters_Match(t *testing.T) {
	now := time.Unix(1700000000, 0)
	base := func() *MediaInfo {
		mi := mediaAt(mediapkg.Video, now, 1000)
		mi.Caption = "季度报告 Q3"
		mi.FileName = "report.mp4"
		mi.SenderID = 42
		return mi
	}

	tests := []struct {
		name    string
		filters HistoryFilters
		mi      *MediaInfo
		want    bool
	}{
		{"零值过滤器放行一切", HistoryFilters{}, base(), true},
		{"nil 媒体不通过", HistoryFilters{}, nil, false},

		{"类型命中", HistoryFilters{MediaTypes: []string{mediapkg.Video}}, base(), true},
		{"类型未命中", HistoryFilters{MediaTypes: []string{mediapkg.Photo}}, base(), false},

		{"日期下界之前被排除", HistoryFilters{DateFrom: now.Unix() + 1}, base(), false},
		{"日期下界之上通过", HistoryFilters{DateFrom: now.Unix()}, base(), true},
		{"日期上界之后被排除", HistoryFilters{DateTo: now.Unix() - 1}, base(), false},
		{"日期上界之内通过", HistoryFilters{DateTo: now.Unix()}, base(), true},

		{"超过大小上限被排除", HistoryFilters{MaxFileSize: 999}, base(), false},
		{"等于大小上限通过", HistoryFilters{MaxFileSize: 1000}, base(), true},

		{"发送者匹配", HistoryFilters{SenderID: 42}, base(), true},
		{"发送者不匹配", HistoryFilters{SenderID: 43}, base(), false},

		{"关键词命中 caption", HistoryFilters{Query: "季度"}, base(), true},
		{"关键词命中文件名", HistoryFilters{Query: "report"}, base(), true},
		{"关键词大小写不敏感", HistoryFilters{Query: "REPORT"}, base(), true},
		{"关键词未命中", HistoryFilters{Query: "发票"}, base(), false},

		{
			"多条件需全部满足",
			HistoryFilters{MediaTypes: []string{mediapkg.Video}, SenderID: 42, Query: "报告"},
			base(), true,
		},
		{
			"多条件中有一条不满足即排除",
			HistoryFilters{MediaTypes: []string{mediapkg.Video}, SenderID: 99, Query: "报告"},
			base(), false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.filters.Match(tt.mi); got != tt.want {
				t.Errorf("Match() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHistoryFilters_IsZero 保证新增字段被纳入零值判定：
// 漏掉一个字段会让带该过滤条件的任务被当成"不过滤"，filters 列存成 NULL，重启后过滤条件凭空消失。
func TestHistoryFilters_IsZero(t *testing.T) {
	if !(HistoryFilters{}).IsZero() {
		t.Error("零值过滤器 IsZero() = false")
	}
	nonZero := []HistoryFilters{
		{MediaTypes: []string{mediapkg.Photo}},
		{DateFrom: 1},
		{DateTo: 1},
		{MaxFileSize: 1},
		{Query: "x"},
		{SenderID: 1},
	}
	for _, f := range nonZero {
		if f.IsZero() {
			t.Errorf("非零过滤器 %+v 的 IsZero() = true（该字段会在持久化时丢失）", f)
		}
	}
}

func TestHistoryFilters_Validate(t *testing.T) {
	tests := []struct {
		name    string
		filters HistoryFilters
		wantErr bool
	}{
		{"零值合法", HistoryFilters{}, false},
		{"合法类型", HistoryFilters{MediaTypes: []string{mediapkg.Photo, mediapkg.Sticker}}, false},
		{"非法类型", HistoryFilters{MediaTypes: []string{"webp"}}, true},
		{"日期下界不能为负", HistoryFilters{DateFrom: -1}, true},
		{"日期上界不能为负", HistoryFilters{DateTo: -1}, true},
		{"日期下界允许 int32 最大值", HistoryFilters{DateFrom: math.MaxInt32}, false},
		{"日期上界允许 int32 最大值", HistoryFilters{DateTo: math.MaxInt32}, false},
		{"日期下界超过 int32", HistoryFilters{DateFrom: int64(math.MaxInt32) + 1}, true},
		{"日期上界超过 int32", HistoryFilters{DateTo: int64(math.MaxInt32) + 1}, true},
		{"日期区间倒置", HistoryFilters{DateFrom: 200, DateTo: 100}, true},
		{"日期区间正常", HistoryFilters{DateFrom: 100, DateTo: 200}, false},
		{"负数大小上限", HistoryFilters{MaxFileSize: -1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if gotErr := tt.filters.Validate() != ""; gotErr != tt.wantErr {
				t.Errorf("Validate() 报错 = %v, want %v（%q）", gotErr, tt.wantErr, tt.filters.Validate())
			}
		})
	}
}
