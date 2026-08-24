# v3.0 开发规划

> 状态：v3.0 已于 2026-08-22 发布。本文勾选框未回填，实际交付核对与遗留项收口见
> [v3.2-plan.md](v3.2-plan.md)（2026-08-24 审计：M3/M5 全清零，M1/M2/M4/M6 各有遗留）。

范围（2026-07-12 确认）：**用户体验向大版本**——下载体验优化、下载能力深化、界面优化，
外加超大聊天规模化、媒体预览画廊、Windows 支持、真实环境回归测试。
不含：安全/多用户鉴权、多账号、上传/转存、平台化 API（推 3.x）。

交付方式：一次性发布 v3.0（不发中间小版本）。
前端：引入 Vite 构建步骤，产物仍 `go:embed` 进二进制。

---

## M1 下载可靠性与可观测

修 v2.0 遗留缺陷，是"下载体验"主轴的地基。

- [ ] **单文件失败不影响任务终态**（`telegram/client.go:1180`、`queue/manager.go:407`）：
      下载错误在 dispatch goroutine 里被吞掉 → 全部文件失败的任务仍标记 completed。
      新增 `partial_failed` 终态，任务卡片显示"完成 N / 失败 M"
- [ ] **FLOOD_WAIT 重试**（`retry/retry.go:62-89`）：现用错误字符串子串匹配，429 / FLOOD_WAIT
      不在列表 → 判为不可重试直接失败。改用 TDLib `ResponseError.Err.Code`，按服务端
      `retry after` 精确退避
- [ ] **扫描页级重试**（`client.go:1263`、`client.go:875`）：retrier 只包了 `DownloadFile`，
      `GetChatHistory` / `GetChatMessageCount` / `GetMessage` 全部裸调，任一页出错即整任务失败
- [ ] **替换 8 空页启发式**（`client.go:54-63`）：连续 8 个空页判定历史结束，容忍窗口仅 ~25s；
      冷缓存时静默误判扫完并标记成功（漏下载且无告警）
- [ ] **去重路径移到信号量之后**（`downloader.go:770-844`）：去重 SQL 查询与 `copyFromDuplicate`
      的全量 `io.Copy` 都在 `limiter.acquire` 之前 → 最多 100 goroutine 并发全文件复制 +
      抢 4 条 SQLite 连接，`max_concurrent=5` 管不到。复制改 hardlink/reflink 优先，回退 copy
- [ ] **真失败的文件纳入补下**（`store/history.go:106-109`）：恢复只捞 `reason='interrupted'`，
      网络/磁盘错误失败的行永远不会被补下
- [ ] **跳过判断校验大小**（`downloader.go:794-798`）：`os.Stat` 命中即跳过，0 字节残留文件被永久跳过
- [ ] `downloading` 状态虚高（`downloader.go:750` vs `:844`）：`RecordStarted` 在 acquire 前发出，
      DB 里可能 100 行 downloading 而实际只下 5 个
- [ ] 磁盘满快速失败（`client.go:914-929`）：现对任何 rename 失败无脑重试 5×500ms + 全量 copy
- [ ] goroutine 泄漏（`client.go:51,259-264`）：`tdCall` 的 24h fallback 让卡住的下载泄漏 goroutine 一天
- [ ] per-file 速度 / ETA（`downloader.go:317-333` 的 `MediaProgress` 只有 size 和 percent，
      全局只有一个 `speed_bps`）；任务级速度与剩余时间
- [ ] per-file 重试端点（今天唯一重试是 `POST /api/tasks/{id}/retry`，创建全新任务从头重扫）
- [ ] 连接状态进 `/api/state`（`client.go:1514` 的 `UpdateConnectionState` 只打日志，
      UI 无法显示"正在等待网络"）
- [ ] **telegram 包注入缝**（与 M6 联动）：`Client.td` 是具体类型 `*tdclient.Client`，
      `NewClient` 直接构造，无任何测试缝。改这些逻辑必须先能测

## M2 超大聊天规模化

