import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { api, fileURL } from '../api'
import { ChatSelect } from './ChatSelect'
import {
  ALL_TYPES, HISTORY_STATUS_LABEL, MEDIA_TYPE_LABEL, fmtSize, fmtTime,
} from '../format'
import { historyFocusStore, toast, useStore } from '../store'
import type { HistoryRecord, MediaTypeStat } from '../types'

const PAGE_SIZES = [20, 50, 100]

const SORT_LABEL: Record<string, string> = {
  created_desc: '最新在前',
  created_asc: '最早在前',
  size_desc: '最大在前',
  size_asc: '最小在前',
}

export function History() {
  // taskID 由任务卡片的"查看文件"写入；清空即回到全部记录
  const focusTaskID = useStore(historyFocusStore)
  const [items, setItems] = useState<HistoryRecord[]>([])
  const [stats, setStats] = useState<MediaTypeStat[]>([])
  const [total, setTotal] = useState<number | null>(null)
  // cursorStack 保存每一页的起始游标：栈顶是当前页，回退即出栈。
  // 游标分页只能顺序前进，"上一页"靠记住来路实现。
  const [cursorStack, setCursorStack] = useState<string[]>([])
  const [nextCursor, setNextCursor] = useState('')
  const [pageSize, setPageSize] = useState(20)
  const [sort, setSort] = useState('created_desc')
  const [chatID, setChatID] = useState(0)
  const [mediaType, setMediaType] = useState('')
  const [status, setStatus] = useState('')
  const [query, setQuery] = useState('')
  const [debouncedQuery, setDebouncedQuery] = useState('')
  const [loading, setLoading] = useState(false)
  const [exporting, setExporting] = useState(false)
  const requestGeneration = useRef(0)

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedQuery(query.trim()), 250)
    return () => clearTimeout(timer)
  }, [query])

  // filterParams 只含筛选维度，不含分页：统计与导出都按它取，避免翻页影响
  const filterParams = useCallback(() => {
    const q = new URLSearchParams()
    if (chatID) q.set('chat_id', String(chatID))
    if (mediaType) q.set('type', mediaType)
    if (status) q.set('status', status)
    if (debouncedQuery) q.set('q', debouncedQuery)
    if (focusTaskID) q.set('task_id', focusTaskID)
    return q
  }, [chatID, mediaType, status, debouncedQuery, focusTaskID])

  const load = useCallback(async (cursor: string, withTotal: boolean) => {
    const generation = ++requestGeneration.current
    setLoading(true)
    try {
      const params = filterParams()
      params.set('limit', String(pageSize))
      params.set('sort', sort)
      if (cursor) params.set('cursor', cursor)
      if (withTotal) params.set('with_total', '1')

      // 统计只在筛选条件变化时取：它要扫过整个匹配集做分组聚合，
      // 而翻页并不改变统计结果，每页都发一次等于白跑一遍全表聚合
      const [pageData, statData] = await Promise.all([
        api.history(params),
        withTotal ? api.historyStats(filterParams()) : Promise.resolve(null),
      ])
      if (generation !== requestGeneration.current) return
      setItems(pageData.items || [])
      setNextCursor(pageData.next_cursor || '')
      if (withTotal) {
        setTotal(pageData.total ?? 0)
        setStats(statData?.by_type || [])
      }
    } catch (e) {
      if (generation === requestGeneration.current) toast((e as Error).message)
    } finally {
      if (generation === requestGeneration.current) setLoading(false)
    }
  }, [filterParams, pageSize, sort])

  // 筛选/排序/页大小变化：回到第一页并重新取总数与统计
  useEffect(() => {
    setCursorStack([])
    void load('', true)
  }, [filterParams, pageSize, sort])

  const goNext = () => {
    if (!nextCursor) return
    setCursorStack((s) => [...s, nextCursor])
    void load(nextCursor, false)
  }

  const goPrev = () => {
    if (cursorStack.length === 0) return
    const rest = cursorStack.slice(0, -1)
    setCursorStack(rest)
    void load(rest.length > 0 ? rest[rest.length - 1] : '', false)
  }

  const exportRows = async (format: 'csv' | 'json') => {
    setExporting(true)
    try {
      const params = filterParams()
      params.set('sort', sort)
      params.set('format', format)
      await api.exportHistory(params)
      toast('已导出到下载目录')
    } catch (e) {
      toast((e as Error).message)
    } finally {
      setExporting(false)
    }
  }

  const totalSize = stats.reduce((a, s) => a + s.total_size, 0)
  const totalFailed = stats.reduce((a, s) => a + s.failed, 0)
  const totalSkipped = stats.reduce((a, s) => a + s.skipped, 0)
  const totalDone = stats.reduce((a, s) => a + s.completed, 0)

  return (
    <>
      <div class="card">
        {focusTaskID && (
          <div class="row meta" style="margin-bottom:8px">
            <span>正在查看任务 <code>{focusTaskID.slice(0, 8)}</code> 下载的文件</span>
            <button class="sm" onClick={() => historyFocusStore.set('')}>显示全部记录</button>
          </div>
        )}
        <div class="row">
          <ChatSelect value={chatID} onChange={setChatID} placeholder="全部聊天" />
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
          <select value={sort} onChange={(e) => setSort(e.currentTarget.value)}>
            {Object.entries(SORT_LABEL).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
          </select>
          <select value={String(pageSize)} onChange={(e) => setPageSize(parseInt(e.currentTarget.value, 10))}>
            {PAGE_SIZES.map((n) => <option key={n} value={String(n)}>{n} 条/页</option>)}
          </select>
          <span class="grow" />
          <button class="sm" disabled={exporting} onClick={() => void exportRows('csv')}>导出 CSV</button>
          <button class="sm" disabled={exporting} onClick={() => void exportRows('json')}>导出 JSON</button>
          <button class="sm" onClick={() => { setCursorStack([]); void load('', true) }}>刷新</button>
        </div>

        <div class="row meta mono" style="margin-top:10px;gap:16px">
          <span>共 {total ?? '—'} 条</span>
          <span>已完成 {totalDone}</span>
          {totalFailed > 0 && <span style="color:var(--err)">失败 {totalFailed}</span>}
          {totalSkipped > 0 && <span>跳过 {totalSkipped}</span>}
          <span>总大小 {fmtSize(totalSize)}</span>
        </div>

        {stats.length > 0 && (
          <div class="row meta mono" style="margin-top:6px;gap:12px;flex-wrap:wrap">
            {stats.map((s) => (
              <span key={s.media_type} title={`完成 ${s.completed} / 失败 ${s.failed} / 跳过 ${s.skipped}`}>
                {MEDIA_TYPE_LABEL[s.media_type] || s.media_type} {s.count}
                <span style="opacity:.6"> · {fmtSize(s.total_size)}</span>
              </span>
            ))}
          </div>
        )}
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
        <button class="sm" disabled={cursorStack.length === 0 || loading} onClick={goPrev}>上一页</button>
        <span class="meta mono">第 {cursorStack.length + 1} 页</span>
        <button class="sm" disabled={!nextCursor || loading} onClick={goNext}>下一页</button>
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
