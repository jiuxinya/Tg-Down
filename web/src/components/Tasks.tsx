import { useState } from 'preact/hooks'
import { api } from '../api'
import {
  ALL_TYPES, MEDIA_TYPE_LABEL, TASK_STATUS_LABEL,
  fmtDuration, fmtSize, fmtSpeed, fmtTime, pct,
} from '../format'
import { ChatSelect } from './ChatSelect'
import {
  chatsStore, historyFocusStore, loadTasks, stateStore, tasksStore, toast, useStore,
} from '../store'
import type { HistoryFilters, MediaProgress, Task } from '../types'

export function Tasks() {
  const tasks = useStore(tasksStore)
  const snap = useStore(stateStore)

  return (
    <>
      <NewTask />
      <ActiveMedia media={snap?.media || []} allPaused={snap?.all_paused} />

      {tasks.length === 0 ? (
        <div class="card empty">还没有任务。</div>
      ) : (
        tasks.map((t) => <TaskCard key={t.id} task={t} />)
      )}
    </>
  )
}

function NewTask() {
  const chats = useStore(chatsStore)
  const tasks = useStore(tasksStore)
  const [chatID, setChatID] = useState(0)
  const [link, setLink] = useState('')
  const [showFilters, setShowFilters] = useState(false)
  const [types, setTypes] = useState<string[]>([])
  const [dateFrom, setDateFrom] = useState('')
  const [dateTo, setDateTo] = useState('')
  const [maxMB, setMaxMB] = useState('')
  const [query, setQuery] = useState('')
  const [sender, setSender] = useState('')
  const [busy, setBusy] = useState(false)

  const activeMonitor = tasks.find((task) => (
    task.kind === 'monitor' && task.chat_id === chatID &&
    (task.status === 'queued' || task.status === 'running')
  ))

  const collect = (): HistoryFilters | undefined => {
    const f: HistoryFilters = {}
    if (types.length) f.media_types = types
    if (dateFrom) f.date_from = Math.floor(new Date(dateFrom + 'T00:00:00').getTime() / 1000)
    if (dateTo) f.date_to = Math.floor(new Date(dateTo + 'T23:59:59').getTime() / 1000)
    const mb = parseFloat(maxMB)
    if (mb > 0) f.max_file_size = Math.floor(mb * 1024 * 1024)
    if (query.trim()) f.query = query.trim()
    const s = parseInt(sender, 10)
    if (s) f.sender_id = s
    return Object.keys(f).length ? f : undefined
  }

  const create = async (kind: string) => {
    if (!chatID) { toast('请先选择聊天'); return }
    setBusy(true)
    try {
      const title = chats.find((c) => c.id === chatID)?.title
      await api.createTask({
        kind, chat_id: chatID, chat_title: title,
        filters: kind === 'history' ? collect() : undefined,
      })
      toast(kind === 'history' ? '已提交历史下载' : '已开始实时监控')
      void loadTasks()
    } catch (e) {
      toast((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const resolveAndCreate = async () => {
    if (!link.trim()) { toast('请粘贴 t.me 链接或 @用户名'); return }
    setBusy(true)
    try {
      const t = await api.resolve(link.trim())
      await api.createTask({
        kind: 'history', chat_id: t.chat_id, chat_title: t.chat_title,
        message_id: t.message_id, filters: t.message_id ? undefined : collect(),
      })
      toast(t.message_id ? '已提交单条消息下载' : '已提交历史下载')
      setLink('')
      void loadTasks()
    } catch (e) {
      toast((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const stopMonitor = async () => {
    if (!activeMonitor) return
    setBusy(true)
    try {
      await api.cancelTask(activeMonitor.id)
      toast('实时监控已停止')
      await loadTasks()
    } catch (e) {
      toast((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const doExport = async () => {
    if (!chatID) { toast('请先选择聊天'); return }
    const raw = prompt('最多导出多少条消息？（0 = 全部；大频道建议先试 1000）', '1000')
    if (raw === null) return
    const limit = parseInt(raw, 10)
    if (isNaN(limit) || limit < 0) { toast('请输入不小于 0 的整数'); return }

    setBusy(true)
    toast('正在导出，需要翻阅整条历史，请稍候…')
    try {
      const title = chats.find((c) => c.id === chatID)?.title || ''
      const r = await api.exportChat(chatID, title, limit)
      toast(`已导出 ${r.messages} 条消息（${r.media_on_disk}/${r.media_count} 个媒体已在本地）→ ${r.html_path}`)
    } catch (e) {
      toast('导出失败: ' + (e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const toggleType = (t: string) => {
    setTypes((prev) => (prev.includes(t) ? prev.filter((x) => x !== t) : prev.concat(t)))
  }

  return (
    <div class="card">
      <div class="row">
        <ChatSelect
          value={chatID} onChange={setChatID} placeholder="选择聊天…"
          searchable refreshable style="flex:1;min-width:200px"
        />
        <button class="accent" disabled={busy} onClick={() => void create('history')}>下载历史媒体</button>
        {activeMonitor ? (
          <button class="danger" disabled={busy} onClick={() => void stopMonitor()}>停止监控</button>
        ) : (
          <button class="tint" disabled={busy} onClick={() => void create('monitor')}>开启监控</button>
        )}
        <button disabled={busy} onClick={() => setShowFilters((v) => !v)}>过滤器</button>
        <button disabled={busy} onClick={() => void doExport()} title="导出为 JSON + 可离线打开的 HTML">导出聊天</button>
      </div>

      <div class="row" style="margin-top:10px">
        <input
          style="flex:1;min-width:240px" placeholder="粘贴 t.me 链接或 @用户名（消息链接只下载该条消息）"
          value={link} onInput={(e) => setLink(e.currentTarget.value)}
        />
        <button class="tint" disabled={busy} onClick={() => void resolveAndCreate()}>解析并下载</button>
      </div>

      {showFilters && (
        <div style="margin-top:12px;border-top:1px solid var(--border);padding-top:12px">
          <div class="row">
            <span class="meta">媒体类型（不勾选 = 默认类型）:</span>
            {ALL_TYPES.map((t) => (
              <label key={t} title={t === 'sticker' ? 'Telegram 不支持按贴纸做服务端检索：勾选后需遍历整条历史，且无法预估总数' : undefined}>
                <input type="checkbox" checked={types.includes(t)} onChange={() => toggleType(t)} />
                {MEDIA_TYPE_LABEL[t]}{t === 'sticker' ? '（完整扫描）' : ''}
              </label>
            ))}
          </div>
          <div class="row" style="margin-top:10px">
            <label class="meta">起始日期 <input type="date" value={dateFrom} onInput={(e) => setDateFrom(e.currentTarget.value)} /></label>
            <label class="meta">结束日期 <input type="date" value={dateTo} onInput={(e) => setDateTo(e.currentTarget.value)} /></label>
            <label class="meta">单文件上限(MB) <input type="number" min="0" style="width:90px" value={maxMB} onInput={(e) => setMaxMB(e.currentTarget.value)} /></label>
            <label class="meta">关键词 <input style="width:130px" placeholder="如 报告" value={query} onInput={(e) => setQuery(e.currentTarget.value)} /></label>
            <label class="meta">发送者 ID <input type="number" style="width:120px" value={sender} onInput={(e) => setSender(e.currentTarget.value)} /></label>
          </div>
        </div>
      )}
    </div>
  )
}

function ActiveMedia({ media, allPaused }: { media: MediaProgress[]; allPaused?: boolean }) {
  if (media.length === 0) return null

  return (
    <div class="card">
      <div class="row" style="margin-bottom:10px">
        <strong>正在下载 ({media.length})</strong>
        <span class="grow" />
        {allPaused ? (
          <button class="sm" onClick={() => api.resumeAll().catch((e) => toast(e.message))}>全部恢复</button>
        ) : (
          <button class="sm" onClick={() => api.pauseAll().catch((e) => toast(e.message))}>全部暂停</button>
        )}
      </div>
      {media.map((m) => {
        const p = pct(m.downloaded_size, m.file_size)
        const paused = m.status === 'paused'
        return (
          <div key={m.id} style="margin-bottom:10px">
            <div class="row" style="gap:8px">
              <span class="truncate" style="flex:1">{m.file_name}</span>
              <span class="meta mono">
                {fmtSize(m.downloaded_size)} / {fmtSize(m.file_size)}
                {m.speed_bps ? ` · ${fmtSpeed(m.speed_bps)}` : ''}
                {m.eta_seconds ? ` · 剩 ${fmtDuration(m.eta_seconds)}` : ''}
              </span>
              <button
                class="sm"
                onClick={() => {
                  const fn = paused ? api.resumeMedia : api.pauseMedia
                  fn(m.id).catch((e: Error) => toast(e.message))
                }}
              >
                {paused ? '恢复' : '暂停'}
              </button>
            </div>
            <div class={'bar' + (paused ? ' warn' : '')} style="margin-top:4px">
              <i style={`width:${p}%`} />
            </div>
          </div>
        )
      })}
    </div>
  )
}

function statusPill(status: string) {
  const cls = { completed: 'ok', partial: 'warn', failed: 'err', running: 'run' }[status] || ''
  return <span class={'pill ' + cls}>{TASK_STATUS_LABEL[status] || status}</span>
}

function filterChips(t: Task): string {
  const bits: string[] = []
  if (t.message_id) bits.push('单条消息')
  const f = t.filters
  if (f) {
    if (f.media_types?.length) bits.push('类型:' + f.media_types.map((x) => MEDIA_TYPE_LABEL[x] || x).join('/'))
    if (f.date_from) bits.push('自 ' + new Date(f.date_from * 1000).toLocaleDateString('zh-CN'))
    if (f.date_to) bits.push('至 ' + new Date(f.date_to * 1000).toLocaleDateString('zh-CN'))
    if (f.max_file_size) bits.push('≤' + Math.round(f.max_file_size / 1048576) + 'MB')
    if (f.query) bits.push('关键词:' + f.query)
    if (f.sender_id) bits.push('发送者:' + f.sender_id)
  }
  return bits.join(' · ')
}

function TaskCard({ task: t }: { task: Task }) {
  const s = t.stats
  const done = s.downloaded + s.skipped + s.failed
  const total = t.expected_total || s.total
  const scanning = t.kind === 'history' && (
    t.phase === 'counting' || (t.status === 'running' && s.total === 0)
  )
  const monitorIdle = t.kind === 'monitor' && t.status === 'running' && done === 0
  const chips = filterChips(t)

  return (
    <div class="card">
      <div class="row">
        <strong>{t.chat_title || `聊天 ${t.chat_id}`}</strong>
        {statusPill(t.status)}
        <span class="meta">{t.kind === 'monitor' ? '实时监控' : '历史下载'}</span>
        <span class="grow" />
        <span class="meta">{fmtTime(t.created_at)}</span>
        {done > 0 && (
          <button
            class="sm" title="在历史页查看本任务下载的文件"
            onClick={() => historyFocusStore.set(t.id)}
          >
            查看文件
          </button>
        )}
        {done > 0 && (
          <button
            class="sm danger" title="删除本任务的下载历史记录（磁盘文件保留）"
            onClick={() => {
              if (!confirm(`清除本任务的下载历史记录？\n（${done} 条记录将被删除，磁盘文件保留）`)) return
              api.clearTaskHistory(t.id)
                .then((r) => { toast(`已清除 ${r.deleted} 条记录`); void loadTasks() })
                .catch((e) => toast((e as Error).message))
            }}
          >
            清除记录
          </button>
        )}
        {(t.status === 'queued' || t.status === 'running') && (
          <button class="sm danger" onClick={() => api.cancelTask(t.id).then(loadTasks).catch((e) => toast(e.message))}>
            取消
          </button>
        )}
        {(t.status === 'failed' || t.status === 'canceled' || t.status === 'partial') && (
          <button
            class="sm"
            onClick={() => api.retryTask(t.id).then(loadTasks).catch((e) => toast(e.message))}
          >
            {t.status === 'partial' ? '补下失败文件' : '重试'}
          </button>
        )}
      </div>

      {chips && <div class="meta" style="margin-top:4px">{chips}</div>}

      {monitorIdle ? (
        <div class="meta" style="margin-top:8px">
          正在监听新消息，暂时没有新的媒体文件。
        </div>
      ) : scanning ? (
        <div class="meta" style="margin-top:8px">
          正在扫描历史… 已翻阅 {t.scanned_messages || 0} 条消息，发现 {t.found_media || 0} 个媒体
        </div>
      ) : (
        <>
          <div class="bar" style="margin-top:8px">
            <i style={`width:${pct(done, total)}%`} />
          </div>
          <div class="row meta mono" style="margin-top:6px;gap:14px">
            <span>已下载 {s.downloaded}</span>
            {s.skipped > 0 && <span>跳过 {s.skipped}</span>}
            {s.failed > 0 && <span style="color:var(--err)">失败 {s.failed}</span>}
            <span>共 {total || '?'}</span>
            <span class="grow" />
            {t.speed_bps ? <span>{fmtSpeed(t.speed_bps)}</span> : null}
            {t.eta_seconds ? <span>剩余 {fmtDuration(t.eta_seconds)}</span> : null}
            <span>{fmtSize(s.downloaded_size)}</span>
          </div>
        </>
      )}

      {t.status === 'partial' && (
        <div class="banner" style="margin-top:10px;margin-bottom:0">
          有 {s.failed} 个文件下载失败。点「补下失败文件」只重试这些文件，不会重扫整条历史。
        </div>
      )}
      {t.error && <div class="meta" style="margin-top:6px;color:var(--err)">{t.error}</div>}
    </div>
  )
}
