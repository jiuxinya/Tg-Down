// Package timeline 提供基于 sidecar JSON 的「类 Telegram 本地时间线」数据层：
// 扫描下载根目录下的 <文件>.json 元数据（save_metadata 开启时由下载器生成），
// 按消息日期排序，支持按频道过滤与游标分页。数据源只读，不改动任何已下载文件。
package timeline

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Entry 时间线中的一条消息（对应一个已下载媒体 + 其 sidecar）
type Entry struct {
	MessageID  int64  `json:"message_id"`
	ChatID     int64  `json:"chat_id"`
	ChatTitle  string `json:"chat_title,omitempty"`
	Date       int64  `json:"date"`
	DateText   string `json:"date_text,omitempty"`
	SenderID   int64  `json:"sender_id,omitempty"`
	Caption    string `json:"caption,omitempty"`
	AlbumID    int64  `json:"album_id,omitempty"`
	MediaType  string `json:"media_type"`
	FileName   string `json:"file_name"`
	FileSize   int64  `json:"file_size,omitempty"`
	MimeType   string `json:"mime_type,omitempty"`
	UniqueID   string `json:"unique_id,omitempty"`
	MessageURL string `json:"message_url,omitempty"`
	// RelPath 是媒体文件相对下载根目录的路径（正斜杠），供文件端点取用
	RelPath string `json:"rel_path"`
}

// ChatSummary 频道摘要，左侧列表用（头像由前端按标题首字生成）
type ChatSummary struct {
	ChatID       int64  `json:"chat_id"`
	Title        string `json:"title"`
	Count        int    `json:"count"`
	LastDate     int64  `json:"last_date"`
	LastDateText string `json:"last_date_text,omitempty"`
}

// PageCursor 分页游标：以此条目为界取更旧的消息
type PageCursor struct {
	Date      int64 `json:"date"`
	MessageID int64 `json:"message_id"`
}

// Index 是 sidecar 扫描结果的内存索引
type Index struct {
	mu      sync.RWMutex
	rebMu   sync.Mutex // 串行化重建（手动刷新与后台重建不得并发改目录缓存）
	entries []Entry
	byChat  map[int64][]Entry
	chats   []ChatSummary
	builtAt time.Time

	// 增量扫描：按目录缓存解析结果与目录快照。
	// 目录 mtime + 文件数未变化时整棵子树跳过，只重扫变化目录（机械盘上从 60s 降到秒级）。
	full     bool // 是否已完成过一次全量扫描
	dirCache map[string][]Entry       // rel 目录 → 该目录内解析出的条目
	dirStat  map[string]dirStatInfo   // rel 目录 → 快照
}

// dirStatInfo 目录快照：用于判定目录内容是否变化
type dirStatInfo struct {
	modSec int64    // 目录 mtime（秒）
	nFiles int      // 目录项数量
	nJson  int      // 目录内 *.json sidecar 数量
	subs   []string // 目录内子目录名（跳过解析时仍需递归，子目录各自判定）
}

func New() *Index {
	return &Index{
		byChat:   make(map[int64][]Entry),
		dirCache: make(map[string][]Entry),
		dirStat:  make(map[string]dirStatInfo),
	}
}

// sidecarFile 是磁盘上 sidecar JSON 的解析结构（与 downloader.mediaSidecar 字段对应）
type sidecarFile struct {
	MessageID  int64  `json:"message_id"`
	ChatID     int64  `json:"chat_id"`
	ChatTitle  string `json:"chat_title"`
	Date       int64  `json:"date"`
	DateText   string `json:"date_text"`
	SenderID   int64  `json:"sender_id"`
	Caption    string `json:"caption"`
	AlbumID    int64  `json:"album_id"`
	MediaType  string `json:"media_type"`
	FileName   string `json:"file_name"`
	FileSize   int64  `json:"file_size"`
	MimeType   string `json:"mime_type"`
	UniqueID   string `json:"unique_id"`
	MessageURL string `json:"message_url"`
}

// Rebuild 重建索引：首次全量扫描；之后增量扫描（只重扫目录快照变化的子树）。
// 单文件损坏/缺失媒体文件时跳过不报错。
func (ix *Index) Rebuild(root string) error {
	ix.rebMu.Lock()
	defer ix.rebMu.Unlock()
	if !ix.full {
		if err := ix.walk(root, "", true, nil); err != nil {
			return err
		}
		ix.full = true
	} else {
		visited := make(map[string]bool)
		if err := ix.walk(root, "", false, visited); err != nil {
			return err
		}
		// 清理已被删除的目录（其内容不能再留在索引里）
		for rel := range ix.dirStat {
			if !visited[rel] {
				delete(ix.dirCache, rel)
				delete(ix.dirStat, rel)
			}
		}
	}
	ix.assemble()
	ix.builtAt = time.Now()
	return nil
}

