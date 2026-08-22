package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// seedBulkHistory 批量灌入 n 条历史记录（单事务，绕过 UpsertHistoryStart 的逐条开销）
func seedBulkHistory(tb testing.TB, s *Store, n int) {
	tb.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO history (task_id, chat_id, chat_title, message_id, media_type, file_name, file_path,
                     file_size, mime_type, status, created_at, unique_id, album_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`)
	if err != nil {
		tb.Fatal(err)
	}
	types := []string{"photo", "video", "document", "animation", "audio", "voice"}
	statuses := []string{"completed", "failed", "skipped"}
	base := time.Now().Add(-time.Duration(n) * time.Second).Unix()
	for i := range n {
		_, err := stmt.ExecContext(ctx,
			fmt.Sprintf("t%d", i%50), int64(i%20), "chat", int64(i),
			types[i%len(types)], fmt.Sprintf("file_%06d_%s.bin", i, types[i%len(types)]),
			fmt.Sprintf("/d/%d.bin", i), int64(i*1024), "application/octet-stream",
			statuses[i%len(statuses)], base+int64(i), fmt.Sprintf("u%d", i),
		)
		if err != nil {
			tb.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		tb.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

// openSeeded 打开一个落盘库并灌入 n 行（内存库无法反映真实的索引/IO 行为）
func openSeeded(tb testing.TB, n int) *Store {
	tb.Helper()
	s, err := Open(filepath.Join(tb.TempDir(), "bench.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	seedBulkHistory(tb, s, n)
	return s
}

const benchRows = 100_000

func BenchmarkQueryHistory_FirstPage(b *testing.B) {
	s := openSeeded(b, benchRows)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := s.QueryHistory(ctx, &HistoryFilter{PageSize: 50, Page: 1}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryHistory_DeepPage(b *testing.B) {
	s := openSeeded(b, benchRows)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := s.QueryHistory(ctx, &HistoryFilter{PageSize: 50, Page: 1500}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryHistory_FilterByType(b *testing.B) {
	s := openSeeded(b, benchRows)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := s.QueryHistory(ctx, &HistoryFilter{MediaType: "video", PageSize: 50, Page: 1}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryHistory_SearchFileName(b *testing.B) {
	s := openSeeded(b, benchRows)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := s.QueryHistory(ctx, &HistoryFilter{Query: "file_099", PageSize: 50, Page: 1}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHistoryStats(b *testing.B) {
	s := openSeeded(b, benchRows)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.HistoryStats(ctx, &HistoryFilter{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRecordPair 测量单个媒体的两次记录写入（入队 + 终态）。
// 这是下载期间唯一的持久化热点：每个文件恰好一对。
func BenchmarkRecordPair(b *testing.B) {
	s := openSeeded(b, benchRows)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		msgID := int64(benchRows + i)
		rec := &HistoryRecord{
			ChatID: 999, MessageID: msgID, MediaType: "video", FileName: "x.mp4",
			FilePath: "/d/x.mp4", FileSize: 1024, Status: HistoryStatusQueued,
		}
		if err := s.UpsertHistoryStart(ctx, rec); err != nil {
			b.Fatal(err)
		}
		if err := s.UpdateHistoryResult(ctx, 999, msgID, HistoryStatusCompleted, "", "/d/x.mp4"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkListFailedByTask(b *testing.B) {
	s := openSeeded(b, benchRows)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.ListFailedByTask(ctx, "t7"); err != nil {
			b.Fatal(err)
		}
	}
}