现状：枚举 100% 靠全量倒序翻页（`SearchChatMessages` 全库零引用），服务端过滤器只用于计数。
100 万消息 = 1 万次串行 `GetChatHistory` 往返（乐观 33 分钟起）；只勾 video 也要拉回全部纯文本消息。

- [ ] **服务端枚举**：`SearchChatMessages` + `SearchMessagesFilter` 替代全量翻页
      （`client.go:978-985` 已有 filter 映射，只用在 `GetChatMessageCount`）
- [ ] **定时任务增量扫描**（`manager.go:409`）：任务完成后清零 `scan_cursor` →
      每次定时触发都从最新消息重扫整条历史。注释里的"去重使重扫廉价"只对下载成立，对扫描不成立
- [ ] `DateTo` skip-ahead（`client.go:1393-1408`）：现在只能从最新一路翻过来逐条丢弃
- [ ] DB 索引：补 `(chat_id, created_at)` / `(status, created_at)` / `(media_type, created_at)` 复合索引；
      `history.task_id` 建索引（`history.go:106` 现走 `idx_history_status` 全扫 failed 行）；
      删冗余 `idx_history_chat_id`（被 `UNIQUE(chat_id,message_id)` 前缀覆盖）
- [ ] keyset 分页替代 `LIMIT/OFFSET`（`history.go:221`）；每请求一次 O(N) `COUNT(*)`（`history.go:196`）
- [ ] 文件名搜索改 FTS5（`history.go:169` 的 `LIKE '%q%'` 必然全表扫）
- [ ] `/api/history/stats` 缓存（`history.go:248` 每次全表 GROUP BY + SUM）
- [ ] `synchronous=NORMAL`；每媒体 2 次独立事务（`history.go:23` + `:66`）改批量提交
- [ ] `schema_version` 表（`store.go:31` 无版本表，两条 O(N) 归一化 UPDATE 每次启动无条件跑）

## M3 下载能力深化

- [ ] **统一媒体类型词汇源**：`downloader.go:32-38` 与 `client.go:68-73` 各定义一套常量，
      靠字面量巧合对齐。新增一种类型要同时改 9 处：`extractMediaFile` / 常量组 / `captionText` /
      `historyCountFilters` / `ValidMediaTypes` / `classifyDir` / `messagePreview` / 前端复选框 /
      `getFileExtension`。漏 `historyCountFilters` → 进度条超 100%；漏 `ValidMediaTypes` → 建任务 400
- [ ] 新增 sticker / video note。注意 TDLib **没有 sticker 的 SearchMessagesFilter**，
      其服务端计数架构上不可能准确，需单独处理；`Sticker` 结构无 `FileName`，需合成文件名
- [ ] **文件名/目录模板**：`{chat_title}` `{date}` `{sender}` `{album}` `{msg_id}` `{ext}` 占位符
      + 同名冲突策略。今天全硬编码（`client.go:770,800,849`、`downloader.go:945-959`），
      chat 目录只用数字 ID（`chat_-1001234567890/`），尽管 `chat_title` 已落库。
      **默认模板 = 现有布局**，避免旧文件重下
- [ ] 关键词 / 发送者过滤（`MediaInfo.SenderID` 已提取但未使用）
- [ ] 聊天导出归档（HTML / JSON，类似 Telegram Desktop export）

## M4 界面重构 + 媒体预览画廊

画廊后端是零起点：`ServeFile` / `FileServer` / `thumbnail` 全库零命中，**TDLib 层从未请求过缩略图**。

- [ ] TDLib 缩略图 / minithumbnail 获取
- [ ] 媒体服务端点。两个必须先堵的泄漏点：`.tdlib-files`（`client.go:155`，就在下载根目录内）
      与 `<文件>.json` sidecar（`downloader.go:878`，含 caption/发送者）——裸 FileServer 会整个暴露。
      `?token=` 鉴权通道已存在（SSE 在用），`<img>` 可复用；写入侧 `isSafePath`（`downloader.go:1007`）
      的判定逻辑可复用到读取侧