// walk 递归扫描下载根目录。
// 目录快照命中（mtime + 目录项数均未变，轻量 ReadDir 一次确认）时：
// 跳过本目录文件解析，但按上次记录的子目录名继续递归（子目录内容可在父目录
// mtime 不变时自行变化，必须逐目录判定）。
// 快照未命中：解析 sidecar + 更新快照，再递归子目录。
// changed=true（全量首扫）时不做跳过；visited 用于清理已消失的目录。
func (ix *Index) walk(root, rel string, changed bool, visited map[string]bool) error {
	if visited != nil {
		visited[rel] = true
	}
	abs := filepath.Join(root, rel)
	if !changed {
		if snap, ok := ix.dirStat[rel]; ok {
			if st, err := os.Stat(abs); err == nil && snap.modSec == st.ModTime().Unix() {
				if d, err := os.ReadDir(abs); err == nil && len(d) == snap.nFiles {
					// 目录未变化：文件不动，子目录软递归（各自判定）
					for _, sub := range snap.subs {
						if err := ix.walk(root, filepath.Join(rel, sub), false, visited); err != nil {
							return err
						}
					}
					return nil
				}
			}
		}
	}
	d, err := os.ReadDir(abs)
	if err != nil {
		return nil // 单目录不可读则跳过，绝不中断整个扫描
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil
	}
	// 本目录文件
	entries, njson := scanDirSidecars(root, abs, d)
	if len(entries) > 0 {
		ix.dirCache[rel] = entries
	} else {
		delete(ix.dirCache, rel)
	}
	var subs []string
	for _, ent := range d {
		if !ent.IsDir() {
			continue
		}
		name := ent.Name()
		// 缓存/系统目录（.thumbs、.tdlib-files、lost+found 等）不进时间线
		if strings.HasPrefix(name, ".") || name == "lost+found" || name == "TG-down" {
			continue
		}
		subs = append(subs, name)
	}
	ix.dirStat[rel] = dirStatInfo{modSec: st.ModTime().Unix(), nFiles: len(d), nJson: njson, subs: subs}
	for _, sub := range subs {
		if err := ix.walk(root, filepath.Join(rel, sub), changed, visited); err != nil {
			return err
		}
	}
	return nil
}

// scanDirSidecars 解析一个目录内全部有效 sidecar，返回条目列表与目录内 json 数量。
// 与旧全量实现的判定规则完全一致：跳过点开头文件、.metadata- 临时文件、
// 无对应媒体文件的残留 sidecar、损坏/缺关键字段的 JSON。
func scanDirSidecars(root, dir string, list []fs.DirEntry) ([]Entry, int) {
	njson := 0
	var out []Entry
	for _, d := range list {
		name := d.Name()
		if d.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasPrefix(name, ".metadata-") {
			continue
		}
		njson++
		mediaAbs := filepath.Join(dir, strings.TrimSuffix(name, ".json"))
		st, err := os.Stat(mediaAbs)
		if err != nil || st.IsDir() {
			continue // sidecar 无对应媒体文件（异常残留），忽略
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var sc sidecarFile
		if err := json.Unmarshal(data, &sc); err != nil {
			continue
		}
		if sc.MessageID == 0 || sc.ChatID == 0 {
			continue
		}
		if sc.DateText == "" {
			sc.DateText = time.Unix(sc.Date, 0).Format("2006-01-02 15:04:05")
		}
		rel, err := filepath.Rel(root, mediaAbs)
		if err != nil {
			continue
		}
		out = append(out, Entry{
			MessageID:  sc.MessageID,
			ChatID:     sc.ChatID,
			ChatTitle:  sc.ChatTitle,
			Date:       sc.Date,
			DateText:   sc.DateText,
			SenderID:   sc.SenderID,
			Caption:    sc.Caption,
			AlbumID:    sc.AlbumID,
			MediaType:  sc.MediaType,
			FileName:   sc.FileName,
			FileSize:   sc.FileSize,
			MimeType:   sc.MimeType,
			UniqueID:   sc.UniqueID,
			MessageURL: sc.MessageURL,
			RelPath:    filepath.ToSlash(rel),
		})
	}
	return out, njson
}

// assemble 从 dirCache 重建全量索引（entries/byChat/chats）。
// 每次增量后全量重组：条目数在十万级时排序组装也在几十毫秒内。
func (ix *Index) assemble() {
	entries := make([]Entry, 0, 1024)
	for _, list := range ix.dirCache {
		entries = append(entries, list...)
	}
	sortEntries(entries)

	byChat := make(map[int64][]Entry)
	for _, e := range entries {
		byChat[e.ChatID] = append(byChat[e.ChatID], e)
	}
	for cid := range byChat {
		sortEntries(byChat[cid])
	}

	// 频道摘要按最后消息时间升序（最早同步的频道在最上面，与消息列表方向一致）
	chats := make([]ChatSummary, 0, len(byChat))
	for cid, list := range byChat {
		last := list[0]
		cs := ChatSummary{
			ChatID:       cid,
			Title:        last.ChatTitle,
			Count:        len(list),
			LastDate:     last.Date,
			LastDateText: last.DateText,
		}
		if cs.Title == "" {
			cs.Title = fmt.Sprintf("chat_%d", cid)
		}
		chats = append(chats, cs)
	}
	sort.Slice(chats, func(i, j int) bool {
		if chats[i].LastDate != chats[j].LastDate {
			return chats[i].LastDate < chats[j].LastDate
		}
		return chats[i].ChatID < chats[j].ChatID
	})

	ix.mu.Lock()
	ix.entries = entries
	ix.byChat = byChat
	ix.chats = chats
	ix.mu.Unlock()
}

func sortEntries(list []Entry) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Date != list[j].Date {
			return list[i].Date > list[j].Date
		}
		return list[i].MessageID > list[j].MessageID
	})
}

