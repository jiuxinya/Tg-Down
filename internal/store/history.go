package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	// DefaultHistoryPageSize 是 QueryHistory 在 PageSize<=0 时使用的默认分页大小。
	DefaultHistoryPageSize = 20
	// MaxHistoryPageSize 是 QueryHistory 允许的最大分页大小。
	MaxHistoryPageSize = 100
)

// UpsertHistoryStart 在下载入队/跳过时写入或刷新一条历史记录，
// 以 (chat_id, message_id) 作为幂等键，使重复扫描不会产生重复行。
//
// 只有 completed 的行受保护（不允许被后续事件回退），failed 的行一律放行重新激活——
// 无论它是被进程重启清扫的，还是因网络/磁盘错误真失败的，都应该能被重试修复。
func (s *Store) UpsertHistoryStart(ctx context.Context, rec *HistoryRecord) error {
	const q = `
INSERT INTO history (task_id, chat_id, chat_title, message_id, media_type, file_name, file_path,
                      file_size, mime_type, status, reason, created_at, finished_at, unique_id,
                      album_id, minithumb)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL, ?, ?, ?)
ON CONFLICT(chat_id, message_id) DO UPDATE SET
  task_id    = excluded.task_id,
  chat_title = excluded.chat_title,
  media_type = excluded.media_type,
  file_name  = excluded.file_name,
  file_path  = excluded.file_path,
  file_size  = excluded.file_size,
  mime_type  = excluded.mime_type,
  status     = excluded.status,
  reason     = NULL,
  created_at = excluded.created_at,
  finished_at = NULL,
  unique_id  = COALESCE(NULLIF(excluded.unique_id, ''), history.unique_id),
  album_id   = excluded.album_id,
  minithumb  = COALESCE(excluded.minithumb, history.minithumb)
WHERE history.status != 'completed'`

	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	_, err := s.execContext(ctx, q,
		nullString(rec.TaskID), rec.ChatID, nullString(rec.ChatTitle), rec.MessageID,
		rec.MediaType, rec.FileName, rec.FilePath, rec.FileSize, nullString(rec.MimeType),
		rec.Status, timeToUnix(createdAt), nullString(rec.UniqueID), rec.AlbumID,
		nullBytes(rec.Minithumb),
	)
	if err != nil {
		return fmt.Errorf("写入下载历史失败: %w", err)
	}
	return nil
}

// SetHistoryThumb 记录缩略图缓存路径（下载完成后单独写入，不影响主记录的终态判定）
func (s *Store) SetHistoryThumb(ctx context.Context, chatID, messageID int64, thumbPath string) error {
	_, err := s.execContext(ctx,
		`UPDATE history SET thumb_path = ? WHERE chat_id = ? AND message_id = ?`,
		nullString(thumbPath), chatID, messageID)
	if err != nil {
		return fmt.Errorf("更新缩略图路径失败: %w", err)
	}
	return nil
}

