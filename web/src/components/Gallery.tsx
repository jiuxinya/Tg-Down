import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import { api, fileURL, thumbURL } from '../api'
import { ALL_TYPES, MEDIA_TYPE_LABEL, fmtDate, fmtSize } from '../format'
import { chatsStore, toast, useStore } from '../store'
import type { HistoryRecord } from '../types'

const PAGE_SIZE = 60

// 能在浏览器里直接预览的类型；其余（文档/语音等）在灯箱里给下载链接
const VIEWABLE = new Set(['photo', 'video', 'animation', 'video_note', 'sticker'])
const VIDEO_KINDS = new Set(['video', 'animation', 'video_note'])

export function Gallery() {
  const chats = useStore(chatsStore)
  const [items, setItems] = useState<HistoryRecord[]>([])
  const [page, setPage] = useState(1)
  const [total, setTotal] = useState(0)
  const [chatID, setChatID] = useState(0)
  const [mediaType, setMediaType] = useState('')
  const [groupAlbums, setGroupAlbums] = useState(true)
  const [lightbox, setLightbox] = useState<number>(-1)
  const [loading, setLoading] = useState(false)
  const sentinel = useRef<HTMLDivElement>(null)
  const requestGeneration = useRef(0)
  const loadingRef = useRef(false)

  const params = useCallback((p: number) => {
    const q = new URLSearchParams()
    q.set('page', String(p))
    q.set('page_size', String(PAGE_SIZE))
    q.set('status', 'completed') // 画廊只展示真正下载下来的文件
    if (chatID) q.set('chat_id', String(chatID))
    if (mediaType) q.set('type', mediaType)
    return q
  }, [chatID, mediaType])

  // 筛选条件变化 → 从第一页重来
  useEffect(() => {
    const generation = ++requestGeneration.current
    loadingRef.current = true
    setLoading(true)
    setItems([])
    setTotal(0)
    setPage(1)
    api.history(params(1))
      .then((r) => {
        if (generation !== requestGeneration.current) return
        setItems(r.items || [])
        setTotal(r.total)
        setPage(1)
      })
      .catch((e) => {
        if (generation === requestGeneration.current) toast((e as Error).message)
      })
      .finally(() => {
        if (generation === requestGeneration.current) {
          loadingRef.current = false
          setLoading(false)
        }
      })
    return () => {
      if (generation === requestGeneration.current) requestGeneration.current++
    }
  }, [params])

  const loadMore = useCallback(async () => {
    if (loadingRef.current || items.length >= total) return
    const generation = requestGeneration.current
    const nextPage = page + 1
    loadingRef.current = true
    setLoading(true)
    try {
      const r = await api.history(params(nextPage))
      if (generation !== requestGeneration.current) return
      setItems((prev) => appendUniqueHistory(prev, r.items || []))
      setPage(nextPage)
      setTotal(r.total)
    } catch (e) {
      if (generation === requestGeneration.current) toast((e as Error).message)
    } finally {
      if (generation === requestGeneration.current) {
        loadingRef.current = false
        setLoading(false)
      }
    }
  }, [items.length, total, page, params])

  // 滚到底自动加载下一页：十万级历史不可能一次性塞进 DOM
  useEffect(() => {
    const el = sentinel.current
    if (!el) return
    const io = new IntersectionObserver((entries) => {
      if (entries.some((e) => e.isIntersecting)) void loadMore()
    }, { rootMargin: '400px' })
    io.observe(el)
    return () => io.disconnect()
  }, [loadMore])

  const groups = useMemo(() => groupByAlbum(items, groupAlbums), [items, groupAlbums])

  return (
    <>
      <div class="card">
        <div class="row">
          <select value={String(chatID)} onChange={(e) => setChatID(parseInt(e.currentTarget.value, 10))}>
            <option value="0">全部聊天</option>
            {chats.map((c) => <option key={c.id} value={String(c.id)}>{c.title}</option>)}
          </select>
          <select value={mediaType} onChange={(e) => setMediaType(e.currentTarget.value)}>
            <option value="">全部类型</option>
            {ALL_TYPES.map((t) => <option key={t} value={t}>{MEDIA_TYPE_LABEL[t]}</option>)}
          </select>
          <label>
            <input
              type="checkbox" checked={groupAlbums}
              onChange={(e) => setGroupAlbums(e.currentTarget.checked)}
            />
            按相册分组
          </label>
          <span class="grow" />
          <span class="meta mono">已加载 {items.length} / {total}</span>
        </div>
      </div>

      {items.length === 0 && !loading && (
        <div class="card empty">还没有已完成的媒体。先去「任务」页下载点东西吧。</div>
      )}

      {groups.map((g) => (
        <div class="album-group" key={g.key}>
          {g.albumID > 0 && <h3>相册 #{g.albumID} · {g.items.length} 项</h3>}
          <div class="grid">
            {g.items.map((rec) => (
              <Cell key={rec.id} rec={rec} onOpen={() => setLightbox(items.indexOf(rec))} />
            ))}
          </div>
        </div>
      ))}

      <div ref={sentinel} style="height:1px" />
      {loading && <div class="empty">加载中…</div>}

      {lightbox >= 0 && items[lightbox] && (
        <Lightbox
          items={items}
          index={lightbox}
          onIndex={setLightbox}
          onClose={() => setLightbox(-1)}
        />
      )}
    </>
  )
}

interface Group { key: string; albumID: number; items: HistoryRecord[] }

// 请求重试或后端页边界变化时按 history id 去重，避免画廊出现重复格子。
export function appendUniqueHistory(current: HistoryRecord[], incoming: HistoryRecord[]): HistoryRecord[] {
  const seen = new Set(current.map((item) => item.id))
  return current.concat(incoming.filter((item) => {
    if (seen.has(item.id)) return false
    seen.add(item.id)
    return true
  }))
}

// groupByAlbum 把同一相册的媒体聚在一起。album_id 一直躺在库里、文件也一直按
// album_<id> 分目录，此前只是 DTO 没把它发出来，画廊因此拼不出相册。
function groupByAlbum(items: HistoryRecord[], enabled: boolean): Group[] {
  if (!enabled) return [{ key: 'all', albumID: 0, items }]

  const out: Group[] = []
  let cur: Group | null = null
  for (const rec of items) {
    const album = rec.album_id || 0
    // 相册内的项在时间上相邻，顺序遍历即可成组，无需全局重排（那会打乱时间顺序）
    if (cur && cur.albumID === album && album !== 0) {
      cur.items.push(rec)
      continue
    }
    if (album === 0 && cur && cur.albumID === 0) {
      cur.items.push(rec)
      continue
    }
    cur = { key: `${album}-${rec.id}`, albumID: album, items: [rec] }
    out.push(cur)
  }
  return out
}

function Cell({ rec, onOpen }: { rec: HistoryRecord; onOpen: () => void }) {
  const [broken, setBroken] = useState(false)
  const showImg = rec.has_thumb && !broken

  return (
    <div class="cell" onClick={onOpen} title={rec.file_name}>
      {showImg ? (
        <img src={thumbURL(rec.id)} alt={rec.file_name} loading="lazy" onError={() => setBroken(true)} />
      ) : (
        <div class="noimg">{rec.file_name}</div>
      )}
      <span class="kind">{MEDIA_TYPE_LABEL[rec.media_type] || rec.media_type}</span>
      {rec.album_id ? <span class="album">相册</span> : null}
    </div>
  )
}

function Lightbox({
  items, index, onIndex, onClose,
}: {
  items: HistoryRecord[]
  index: number
  onIndex: (i: number) => void
  onClose: () => void
}) {
  const rec = items[index]

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
      if (e.key === 'ArrowLeft' && index > 0) onIndex(index - 1)
      if (e.key === 'ArrowRight' && index < items.length - 1) onIndex(index + 1)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [index, items.length, onIndex, onClose])

  const stop = (e: Event) => e.stopPropagation()
  const url = fileURL(rec.id)

  return (
    <div class="lightbox" onClick={onClose}>
      <div class="lb-bar" onClick={stop}>
        <span class="truncate">{rec.file_name}</span>
        <span class="meta" style="color:#bbb">
          {fmtSize(rec.file_size)} · {fmtDate(rec.created_at)} · {index + 1}/{items.length}
        </span>
        <span class="grow" />
        <a href={url} download={rec.file_name}>下载</a>
        <button class="sm" onClick={onClose}>关闭</button>
      </div>

      {index > 0 && (
        <button class="nav prev" onClick={(e) => { stop(e); onIndex(index - 1) }}>‹</button>
      )}
      {index < items.length - 1 && (
        <button class="nav next" onClick={(e) => { stop(e); onIndex(index + 1) }}>›</button>
      )}

      <div onClick={stop}>
        <Viewer rec={rec} url={url} />
      </div>
    </div>
  )
}

function Viewer({ rec, url }: { rec: HistoryRecord; url: string }) {
  if (VIDEO_KINDS.has(rec.media_type)) {
    // preload=none：灯箱里切换时不要预拉几百 MB 的视频
    return <video src={url} controls autoPlay preload="none" />
  }
  if (VIEWABLE.has(rec.media_type)) {
    return <img src={url} alt={rec.file_name} />
  }
  if (rec.media_type === 'audio' || rec.media_type === 'voice') {
    return <audio src={url} controls autoPlay />
  }
  return (
    <div class="fallback">
      <p>{rec.file_name}</p>
      <p class="meta" style="color:#bbb">{fmtSize(rec.file_size)} · 该类型无法在浏览器内预览</p>
      <a href={url} download={rec.file_name}>下载文件</a>
    </div>
  )
}
