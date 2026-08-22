package telegram

import (
	"testing"

	mediapkg "tg-down/internal/media"
)

// TestServerFilterCoverage 强制 historyCountFilters 与 media 词汇源保持同步。
//
// 历史上媒体类型的定义散落在 9 处，新增一种类型时漏改服务端过滤器表会让 expectedTotal
// 少估，前端进度条随之超过 100%——而且没有任何测试会发现。此处把这条约束固化下来：
// media 包声明可服务端枚举的类型，必须在此有过滤器；反之不可枚举的类型不得出现在表中。
func TestServerFilterCoverage(t *testing.T) {
	for _, mt := range mediapkg.AllTypes {
		_, hasFilter := historyCountFilters[mt]
		wantFilter := mediapkg.HasServerFilter(mt)

		switch {
		case wantFilter && !hasFilter:
			t.Errorf("媒体类型 %q 声明可服务端枚举，但 historyCountFilters 中没有对应的 SearchMessagesFilter", mt)
		case !wantFilter && hasFilter:
			t.Errorf("媒体类型 %q 声明不可服务端枚举，但 historyCountFilters 中却有过滤器", mt)
		}
	}

	for mt := range historyCountFilters {
		if !mediapkg.IsValid(mt) {
			t.Errorf("historyCountFilters 中的 %q 不是 media 包认可的合法类型", mt)
		}
	}
}
