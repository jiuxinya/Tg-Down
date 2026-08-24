package store

import (
	"context"
	"testing"
	"time"
)

// seedNames 灌入若干条历史，MessageID 按序递增
func seedNames(t *testing.T, s *Store, names ...string) {
	t.Helper()
	ctx := context.Background()
	for i, name := range names {
		if err := s.UpsertHistoryStart(ctx, &HistoryRecord{
			ChatID: 1, MessageID: int64(i + 1), MediaType: "document", FileName: name,
			FilePath: "/tmp/" + name, FileSize: int64(i + 1), Status: HistoryStatusQueued,
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func searchNames(t *testing.T, s *Store, query string) []string {
	t.Helper()
	page, err := s.QueryHistory(context.Background(), &HistoryFilter{Query: query, Limit: 50})
	if err != nil {
		t.Fatalf("搜索 %q 失败: %v", query, err)
	}
	names := make([]string, 0, len(page.Items))
	for _, rec := range page.Items {
		names = append(names, rec.FileName)
	}
	return names
}

// TestHistorySearch_FTSSemantics 覆盖文件名搜索的行为面。
//
// 换到 FTS5 后语义从"子串匹配"变成"词元前缀匹配"，因此这里显式钉住每一类输入的结果，
// 包括那些必须交给 MATCH 之外处理的输入：FTS5 会把 "、*、AND/OR/NOT 当成查询语法，
// 原样传入轻则查不到、重则语法错误返回 500。
func TestHistorySearch_FTSSemantics(t *testing.T) {
	s := newTestStore(t)
	seedNames(t, s,
		"holiday_video.mp4", "report final.pdf", "中文文件名.jpg",
		"AND OR NOT.txt", `quote"inside.txt`, "UPPERCASE.TXT",
	)

	for _, tt := range []struct {
		name  string
		query string
		want  []string
	}{
		{"完整词元", "holiday", []string{"holiday_video.mp4"}},
		{"前缀匹配", "vid", []string{"holiday_video.mp4"}},
		{"下划线分词", "video", []string{"holiday_video.mp4"}},
		{"扩展名", "pdf", []string{"report final.pdf"}},
		{"CJK", "中文", []string{"中文文件名.jpg"}},
		{"大小写不敏感", "uppercase", []string{"UPPERCASE.TXT"}},
		{"FTS 运算符当字面量", "AND", []string{"AND OR NOT.txt"}},
		{"输入含双引号不报错", `quote"inside`, []string{`quote"inside.txt`}},
		{"多词全部命中才算", "report final", []string{"report final.pdf"}},
		{"无匹配", "nonexistent", nil},
		{"纯空白", "   ", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := searchNames(t, s, tt.query)
			if len(got) != len(tt.want) {
				t.Fatalf("搜索 %q = %v，期望 %v", tt.query, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("搜索 %q 第 %d 项 = %q，期望 %q", tt.query, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestHistorySearch_IndexStaysInSync 校验三条写路径都会同步全文索引。
//
// 触发器漏掉任何一面都会让索引与数据发散：漏 UPDATE 则重扫改名后的文件搜不到，
// 漏 DELETE 则删掉的行仍能被搜出来（随后按 id 取详情时 404）。
func TestHistorySearch_IndexStaysInSync(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedNames(t, s, "before_rename.mp4")

	// UPSERT 走 ON CONFLICT DO UPDATE 改名
	if err := s.UpsertHistoryStart(ctx, &HistoryRecord{
		ChatID: 1, MessageID: 1, MediaType: "document", FileName: "after_rename.mp4",
		FilePath: "/tmp/x", FileSize: 1, Status: HistoryStatusQueued, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := searchNames(t, s, "after"); len(got) != 1 {
		t.Errorf("改名后按新名搜索得到 %v，期望命中", got)
	}
	if got := searchNames(t, s, "before"); len(got) != 0 {
		t.Errorf("改名后旧名仍可搜到: %v", got)
	}

	// 不涉及 file_name 的更新不应影响索引（触发器带 WHEN 条件）
	if err := s.SetHistoryThumb(ctx, 1, 1, "/tmp/thumb.jpg"); err != nil {
		t.Fatal(err)
	}
	if got := searchNames(t, s, "after"); len(got) != 1 {
		t.Errorf("更新缩略图后搜索失效: %v", got)
	}

	// 删除后不应再命中
	if _, err := s.db.ExecContext(ctx, `DELETE FROM history WHERE chat_id=1 AND message_id=1`); err != nil {
		t.Fatal(err)
	}
	if got := searchNames(t, s, "after"); len(got) != 0 {
		t.Errorf("删除后仍可搜到: %v", got)
	}
}

// TestHistorySearch_PunctuationFallsBackToLike 锁定纯标点查询的回退路径。
// unicode61 分词器不索引标点，若不回退 LIKE，"按文件名里的字面标点搜索"这一能力会消失。
func TestHistorySearch_PunctuationFallsBackToLike(t *testing.T) {
	s := newTestStore(t)
	seedNames(t, s, "literal%percent.txt", "literalXpercent.txt", "under_score.txt")

	if got := searchNames(t, s, "%"); len(got) != 1 || got[0] != "literal%percent.txt" {
		t.Errorf("搜索 %%（LIKE 通配符）= %v，期望仅字面命中", got)
	}
	// 关键：元字符不得被当作通配符把所有行都匹配出来
	if got := searchNames(t, s, "%"); len(got) == 3 {
		t.Error("LIKE 元字符被当成通配符，匹配了全部行")
	}
}

// TestHistorySearch_MigrationBackfillsExistingRows 校验升级老库时既有行被回填进索引，
// 否则升级前下载的文件全部搜不到。
func TestHistorySearch_MigrationBackfillsExistingRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedNames(t, s, "preexisting_file.mp4")

	// 模拟"索引缺失的老库"：清空 FTS 后重跑迁移步骤
	if _, err := s.db.ExecContext(ctx, `INSERT INTO history_fts(history_fts) VALUES ('delete-all')`); err != nil {
		t.Fatal(err)
	}
	if got := searchNames(t, s, "preexisting"); len(got) != 0 {
		t.Fatalf("清空索引后仍能搜到 %v，测试前提不成立", got)
	}
	if err := migrateHistoryFTS(ctx, s.db); err != nil {
		t.Fatalf("回填索引失败: %v", err)
	}
	if got := searchNames(t, s, "preexisting"); len(got) != 1 {
		t.Errorf("迁移未回填既有行，搜索得到 %v", got)
	}
}

// TestHistoryStats_CacheInvalidatedOnWrite 校验统计缓存在写入后失效。
// 缓存本身是为了让翻页不再重复扫表，但下载进行中读到旧数字会让用户以为统计坏了。
func TestHistoryStats_CacheInvalidatedOnWrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedNames(t, s, "a.txt")

	first, err := s.HistoryStats(ctx, &HistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Count != 1 {
		t.Fatalf("初始统计 = %+v，期望 1 条计数 1", first)
	}
	// 命中缓存：结果不变
	if again, _ := s.HistoryStats(ctx, &HistoryFilter{}); len(again) != 1 || again[0].Count != 1 {
		t.Fatalf("缓存命中后统计 = %+v，期望不变", again)
	}

	seedNames(t, s, "a.txt", "b.txt") // 再写入一条（b.txt 为新行）
	after, err := s.HistoryStats(ctx, &HistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Count != 2 {
		t.Errorf("写入后统计 = %+v，期望计数 2（缓存未失效）", after)
	}
}

// TestHistoryStats_CacheKeyedByFilter 校验不同筛选条件不会互相串用缓存。
func TestHistoryStats_CacheKeyedByFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedNames(t, s, "a.txt", "b.txt")

	all, err := s.HistoryStats(ctx, &HistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Count != 2 {
		t.Fatalf("无筛选统计 = %+v，期望计数 2", all)
	}
	// 换一个筛选条件：不得命中上一条缓存
	filtered, err := s.HistoryStats(ctx, &HistoryFilter{Query: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Count != 1 {
		t.Errorf("带筛选统计 = %+v，期望计数 1（缓存串用了无筛选的结果）", filtered)
	}
}

// TestUpdateHistoryResultWithThumb_SingleStatement 校验终态与缩略图路径在一条语句里写入。
func TestUpdateHistoryResultWithThumb_SingleStatement(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedNames(t, s, "clip.mp4")

	if err := s.UpdateHistoryResultWithThumb(ctx, 1, 1,
		HistoryStatusCompleted, "", "/tmp/clip.mp4", "/tmp/.thumbs/u1.jpg"); err != nil {
		t.Fatal(err)
	}
	rec, err := s.GetHistoryByID(ctx, 1)
	if err != nil || rec == nil {
		t.Fatalf("GetHistoryByID() = %+v, %v", rec, err)
	}
	if rec.Status != HistoryStatusCompleted {
		t.Errorf("状态 = %q，期望 completed", rec.Status)
	}
	if rec.ThumbPath != "/tmp/.thumbs/u1.jpg" {
		t.Errorf("缩略图路径 = %q，期望一并写入", rec.ThumbPath)
	}
	if rec.FinishedAt == nil {
		t.Error("终态未记录 finished_at")
	}

	// thumbPath 为空时保持原值不覆盖，失败/跳过路径才能共用这条语句
	if err := s.UpdateHistoryResultWithThumb(ctx, 1, 1,
		HistoryStatusCompleted, "", "/tmp/clip.mp4", ""); err != nil {
		t.Fatal(err)
	}
	rec, _ = s.GetHistoryByID(ctx, 1)
	if rec.ThumbPath != "/tmp/.thumbs/u1.jpg" {
		t.Errorf("空 thumbPath 覆盖了原值: %q", rec.ThumbPath)
	}
}
