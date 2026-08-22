package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyV1Schema 是 v1.x 的历史表结构：没有 unique_id / album_id 列
const legacyV1Schema = `
CREATE TABLE history (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id     TEXT,
  chat_id     INTEGER NOT NULL,
  chat_title  TEXT,
  message_id  INTEGER NOT NULL,
  media_type  TEXT NOT NULL,
  file_name   TEXT NOT NULL,
  file_path   TEXT NOT NULL,
  file_size   INTEGER NOT NULL,
  mime_type   TEXT,
  status      TEXT NOT NULL,
  reason      TEXT,
  created_at  INTEGER NOT NULL,
  finished_at INTEGER,
  UNIQUE(chat_id, message_id)
);
CREATE TABLE tasks (
  id              TEXT PRIMARY KEY,
  kind            TEXT NOT NULL,
  chat_id         INTEGER NOT NULL,
  chat_title      TEXT,
  status          TEXT NOT NULL,
  created_at      INTEGER NOT NULL,
  started_at      INTEGER,
  finished_at     INTEGER,
  error           TEXT,
  total           INTEGER DEFAULT 0,
  downloaded      INTEGER DEFAULT 0,
  failed          INTEGER DEFAULT 0,
  skipped         INTEGER DEFAULT 0,
  total_size      INTEGER DEFAULT 0,
  downloaded_size INTEGER DEFAULT 0
);
`