// ChatList 返回频道摘要（按最后同步时间降序）
func (ix *Index) ChatList() []ChatSummary {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]ChatSummary, len(ix.chats))
	copy(out, ix.chats)
	return out
}

// Query 分页查询时间线。chatID=0 查全部；beforeDate/beforeMsgID 为游标（0,0 = 第一页）；
// tags 非空时按标签过滤：matchAll=true 须同时含全部标签（交集），false 含任一即可（并集）。
// 游标始终指向"最后返回的条目"，因此跨页不会漏掉被标签过滤掏空的区间。
func (ix *Index) Query(chatID, beforeDate, beforeMsgID int64, limit int, tags []string, matchAll bool) ([]Entry, *PageCursor, error) {
	if limit <= 0 {
		limit = 50
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	var list []Entry
	if chatID == 0 {
		list = ix.entries
	} else {
		list = ix.byChat[chatID]
	}
	// entries 已按 (date desc, message_id desc) 排序，线性截取即可
	start := 0
	if beforeDate != 0 || beforeMsgID != 0 {
		for start < len(list) {
			e := list[start]
			if e.Date < beforeDate || (e.Date == beforeDate && e.MessageID < beforeMsgID) {
				break
			}
			start++
		}
	}
	page := make([]Entry, 0, limit)
	for i := start; i < len(list) && len(page) < limit; i++ {
		e := list[i]
		if len(tags) > 0 && !captionHasTags(e.Caption, tags, matchAll) {
			continue
		}
		page = append(page, e)
	}
	if len(page) == 0 {
		// 本段全被过滤：若无后续可扫区间则到尾；否则游标指向段尾继续
		if start >= len(list) {
			return page, nil, nil
		}
		last := list[len(list)-1]
		return page, &PageCursor{Date: last.Date, MessageID: last.MessageID}, nil
	}
	if len(page) < limit && start+len(page) >= len(list) {
		return page, nil, nil // 已扫到尾，无下一页
	}
	last := page[len(page)-1]
	return page, &PageCursor{Date: last.Date, MessageID: last.MessageID}, nil
}

// captionHasTag 判断 caption 是否包含 #tag（词边界：tag 后不能紧跟字母数字下划线，
// 避免 #tag2 误匹配 #tag）
func captionHasTag(caption, tag string) bool {
	if tag == "" || caption == "" {
		return false
	}
	low := strings.ToLower(caption)
	t := strings.ToLower(tag)
	for {
		idx := strings.Index(low, "#"+t)
		if idx < 0 {
			return false
		}
		after := idx + 1 + len(t)
		// 词边界：标签后不能紧跟字母/数字/下划线（Unicode），避免 #马赛克图片 命中 #马赛克
		if after >= len(low) || !isTagWordCharAt(low, after) {
			return true
		}
		low = low[after:]
	}
}

// captionHasTags 按多标签过滤：matchAll=true 全部命中（交集），false 任一命中（并集）
func captionHasTags(caption string, tags []string, matchAll bool) bool {
	for _, t := range tags {
		hit := captionHasTag(caption, t)
		if matchAll && !hit {
			return false
		}
		if !matchAll && hit {
			return true
		}
	}
	return matchAll
}

// isTagWordCharAt 判断 caption 的 i 位置是否为标签词内字符（Unicode 字母/数字/下划线）
func isTagWordCharAt(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// ResolvePath 把相对路径解析为下载根目录内的绝对路径。
// 严格校验：拒绝逃逸、绝对路径与不存在文件。
func ResolvePath(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("非法路径: %q", rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界: %q", rel)
	}
	abs := filepath.Join(root, clean)
	if !strings.HasPrefix(abs, filepath.Clean(root)+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界: %q", rel)
	}
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() {
		return "", fmt.Errorf("文件不存在: %q", rel)
	}
	return abs, nil
}