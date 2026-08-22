import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { api, fileURL } from '../api'
import {
  ALL_TYPES, HISTORY_STATUS_LABEL, MEDIA_TYPE_LABEL, fmtSize, fmtTime,
} from '../format'
import { chatsStore, toast, useStore } from '../store'
import type { HistoryRecord, MediaTypeStat } from '../types'

const PAGE_SIZES = [20, 50, 100]

export function History() {
  const chats = useStore(chatsStore)
  const [items, setItems] = useState<HistoryRecord[]>([])
  const [stats, setStats] = useState<MediaTypeStat[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [chatID, setChatID] = useState(0)
  const [mediaType, setMediaType] = useState('')
  const [status, setStatus] = useState('')
  const [query, setQuery] = useState('')
  const [debouncedQuery, setDebouncedQuery] = useState('')
  const [loading, setLoading] = useState(false)
  const requestGeneration = useRef(0)

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedQuery(query.trim()), 250)
    return () => clearTimeout(timer)
  }, [query])

  const buildParams = useCallback((withPaging: boolean) => {
    const q = new URLSearchParams()
    if (withPaging) {
      q.set('page', String(page))
      q.set('page_size', String(pageSize))
    }
    if (chatID) q.set('chat_id', String(chatID))
    if (mediaType) q.set('type', mediaType)
    if (status) q.set('status', status)
    if (debouncedQuery) q.set('q', debouncedQuery)
    return q
  }, [page, pageSize, chatID, mediaType, status, debouncedQuery])

  const reload = useCallback(async () => {
    const generation = ++requestGeneration.current
    setLoading(true)
    try {
      const [pageData, statData] = await Promise.all([
        api.history(buildParams(true)),
        api.historyStats(buildParams(false)),
      ])
      if (generation !== requestGeneration.current) return
      setItems(pageData.items || [])
      setTotal(pageData.total)
      setStats(statData.by_type || [])
    } catch (e) {
      if (generation === requestGeneration.current) toast((e as Error).message)
    } finally {
      if (generation === requestGeneration.current) setLoading(false)
    }
  }, [buildParams])

  useEffect(() => {
    void reload()
    return () => { requestGeneration.current++ }
  }, [reload])

  // 改筛选条件时回到第一页，否则会停在一个可能不存在的深页上
  useEffect(() => { setPage(1) }, [chatID, mediaType, status, debouncedQuery, pageSize])

  const pages = Math.max(1, Math.ceil(total / pageSize))
  const totalSize = stats.reduce((a, s) => a + s.total_size, 0)
  const totalFailed = stats.reduce((a, s) => a + s.failed, 0)
  const totalSkipped = stats.reduce((a, s) => a + s.skipped, 0)
  const totalDone = stats.reduce((a, s) => a + s.completed, 0)

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
          <select value={status} onChange={(e) => setStatus(e.currentTarget.value)}>
            <option value="">全部状态</option>
            {Object.entries(HISTORY_STATUS_LABEL).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
          </select>
          <input
            placeholder="搜索文件名…" value={query}
            onInput={(e) => setQuery(e.currentTarget.value)}
          />
          <select value={String(pageSize)} onChange={(e) => setPageSize(parseInt(e.currentTarget.value, 10))}>
            {PAGE_SIZES.map((n) => <option key={n} value={String(n)}>{n} 条/页</option>)}
          </select>
          <span class="grow" />
          <button class="sm" onClick={() => void reload()}>刷新</button>
        </div>

        <div class="row meta mono" style="margin-top:10px;gap:16px">
          <span>共 {total} 条</span>
          <span>已完成 {totalDone}</span>
          {totalFailed > 0 && <span style="color:var(--err)">失败 {totalFailed}</span>}
          {totalSkipped > 0 && <span>跳过 {totalSkipped}</span>}
          <span>总大小 {fmtSize(totalSize)}</span>
        </div>
      </div>

      <div class="card" style="padding:0;overflow-x:auto">
        {items.length === 0 ? (
          <div class="empty">{loading ? '加载中…' : '没有匹配的记录'}</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>文件</th>
                <th>类型</th>
                <th>聊天</th>
                <th>状态</th>
                <th class="num">大小</th>
                <th>时间</th>
              </tr>
            </thead>
            <tbody>
              {items.map((r) => <Row key={r.id} rec={r} />)}
            </tbody>
          </table>
        )}
      </div>

      <div class="row" style="justify-content:center">
        <button class="sm" disabled={page <= 1} onClick={() => setPage((p) => p - 1)}>上一页</button>
        <span class="meta mono">{page} / {pages}</span>
        <button class="sm" disabled={page >= pages} onClick={() => setPage((p) => p + 1)}>下一页</button>
      </div>
    </>
  )
}

function Row({ rec }: { rec: HistoryRecord }) {
  const cls = { completed: 'ok', failed: 'err', skipped: '' }[rec.status] || ''
  return (
    <tr>
      <td>
        {rec.status === 'completed' ? (
          <a href={fileURL(rec.id)} target="_blank" rel="noreferrer" class="truncate" style="display:block">
            {rec.file_name}
          </a>
        ) : (
          <span class="truncate" style="display:block">{rec.file_name}</span>
        )}
        <div class="meta truncate" title={rec.file_path}>{rec.file_path}</div>
      </td>
      <td>{MEDIA_TYPE_LABEL[rec.media_type] || rec.media_type}</td>
      <td class="truncate">{rec.chat_title || rec.chat_id}</td>
      <td>
        <span class={'pill ' + cls}>{HISTORY_STATUS_LABEL[rec.status] || rec.status}</span>
        {/* 失败原因此前一直发给了浏览器却一个字节没用上 */}
        {rec.reason && <div class="meta truncate" title={rec.reason}>{rec.reason}</div>}
      </td>
      <td class="num">{fmtSize(rec.file_size)}</td>
      <td class="meta">{fmtTime(rec.created_at)}</td>
    </tr>
  )
}