- [ ] `album_id` 加进 API DTO（`store.go:74` 已落库、文件已按 `album_<id>` 分目录、索引已建，
      唯独 `handlers.go:611-626` 的 DTO 把它丢了）→ 画廊按相册分组
- [ ] **前端 Vite 重构**：1638 行单文件已有 4 组重复 DOM 代码（媒体卡片渲染两遍、聊天下拉三遍、
      并发输入框两处、全部暂停按钮两处）；30 处 `innerHTML` 全量重建 + 1Hz state 推送 →
      图片节点每秒销毁重建。产物仍 `go:embed`
- [ ] 增量渲染替代全量 `innerHTML`；SSE 的 `task` 事件 payload 目前被完全丢弃，
      只触发 `/api/tasks` 全量重拉（`index.html:1624`）
- [ ] CSP / X-Frame-Options / X-Content-Type-Options（今天一个安全响应头都没有，
      而转义靠手写 `escapeHtml` 逐点调用）
- [ ] 历史页：`reason` 与 `file_path` 已发给浏览器但未使用；stats 返回 5 字段只画了 1 个；
      补排序、每页条数、导出
- [ ] 任务详情页 → 该任务下载了哪些文件（后端无此端点）

## M5 Windows 支持

- [ ] 静态库列表提取单一来源（现在 `Makefile:12` / `.github/actions/setup-tdlib/action.yml:26` /
      `Dockerfile:27` / `release.yml:60` 四处各写一遍）
- [ ] `scripts/install-tdlib.sh:24` 的 `case "$(uname -s)"` 无 Windows 分支且无 default →
      MSYS2 下静默跳过依赖安装。补 MinGW-w64 分支（CGo 只能用 gcc 系，MSVC 路线不可行）
- [ ] 链接 flag：`-Wl,-rpath`（PE 无此概念）、`-ldl`（Windows 无 libdl）需 Windows 变体
- [ ] `cmd/main.go:182` 的 `syscall.SIGTERM` 在 Windows Go 里不存在 → 编译失败
- [ ] `sanitizeFileName`（`downloader.go:966-985`）不处理 CON/PRN/NUL 保留名、长度上限、首尾空格点
- [ ] CI 加 windows job；发布产物 `.zip` + `tg-down.exe`（`release.yml:73-88` 的
      `start.sh` / `tar.gz` / `objdump` 是 Linux 专属）

## M6 真实回归测试基线

v2.0 发布前真实手测被跳过。`internal/telegram`（1558 行，全仓最大）零测试、零注入缝。
隐藏约束：`cmd` 与 `internal/web` 传递依赖 telegram → CGo，**连 store/config 的测试都必须先编 TDLib**，
不存在快速单测车道。

- [ ] telegram / web 包接口缝（项目已有此习惯：`queue.ChatDownloader`（`queue/queue.go:43`）与
      `downloader.SetDownloadFunc`（`downloader.go:235`）是现成的两条缝，只是没延伸过去）
- [ ] 不依赖 TDLib 的快速单测车道
- [ ] 真实账号集成测试（`//go:build integration`）：登录 / 下载 / kill -9 恢复 / 转发去重 /
      相册 / sidecar / 定时 / 通知——把 v2.0 跳过的清单固化成可重跑基线
- [ ] `make test` + 覆盖率（Makefile 无 test target；CI 是裸 `go test -v ./...`，无 `-race` 无 `-cover`）
- [ ] `internal/web/handlers.go`（765 行）、`internal/notify`、`internal/logger` 零覆盖

---

## 顺序与依赖

**M1 → M2 → M3 → M4 → M5 → M6**

- M6 的"注入缝"提到 M1 一起做（改 telegram 核心逻辑不能没有测试）
- M1 + M2 都动扫描/下载核心，连着做避免二次进场
- M4 依赖 M3 的媒体类型 registry 与 M4 自身的后端文件端点
- M5 与 M6 可与 M4 并行

## 破坏性变更

- 文件名模板改变落盘路径（默认值 = 现有布局，仅用户显式改模板才变）
- DB 索引与 `schema_version` 迁移
- 前端整体重构 + 引入 Node 构建链（贡献者需装 npm）