// TestOpen_UpgradesLegacyV1Database 校验 v1.x 的旧库能被升级打开。
//
// 回归点：Open 曾一次性执行整段 schema，其中 `CREATE INDEX ... ON history(unique_id)`
// 排在补列迁移之前。旧库的 history 表已存在（CREATE TABLE IF NOT EXISTS 是空操作）却没有
// unique_id 列，于是建索引报 "no such column"，Open 直接失败——任何 v1.x 用户升级后
// 都会开不了库，且历史数据看起来像是丢了。
func TestOpen_UpgradesLegacyV1Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// 造一个 v1.x 形态的库，并写入一行历史数据
	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(legacyV1Schema); err != nil {
		t.Fatalf("建立 v1.x schema 失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO history
		(chat_id, message_id, media_type, file_name, file_path, file_size, status, created_at)
		VALUES (100, 1, 'photo', 'old.jpg', '/tmp/old.jpg', 123, 'completed', 1700000000)`); err != nil {
		t.Fatalf("写入 v1.x 历史行失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// 升级打开：必须成功
	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开 v1.x 旧库失败（升级路径断裂）: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// 旧数据仍在，且新列可用
	items, total, err := s.QueryHistory(context.Background(), &HistoryFilter{})
	if err != nil {
		t.Fatalf("QueryHistory() error = %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("升级后历史记录数 = %d，want 1（旧数据不得丢失）", total)
	}
	if items[0].FileName != "old.jpg" {
		t.Errorf("升级后 FileName = %q, want old.jpg", items[0].FileName)
	}

	// 新增列的写入路径可用（unique_id 是内容级去重的基石）
	if err := s.UpsertHistoryStart(context.Background(), &HistoryRecord{
		ChatID: 100, MessageID: 2, MediaType: "video", FileName: "new.mp4",
		FilePath: "/tmp/new.mp4", FileSize: 456, Status: HistoryStatusQueued, UniqueID: "uniq-1",
	}); err != nil {
		t.Fatalf("升级后写入带 unique_id 的记录失败: %v", err)
	}
	rec, err := s.FindCompletedByUniqueID(context.Background(), "uniq-1")
	if err != nil {
		t.Fatalf("FindCompletedByUniqueID() error = %v", err)
	}
	if rec != nil {
		t.Error("queued 状态的记录不应被去重查询命中")
	}
}

// TestOpen_SchemaVersionGate 校验迁移只跑一次：schema_version 打上标记后，
// 后续 Open 不再重跑那两条全表扫描的归一化 UPDATE。
func TestOpen_SchemaVersionGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	v, err := schemaVersion(context.Background(), s.db)
	if err != nil {
		t.Fatalf("schemaVersion() error = %v", err)
	}
	if v != currentSchemaVersion {
		t.Errorf("新建库的 schema 版本 = %d，want %d", v, currentSchemaVersion)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// 再次打开：版本已是最新，迁移跳过，不应重复写入版本行
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("二次 Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	var rows int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&rows); err != nil {
		t.Fatalf("统计 schema_version 失败: %v", err)
	}
	if rows != 1 {
		t.Errorf("schema_version 行数 = %d，want 1（迁移重复执行了）", rows)
	}
}

func TestOpenRejectsNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version(version) VALUES (?)`, currentSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(path)
	if err == nil || !strings.Contains(err.Error(), "高于当前支持") {
		t.Fatalf("Open(newer schema) error = %v, want explicit downgrade rejection", err)
	}
	db, err = sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var currentTables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('tasks','history','schedules')`).Scan(&currentTables); err != nil {
		t.Fatal(err)
	}
	if currentTables != 0 {
		t.Fatalf("rejecting a future schema still created %d current-version tables", currentTables)
	}
}

// TestOpen_DropsObsoleteIndexes 校验被复合索引取代的旧单列索引在升级时被删除：
// 留着它们只是白付每次写入的维护成本。
func TestOpen_DropsObsoleteIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.db")

	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(legacyV1Schema); err != nil {
		t.Fatalf("建立 v1.x schema 失败: %v", err)
	}
	for _, name := range obsoleteIndexes {
		col := map[string]string{
			"idx_history_media_type": "media_type",
			"idx_history_chat_id":    "chat_id",
			"idx_history_status":     "status",
		}[name]
		if _, err := db.Exec("CREATE INDEX " + name + " ON history(" + col + ")"); err != nil {
			t.Fatalf("建立旧索引 %s 失败: %v", name, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, name := range obsoleteIndexes {
		var n int
		err := s.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n)
		if err != nil {
			t.Fatalf("查询索引 %s 失败: %v", name, err)
		}
		if n != 0 {
			t.Errorf("过时索引 %s 仍然存在", name)
		}
	}
	// 取代它们的复合索引必须在
	for _, name := range []string{"idx_history_type_time", "idx_history_status_time", "idx_history_chat_time"} {
		var n int
		err := s.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n)
		if err != nil {
			t.Fatalf("查询索引 %s 失败: %v", name, err)
		}
		if n != 1 {
			t.Errorf("复合索引 %s 未创建", name)
		}
	}
}

func TestOpen_UpgradesSchemaV2TaskRuntimeFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	const schedulesV2 = `
CREATE TABLE schedules (
  id TEXT PRIMARY KEY, chat_id INTEGER NOT NULL, interval_min INTEGER NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, last_max_id INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE schema_version (version INTEGER NOT NULL);
INSERT INTO schema_version(version) VALUES (2);`
	if _, err := db.Exec(legacyV1Schema + schedulesV2); err != nil {
		t.Fatalf("create v2 database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(v2) error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	row := &TaskRow{
		ID: "scheduled", Kind: "history", ChatID: 9, Status: TaskStatusQueued, CreatedAt: time.Now(),
		StopAtMessageID: 123, ScheduleID: "s1", RetryFailedOnly: true,
	}
	if err := s.CreateTask(context.Background(), row); err != nil {
		t.Fatalf("CreateTask with v3 fields after upgrade: %v", err)
	}
	got, err := s.GetTask(context.Background(), row.ID)
	if err != nil || got == nil || got.StopAtMessageID != 123 || got.ScheduleID != "s1" || !got.RetryFailedOnly {
		t.Fatalf("v3 task fields after v2 upgrade: row=%+v err=%v", got, err)
	}
}
