package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tg-down/internal/logger"
)

func TestNextBackoffCaps(t *testing.T) {
	cases := []struct{ in, want time.Duration }{
		{initialReconnect, 4 * time.Second},
		{4 * time.Second, 8 * time.Second},
		{40 * time.Second, maxReconnect},
		{maxReconnect, maxReconnect},
	}
	for _, c := range cases {
		if got := nextBackoff(c.in); got != c.want {
			t.Errorf("nextBackoff(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestConsumeEventsReportsEstablished 是退避重置的判据：只有真正拿到 200 才算连上过。
// 少了这个信号，一次瞬时抖动会把重连间隔永久顶在 60s。
func TestConsumeEventsReportsEstablished(t *testing.T) {
	log := logger.New("error")

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 立即结束流，模拟一次瞬时断开
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	if established, _ := consumeEvents(context.Background(), ok.Client(), ok.URL, log); !established {
		t.Error("200 响应应记为已建立事件流")
	}
	if established, _ := consumeEvents(context.Background(), bad.Client(), bad.URL, log); established {
		t.Error("500 响应不应记为已建立事件流")
	}
}

// TestScanEventsJoinsMultilineData SSE 规范里一帧可以有多条 data: 行，
// 覆盖式赋值只会留下最后一行，整帧再也解不出来。
func TestScanEventsJoinsMultilineData(t *testing.T) {
	stream := "event: task\n" +
		"data: {\"id\":\"t1\",\n" +
		"data:  \"status\":\"failed\"}\n" +
		"\n" +
		": 这是注释\n" +
		"event: state\n" +
		"data: {\"x\":1}\n" +
		"\n"

	type frame struct{ event, data string }
	var frames []frame
	err := scanEvents(strings.NewReader(stream), func(e, d string) {
		if e != "" {
			frames = append(frames, frame{e, d})
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("帧数 = %d, want 2: %+v", len(frames), frames)
	}
	want := "{\"id\":\"t1\",\n\"status\":\"failed\"}"
	if frames[0].event != "task" || frames[0].data != want {
		t.Errorf("首帧 = %+v, want data %q", frames[0], want)
	}
}

// TestHandleTaskEventCountsBadFrames 坏帧此前被静默丢弃，一条通知都不弹时无从判断问题在哪层
func TestHandleTaskEventCountsBadFrames(t *testing.T) {
	before := badFrames.Load()
	handleTaskEvent("{ 这不是 JSON", logger.New("error"))
	if got := badFrames.Load(); got != before+1 {
		t.Errorf("坏帧计数 = %d, want %d", got, before+1)
	}
}
