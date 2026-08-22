package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	tdclient "github.com/zelenin/go-tdlib/client"

	"tg-down/internal/downloader"
	mediapkg "tg-down/internal/media"
)

type fakeGetMessageAPI struct {
	tdAPI
	messages map[int64]*tdclient.Message
	errs     map[int64]error
}

func (f *fakeGetMessageAPI) GetMessage(
	_ context.Context, req *tdclient.GetMessageRequest,
) (*tdclient.Message, error) {
	if err := f.errs[req.MessageId]; err != nil {
		return nil, err
	}
	return f.messages[req.MessageId], nil
}

func TestRetryInterruptedMessagesDispatchesValidAndReportsUnresolved(t *testing.T) {
	c := newTestClient(t)
	fake := &fakeGetMessageAPI{
		messages: map[int64]*tdclient.Message{
			1: {
				Id: 1, ChatId: 100, Date: 1700000000,
				Content: &tdclient.MessageDocument{Document: &tdclient.Document{
					FileName: "ok.bin", Document: &tdclient.File{Id: 1, Size: 10},
				}},
			},
			3: {Id: 3, ChatId: 100, Content: &tdclient.MessageText{}},
			4: {
				Id: 4, ChatId: 100,
				Content: &tdclient.MessagePhoto{Photo: &tdclient.Photo{Sizes: []*tdclient.PhotoSize{{
					Photo: &tdclient.File{Id: 4, Size: 4},
				}}}},
			},
		},
		errs: map[int64]error{2: errors.New("message not found")},
	}
	spec := &downloader.HistorySpec{
		ChatID: 100, TaskID: "retry", RetryOnly: true,
		RetryMessageIDs: []int64{1, 2, 3, 4},
		Filters:         downloader.HistoryFilters{MediaTypes: []string{mediapkg.Document}},
	}
	var got []*downloader.MediaInfo
	err := c.retryInterruptedMessages(context.Background(), fake, spec, collectDispatch(&got))
	if err == nil {
		t.Fatal("未恢复的消息应使 RetryOnly 返回错误")
	}
	if len(got) != 1 || got[0].MessageID != 1 {
		t.Fatalf("有效重试消息未分发: %+v", got)
	}
	for _, id := range []string{"2", "3", "4"} {
		if !strings.Contains(err.Error(), "消息 "+id) {
			t.Errorf("汇总错误未包含消息 %s: %v", id, err)
		}
	}
}