// GetHistoryByID 按主键查询单条下载记录（含缩略图字段），未找到返回 nil, nil。
//
// 这是媒体服务端点的唯一入口：只有库里记录在案的文件才可能被读到，
// 因此下载根目录里的 .tdlib-files 缓存、.json sidecar 天然不可达——比开一个
// FileServer 再想办法把它们挡住要可靠得多。
func (s *Store) GetHistoryByID(ctx context.Context, id int64) (*HistoryRecord, error) {
	const q = `
SELECT id, task_id, chat_id, chat_title, message_id, media_type, file_name, file_path,
	       file_size, mime_type, status, reason, created_at, finished_at, unique_id, album_id,
	       thumb_path, minithumb
FROM history WHERE id = ?`

	rec, err := scanHistoryRow(s.db.QueryRowContext(ctx, q, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("按 id 查询下载历史失败: %w", err)
	}

	return rec, nil
}

// DeleteHistoryByTask 删除某个任务关联的全部下载历史记录（只删记录，不动磁盘文件）。
// 返回删除的行数。
func (s *Store) DeleteHistoryByTask(ctx context.Context, taskID string) (int64, error) {
	res, err := s.execContext(ctx, `DELETE FROM history WHERE task_id = ?`, taskID)
	if err != nil {
		return 0, fmt.Errorf("删除任务下载历史失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取删除行数失败: %w", err)
	}
	return n, nil
}

// DeleteHistory 删除一条下载历史记录（只删记录，不动磁盘文件）。
// 画廊/文件访问入口随之失效（按 id 查库），但文件本体保留在下载目录。
func (s *Store) DeleteHistory(ctx context.Context, id int64) error {
	res, err := s.execContext(ctx, `DELETE FROM history WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除下载历史失败: %w", err)
	}
	return checkRowsAffected(res, "下载历史", strconv.FormatInt(id, 10))
}

// UpdateHistoryResult 按 (chat_id, message_id) 更新下载结果；状态为终态
// （completed/failed/skipped）时记录 finished_at；filePath 为空时保留原有路径不覆盖。
// 终态守卫：已 completed 的行不允许被后续 failed 事件覆盖（并发下载同一消息时，
// 落败一方的 RecordFailed 可能晚于胜出一方的 RecordCompleted 到达，文件其实已存在），
// 但 failed -> completed（重试成功）仍允许，保持重试语义。
func (s *Store) UpdateHistoryResult(ctx context.Context, chatID, messageID int64, status, reason, filePath string) error {
	return s.UpdateHistoryResultWithThumb(ctx, chatID, messageID, status, reason, filePath, "")
}

// UpdateHistoryResultWithThumb 在写终态的同一条语句里一并记录缩略图路径。
//
// 缩略图与终态在下载完成的同一时刻就都已知，分两条语句写等于每个媒体多付一次
// 独立事务的提交开销；thumbPath 为空时该列保持不变，因此失败/跳过路径可以共用这条语句。
func (s *Store) UpdateHistoryResultWithThumb(
	ctx context.Context, chatID, messageID int64, status, reason, filePath, thumbPath string,
) error {
	const q = `
	UPDATE history SET
	  status = CASE WHEN status = 'completed' AND ? = 1 THEN status ELSE ? END,
	  reason = CASE WHEN status = 'completed' AND ? = 1 THEN reason ELSE ? END,
	  file_path = CASE
	    WHEN status = 'completed' AND ? = 1 THEN file_path
	    ELSE COALESCE(NULLIF(?, ''), file_path)
	  END,
	  thumb_path = COALESCE(NULLIF(?, ''), thumb_path),
	  finished_at = CASE
	    WHEN status = 'completed' AND ? = 1 THEN finished_at
	    WHEN ? = 1 THEN ?
	    ELSE finished_at
	  END
	WHERE chat_id = ? AND message_id = ?`

	isTerminal := status == HistoryStatusCompleted || status == HistoryStatusFailed || status == HistoryStatusSkipped
	protectCompleted := status != HistoryStatusCompleted
	res, err := s.execContext(ctx, q,
		protectCompleted, status,
		protectCompleted, nullString(reason),
		protectCompleted, filePath,
		thumbPath,
		protectCompleted, isTerminal, time.Now().Unix(),
		chatID, messageID)
	if err != nil {
		return fmt.Errorf("更新下载历史失败: %w", err)
	}
	return checkRowsAffected(res, "下载历史", fmt.Sprintf("chat_id=%d,message_id=%d", chatID, messageID))
}

// SweepInterruptedHistory 将所有仍停留在在途状态的历史行终结为 "failed"（原因
// HistoryReasonInterrupted），供进程启动时调用一次：崩溃/被杀/关停会遗留永不终态的
// 在途行（其 RecordCompleted/RecordFailed 事件丢失），污染统计与状态筛选。
// 被清扫的行可由 ListFailedByTask 定位并在任务恢复时补下。返回被清理的行数。
//
// 同时清扫 'queued'（当前的在途状态）与 'downloading'（v2.x 遗留，升级后的旧库里仍有）。
func (s *Store) SweepInterruptedHistory(ctx context.Context) (int64, error) {
	const q = `
UPDATE history SET
  status = 'failed',
  reason = ?,
  finished_at = ?
WHERE status IN ('queued', 'downloading')`

	res, err := s.execContext(ctx, q, HistoryReasonInterrupted, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("清理中断的下载历史失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListFailedByTask 返回指定任务所有失败行的 message_id 列表，供任务恢复或重试时逐条补下。
//
// 此前该查询限定 reason = 'interrupted'，只捞被进程重启清扫的行，导致因网络/磁盘/TDLib
// 报错而失败的文件永远不会被补下——它们的 reason 是真实错误文本，不匹配这个条件。
// 现在不再按 reason 过滤：任何失败的文件都应该有机会重试。
func (s *Store) ListFailedByTask(ctx context.Context, taskID string) ([]int64, error) {
	const q = `
SELECT message_id FROM history
WHERE task_id = ? AND status = 'failed'
ORDER BY message_id DESC`

	rows, err := s.db.QueryContext(ctx, q, taskID)
	if err != nil {
		return nil, fmt.Errorf("查询失败的下载历史失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("解析失败的下载历史失败: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历失败的下载历史失败: %w", err)
	}
	return ids, nil
}

// FindCompletedByUniqueID 按 TDLib remote unique_id 查找最近一条已完成的下载记录，
// 用于内容级去重（同一文件被转发到多个聊天时避免重复下载）；未找到返回 nil, nil
func (s *Store) FindCompletedByUniqueID(ctx context.Context, uniqueID string) (*HistoryRecord, error) {
	if uniqueID == "" {
		return nil, nil
	}
	const q = `
SELECT id, task_id, chat_id, chat_title, message_id, media_type, file_name, file_path,
	       file_size, mime_type, status, reason, created_at, finished_at, unique_id, album_id,
	       thumb_path, minithumb
FROM history
WHERE unique_id = ? AND status = 'completed'
ORDER BY finished_at DESC LIMIT 1`

	row := s.db.QueryRowContext(ctx, q, uniqueID)
	rec, err := scanHistoryRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("按 unique_id 查询下载历史失败: %w", err)
	}
	return rec, nil
}

// historyFilterClause 根据过滤条件构建 WHERE 子句（不含 "WHERE" 关键字）与对应参数，
// 供 QueryHistory 与 HistoryStats 共用
func historyFilterClause(f *HistoryFilter) (where string, args []any) {
	var conds []string

	if f.MediaType != "" {
		conds = append(conds, "media_type = ?")
		args = append(args, f.MediaType)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.Query != "" {
		// 走 FTS5 倒排索引而非 LIKE '%q%'：前置通配符使 LIKE 必然全表扫描，
		// 十万行量级下每次搜索都要读完整张表。
		//
		// unicode61 分词器不索引标点，因此纯标点的查询（"%"、"_"）在 FTS 里没有词元可查。
		// 这类查询回退到 LIKE：它们本就罕见，全表扫一次的代价可以接受，
		// 而"能按文件名里的字面标点搜索"这一既有能力不该因为换索引而消失。
		if match := ftsMatchQuery(f.Query); match != "" {
			conds = append(conds, "id IN (SELECT rowid FROM history_fts WHERE file_name MATCH ?)")
			args = append(args, match)
		} else {
			conds = append(conds, "file_name LIKE ? ESCAPE '!'")
			args = append(args, "%"+escapeLikePattern(f.Query)+"%")
		}
	}
	if f.ChatID != 0 {
		conds = append(conds, "chat_id = ?")
		args = append(args, f.ChatID)
	}
	if f.TaskID != "" {
		conds = append(conds, "task_id = ?")
		args = append(args, f.TaskID)
	}
	if f.From != nil {
		conds = append(conds, "created_at >= ?")
		args = append(args, f.From.Unix())
	}
	if f.To != nil {
		conds = append(conds, "created_at <= ?")
		args = append(args, f.To.Unix())
	}

	if len(conds) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// escapeLikePattern 把用户输入中的 LIKE 元字符转成字面量；! 自身也需先转义。
func escapeLikePattern(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}

// ftsMatchQuery 把用户输入转成 FTS5 的前缀匹配表达式；无可索引词元时返回空串，
// 由调用方回退到 LIKE。
//
// 用户在搜索框里输入的是文件名片段，不是 FTS 查询语法。直接把原文交给 MATCH，
// 其中的 "、*、:、^、AND/OR/NOT 等会被当作运算符——轻则查不到，重则语法错误报 500。
// 因此把每个词整体加双引号变成字面量短语，再补 * 做前缀匹配（搜 "vid" 能命中 "video.mp4"）。
// 内部的双引号按 FTS5 规则用两个双引号转义。
func ftsMatchQuery(s string) string {
	terms := make([]string, 0, 4)
	for _, field := range strings.Fields(s) {
		// 剥掉词元首尾的标点：unicode61 不索引它们，留着会让短语匹配不到任何词元
		trimmed := strings.TrimFunc(field, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if trimmed == "" {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(trimmed, `"`, `""`)+`"*`)
	}
	if len(terms) == 0 {
		return ""
	}
	return strings.Join(terms, " ")
}

// HistoryPage 是一页下载历史及其游标。
type HistoryPage struct {
	Items []*HistoryRecord
	// NextCursor 是下一页的起点，nil 表示已到末页
	NextCursor *HistoryCursor
	// Total 是忽略分页的匹配总数，仅在请求 WithTotal 时有值
	Total *int
}

// QueryHistory 按过滤条件取一页下载历史。
//
// 分页用 keyset 而非 LIMIT/OFFSET：翻到第 N 页时 OFFSET 仍要先扫掉前面 (N-1)*size 行，
// 代价随页码线性增长；游标则每页都是一次索引定位。排序键恒定带上 id 作次键，
// 否则同一秒入库的多行在 created_at 上并列，翻页时可能重复或漏掉。
func (s *Store) QueryHistory(ctx context.Context, f *HistoryFilter) (*HistoryPage, error) {
	where, args := historyFilterClause(f)

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultHistoryPageSize
	}
	if limit > MaxHistoryPageSize {
		limit = MaxHistoryPageSize
	}

	sortCol, desc := f.Sort.sortColumn()
	queryArgs := append([]any{}, args...)

	var q strings.Builder
	// 不取 minithumb BLOB：列表接口只用它算"有无缩略图"，取回本体等于每页白搬数十 KB
	q.WriteString(`SELECT id, task_id, chat_id, chat_title, message_id, media_type, file_name, file_path,
	       file_size, mime_type, status, reason, created_at, finished_at, unique_id, album_id,
	       thumb_path, (minithumb IS NOT NULL AND length(minithumb) > 0)
FROM history `)
	q.WriteString(where)

	if f.Cursor != nil {
		cmp := "<"
		if !desc {
			cmp = ">"
		}
		// 行值比较而非展开成 (col < ? OR (col = ? AND id < ?))：后者是等价的布尔表达式，
		// 但 SQLite 无法把带 OR 的形式收敛成一次索引区间扫描，深翻页会退化成近似全表扫描
		// （实测第 75000 行处相差一个数量级）。(col, id) < (?, ?) 则直接定位到索引上的一点。
		clause := fmt.Sprintf("(%s, id) %s (?, ?)", sortCol, cmp)
		if where == "" {
			q.WriteString(" WHERE " + clause)
		} else {
			q.WriteString(" AND " + clause)
		}
		queryArgs = append(queryArgs, f.Cursor.SortValue, f.Cursor.ID)
	}

	dir := "DESC"
	if !desc {
		dir = "ASC"
	}
	// 多取一行用来判断还有没有下一页，返回前丢弃
	fmt.Fprintf(&q, " ORDER BY %s %s, id %s LIMIT ?", sortCol, dir, dir)
	queryArgs = append(queryArgs, limit+1)

	rows, err := s.db.QueryContext(ctx, q.String(), queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("查询下载历史失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var items []*HistoryRecord
	for rows.Next() {
		rec, err := scanHistoryListRow(rows)
		if err != nil {
			return nil, fmt.Errorf("解析下载历史失败: %w", err)
		}
		items = append(items, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历下载历史失败: %w", err)
	}

	page := &HistoryPage{}
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		page.NextCursor = &HistoryCursor{SortValue: cursorValue(last, sortCol), ID: last.ID}
	}
	page.Items = items

	if f.WithTotal {
		total, err := s.historyTotal(ctx, where, args)
		if err != nil {
			return nil, err
		}
		page.Total = &total
	}
	return page, nil
}

// cursorValue 取出该行的排序键取值，与 sortColumn 的列名一一对应
func cursorValue(rec *HistoryRecord, sortCol string) int64 {
	if sortCol == sortColFileSize {
		return rec.FileSize
	}
	return rec.CreatedAt.Unix()
}

// historyTotal 统计匹配总数（忽略分页）
func (s *Store) historyTotal(ctx context.Context, where string, args []any) (int, error) {
	var total int
	q := "SELECT COUNT(*) FROM history " + where
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("统计下载历史总数失败: %w", err)
	}
	return total, nil
}

// HistoryStats 按 media_type 分组统计下载历史，过滤条件与 QueryHistory 一致（忽略分页）。
//
// 结果按筛选条件缓存：这条查询要扫过整个匹配集做分组聚合，而历史页每次翻页都会连带请求
// 一次统计（分页变了但统计不变）。任何写入都会清空缓存，因此下载进行中读到的仍是当前数据。
func (s *Store) HistoryStats(ctx context.Context, f *HistoryFilter) ([]MediaTypeStat, error) {
	where, args := historyFilterClause(f)
	cacheKey := statsCacheKey(where, args)

	s.statsMu.RLock()
	cached, ok := s.statsCache[cacheKey]
	s.statsMu.RUnlock()
	if ok {
		return append([]MediaTypeStat(nil), cached...), nil
	}

	var q strings.Builder
	q.WriteString(`SELECT media_type,
       COUNT(*),
       COALESCE(SUM(file_size), 0),
       SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END),
       SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END),
       SUM(CASE WHEN status = 'skipped' THEN 1 ELSE 0 END)
FROM history `)
	q.WriteString(where)
	q.WriteString(` GROUP BY media_type ORDER BY media_type`)

	rows, err := s.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("统计下载历史失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var stats []MediaTypeStat
	for rows.Next() {
		var st MediaTypeStat
		if err := rows.Scan(&st.MediaType, &st.Count, &st.TotalSize, &st.Completed, &st.Failed, &st.Skipped); err != nil {
			return nil, fmt.Errorf("解析下载历史统计失败: %w", err)
		}
		stats = append(stats, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历下载历史统计失败: %w", err)
	}

	s.statsMu.Lock()
	// 缓存条目数按筛选组合增长，给个上限防止被大量不同筛选撑大
	if len(s.statsCache) >= maxStatsCacheEntries {
		s.statsCache = map[string][]MediaTypeStat{}
	}
	s.statsCache[cacheKey] = append([]MediaTypeStat(nil), stats...)
	s.statsMu.Unlock()

	return stats, nil
}

// maxStatsCacheEntries 是统计缓存的条目上限
const maxStatsCacheEntries = 64

// statsCacheKey 由 WHERE 子句与其实参构成，两个筛选条件相同的请求才会命中同一条缓存
func statsCacheKey(where string, args []any) string {
	var b strings.Builder
	b.WriteString(where)
	for _, a := range args {
		fmt.Fprintf(&b, "\x00%v", a)
	}
	return b.String()
}

// scanHistoryRow 从单行结果解析出 HistoryRecord
func scanHistoryRow(row scanner) (*HistoryRecord, error) {
	var (
		rec                             HistoryRecord
		taskID, chatTitle, mime, reason sql.NullString
		uniqueID, thumbPath             sql.NullString
		createdAt                       int64
		finishedAt                      sql.NullInt64
		minithumb                       []byte
	)

	if err := row.Scan(
		&rec.ID, &taskID, &rec.ChatID, &chatTitle, &rec.MessageID, &rec.MediaType, &rec.FileName,
		&rec.FilePath, &rec.FileSize, &mime, &rec.Status, &reason, &createdAt, &finishedAt,
		&uniqueID, &rec.AlbumID, &thumbPath, &minithumb,
	); err != nil {
		return nil, err
	}

	rec.TaskID = taskID.String
	rec.ChatTitle = chatTitle.String
	rec.MimeType = mime.String
	rec.Reason = reason.String
	rec.UniqueID = uniqueID.String
	rec.ThumbPath = thumbPath.String
	rec.Minithumb = minithumb
	rec.HasMinithumb = len(minithumb) > 0
	rec.CreatedAt = unixToTime(createdAt)
	rec.FinishedAt = nullInt64ToTimePtr(finishedAt)
	return &rec, nil
}

// scanHistoryListRow 解析列表查询的一行：末列是"有无 minithumb"的布尔而非 BLOB 本体。
func scanHistoryListRow(row scanner) (*HistoryRecord, error) {
	var (
		rec                             HistoryRecord
		taskID, chatTitle, mime, reason sql.NullString
		uniqueID, thumbPath             sql.NullString
		createdAt                       int64
		finishedAt                      sql.NullInt64
		hasMinithumb                    bool
	)

	if err := row.Scan(
		&rec.ID, &taskID, &rec.ChatID, &chatTitle, &rec.MessageID, &rec.MediaType, &rec.FileName,
		&rec.FilePath, &rec.FileSize, &mime, &rec.Status, &reason, &createdAt, &finishedAt,
		&uniqueID, &rec.AlbumID, &thumbPath, &hasMinithumb,
	); err != nil {
		return nil, err
	}

	rec.TaskID = taskID.String
	rec.ChatTitle = chatTitle.String
	rec.MimeType = mime.String
	rec.Reason = reason.String
	rec.UniqueID = uniqueID.String
	rec.ThumbPath = thumbPath.String
	rec.HasMinithumb = hasMinithumb
	rec.CreatedAt = unixToTime(createdAt)
	rec.FinishedAt = nullInt64ToTimePtr(finishedAt)
	return &rec, nil
}
