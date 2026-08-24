import { useEffect, useState } from 'preact/hooks'
import { api } from '../api'
import { ALL_TYPES, MEDIA_TYPE_LABEL, fmtTime } from '../format'
import { ChatSelect } from './ChatSelect'
import { chatsStore, toast, useStore } from '../store'
import type { HistoryFilters, Schedule } from '../types'

const MIN_INTERVAL = 10

export function Schedules() {
  const chats = useStore(chatsStore)
  const [list, setList] = useState<Schedule[]>([])
  const [chatID, setChatID] = useState(0)
  const [interval, setInterval] = useState('60')
  const [busy, setBusy] = useState(false)
  const [showFilters, setShowFilters] = useState(false)
  const [types, setTypes] = useState<string[]>([])
  const [dateFrom, setDateFrom] = useState('')
  const [dateTo, setDateTo] = useState('')
  const [maxMB, setMaxMB] = useState('')
  const [query, setQuery] = useState('')
  const [sender, setSender] = useState('')

  const reload = async () => {
    try { setList(await api.schedules()) } catch (e) { toast((e as Error).message) }
  }
  useEffect(() => { void reload() }, [])

  const collectFilters = (): HistoryFilters | undefined => {
    const filters: HistoryFilters = {}
    if (types.length) filters.media_types = types
    if (dateFrom) filters.date_from = Math.floor(new Date(dateFrom + 'T00:00:00').getTime() / 1000)
    if (dateTo) filters.date_to = Math.floor(new Date(dateTo + 'T23:59:59').getTime() / 1000)
    const mb = parseFloat(maxMB)
    if (mb > 0) filters.max_file_size = Math.floor(mb * 1024 * 1024)
    if (query.trim()) filters.query = query.trim()
    const senderID = parseInt(sender, 10)
    if (senderID) filters.sender_id = senderID
    return Object.keys(filters).length ? filters : undefined
  }

  const create = async () => {
    const min = parseInt(interval, 10)
    if (!chatID) { toast('请先选择聊天'); return }
    if (!min || min < MIN_INTERVAL) { toast(`间隔不能小于 ${MIN_INTERVAL} 分钟`); return }
    setBusy(true)
    try {
      await api.createSchedule({
        chat_id: chatID,
        chat_title: chats.find((c) => c.id === chatID)?.title,
        interval_min: min,
        filters: collectFilters(),
      })
      toast('已创建定时计划')
      await reload()
    } catch (e) {
      toast((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <div class="card">
        <div class="row">
          <ChatSelect
            value={chatID} onChange={setChatID}
            placeholder="选择聊天…" style="flex:1;min-width:200px"
          />
          <label class="meta">
            每
            <input
              type="number" min={String(MIN_INTERVAL)} style="width:80px"
              value={interval} onInput={(e) => setInterval(e.currentTarget.value)}
            />
            分钟
          </label>
          <button class="accent" disabled={busy} onClick={() => void create()}>创建计划</button>
          <button disabled={busy} onClick={() => setShowFilters((value) => !value)}>过滤器</button>
        </div>
        {showFilters && (
          <div class="filter-panel">
            <div class="row">
              <span class="meta">媒体类型</span>
              {ALL_TYPES.map((type) => (
                <label key={type}>
                  <input
                    type="checkbox" checked={types.includes(type)}
                    onChange={() => setTypes((current) => (
                      current.includes(type) ? current.filter((item) => item !== type) : current.concat(type)
                    ))}
                  />
                  {MEDIA_TYPE_LABEL[type]}
                </label>
              ))}
            </div>
            <div class="row" style="margin-top:10px">
              <label class="meta">起始日期 <input type="date" value={dateFrom} onInput={(e) => setDateFrom(e.currentTarget.value)} /></label>
              <label class="meta">结束日期 <input type="date" value={dateTo} onInput={(e) => setDateTo(e.currentTarget.value)} /></label>
              <label class="meta">单文件上限(MB) <input type="number" min="0" style="width:90px" value={maxMB} onInput={(e) => setMaxMB(e.currentTarget.value)} /></label>
              <label class="meta">关键词 <input style="width:130px" value={query} onInput={(e) => setQuery(e.currentTarget.value)} /></label>
              <label class="meta">发送者 ID <input type="number" style="width:120px" value={sender} onInput={(e) => setSender(e.currentTarget.value)} /></label>
            </div>
          </div>
        )}
      </div>

      {list.length === 0 ? (
        <div class="card empty">还没有定时计划。</div>
      ) : (
        list.map((s) => (
          <div class="card" key={s.id}>
            <div class="row">
              <strong>{s.chat_title || `聊天 ${s.chat_id}`}</strong>
              <span class={'pill ' + (s.enabled ? 'ok' : '')}>{s.enabled ? '已启用' : '已停用'}</span>
              <span class="meta">每 {s.interval_min} 分钟</span>
              <span class="grow" />
              <span class="meta">上次触发 {s.last_run ? fmtTime(s.last_run) : '从未'}</span>
              <button
                class="sm"
                onClick={() => api.toggleSchedule(s.id, !s.enabled).then(reload).catch((e) => toast(e.message))}
              >
                {s.enabled ? '停用' : '启用'}
              </button>
              <button
                class="sm danger"
                onClick={() => api.deleteSchedule(s.id).then(reload).catch((e) => toast(e.message))}
              >
                删除
              </button>
            </div>
            {s.last_max_id ? (
              <div class="meta mono" style="margin-top:4px">增量水位：消息 {s.last_max_id}</div>
            ) : null}
            {scheduleFilterSummary(s.filters) && (
              <div class="meta" style="margin-top:4px">{scheduleFilterSummary(s.filters)}</div>
            )}
          </div>
        ))
      )}
    </>
  )
}

function scheduleFilterSummary(raw?: string): string {
  if (!raw) return ''
  try {
    const filters = JSON.parse(raw) as HistoryFilters
    const parts: string[] = []
    if (filters.media_types?.length) {
      parts.push(filters.media_types.map((type) => MEDIA_TYPE_LABEL[type] || type).join('/'))
    }
    if (filters.date_from) parts.push('自 ' + new Date(filters.date_from * 1000).toLocaleDateString('zh-CN'))
    if (filters.date_to) parts.push('至 ' + new Date(filters.date_to * 1000).toLocaleDateString('zh-CN'))
    if (filters.max_file_size) parts.push('不超过 ' + Math.round(filters.max_file_size / 1048576) + ' MB')
    if (filters.query) parts.push('关键词 ' + filters.query)
    if (filters.sender_id) parts.push('发送者 ' + filters.sender_id)
    return parts.join(' · ')
  } catch {
    return '过滤器数据不可读'
  }
}
