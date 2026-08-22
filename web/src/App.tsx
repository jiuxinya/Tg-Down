import { useEffect, useState } from 'preact/hooks'
import { APIError, api, authRedirecting, beginTokenBootstrap } from './api'
import { Auth } from './components/Auth'
import { Gallery } from './components/Gallery'
import { History } from './components/History'
import { Logs } from './components/Logs'
import { Schedules } from './components/Schedules'
import { SettingsPanel } from './components/Settings'
import { Tasks } from './components/Tasks'
import { CONNECTION_LABEL, fmtSize } from './format'
import {
  connectEvents, loadChats, loadTasks, stateStore, toast, toastStore, useStore,
} from './store'
import type { Settings } from './types'

type Tab = 'tasks' | 'gallery' | 'history' | 'schedules' | 'logs' | 'settings'

const TABS: Array<[Tab, string]> = [
  ['tasks', '任务'],
  ['gallery', '画廊'],
  ['history', '历史'],
  ['schedules', '定时'],
  ['logs', '日志'],
  ['settings', '设置'],
]

const THEME_KEY = 'tg_down_theme'

export function App() {
  const snap = useStore(stateStore)
  const msg = useStore(toastStore)
  const [tab, setTab] = useState<Tab>('tasks')
  const [settings, setSettings] = useState<Settings | null>(null)
  const [accessDenied, setAccessDenied] = useState(false)
  const [eventNotice, setEventNotice] = useState('')
  const [theme, setTheme] = useState(() => {
    try { return localStorage.getItem(THEME_KEY) || 'auto' } catch { return 'auto' }
  })

  useEffect(() => {
    if (theme === 'auto') document.documentElement.removeAttribute('data-theme')
    else document.documentElement.setAttribute('data-theme', theme)
    try { localStorage.setItem(THEME_KEY, theme) } catch { /* 隐私模式，忽略 */ }
  }, [theme])

  useEffect(() => {
    if (authRedirecting) return
    api.state()
      .then(stateStore.set.bind(stateStore))
      .catch((e) => {
        if (e instanceof APIError && e.status === 401) setAccessDenied(true)
        else setEventNotice('无法连接管理端：' + (e as Error).message)
      })
    return connectEvents((status) => {
      if (status === 'unauthorized') {
        setAccessDenied(true)
        setEventNotice('')
      } else if (status === 'reconnecting') {
        setEventNotice('实时连接已断开，正在重连…')
      } else {
        setEventNotice('')
      }
    })
  }, [])

  const ready = snap?.state === 'ready'

  useEffect(() => {
    if (!ready) return
    void loadChats()
    void loadTasks()
    api.settings().then(setSettings).catch(() => {})
  }, [ready])

  const conn = snap?.connection && snap.connection !== 'ready'
    ? CONNECTION_LABEL[snap.connection]
    : ''

  return (
    <div class="app">
      <header class="top">
        <h1>Tg-Down</h1>
        <span class="meta mono">{snap?.version || ''}</span>
        <span class="grow" />
        {ready && (
          <nav class="tabs">
            {TABS.map(([k, label]) => (
              <button key={k} class={tab === k ? 'on' : ''} onClick={() => setTab(k)}>{label}</button>
            ))}
          </nav>
        )}
        <select value={theme} onChange={(e) => setTheme(e.currentTarget.value)} title="主题">
          <option value="auto">跟随系统</option>
          <option value="light">浅色</option>
          <option value="dark">深色</option>
        </select>
        {ready && (
          <button
            class="sm"
            onClick={() => {
              if (confirm('确定要登出并清除本地会话吗？')) {
                api.logout().then(() => location.reload()).catch((e) => toast(e.message))
              }
            }}
          >
            登出
          </button>
        )}
      </header>

      {conn && <div class="banner">{conn}</div>}
      {eventNotice && <div class="banner">{eventNotice}</div>}

      {accessDenied ? (
        <AccessTokenGate />
      ) : !ready ? (
        <Auth snap={snap ?? { state: 'connecting' } as never} />
      ) : (
        <>
          <Overview settings={settings} />
          {tab === 'tasks' && <Tasks />}
          {tab === 'gallery' && <Gallery />}
          {tab === 'history' && <History />}
          {tab === 'schedules' && <Schedules />}
          {tab === 'logs' && <Logs />}
          {tab === 'settings' && <SettingsPanel settings={settings} onChange={setSettings} />}
        </>
      )}

      {msg && <div class="toast">{msg}</div>}
    </div>
  )
}

function AccessTokenGate() {
  const [token, setToken] = useState('')
  const submit = (e: Event) => {
    e.preventDefault()
    if (!token.trim()) { toast('访问令牌不能为空'); return }
    beginTokenBootstrap(token.trim())
  }

  return (
    <form class="card" onSubmit={submit}>
      <h2 style="margin-top:0">访问鉴权</h2>
      <div class="row">
        <input
          type="password" autoComplete="off" autoFocus style="flex:1;min-width:220px"
          placeholder="TG_DOWN_WEB_TOKEN" value={token}
          onInput={(e) => setToken(e.currentTarget.value)}
        />
        <button class="accent">进入管理台</button>
      </div>
    </form>
  )
}

function Overview({ settings }: { settings: Settings | null }) {
  const snap = useStore(stateStore)
  const [conc, setConc] = useState('')
  if (!snap) return null

  const s = snap.stats
  const limit = snap.media_concurrency?.max_concurrent ?? settings?.media_concurrency.max_concurrent ?? 0

  const apply = async () => {
    const n = parseInt(conc, 10)
    if (!n || n < 1) { toast('并发数必须是正整数'); return }
    try {
      await api.setConcurrency(n)
      toast(`并发已设为 ${n}`)
      setConc('')
    } catch (e) {
      toast((e as Error).message)
    }
  }

  return (
    <div class="card">
      <div class="row meta mono" style="gap:18px">
        <span>活动任务 {snap.active_tasks}</span>
        <span>已下载 {s.downloaded}</span>
        {s.failed > 0 && <span style="color:var(--err)">失败 {s.failed}</span>}
        {s.skipped > 0 && <span>跳过 {s.skipped}</span>}
        <span>{fmtSize(s.downloaded_size)}</span>
        <span class="grow" />
        <label class="meta">
          并发 {limit}
          <input
            type="number" min="1" style="width:70px" placeholder={String(limit)}
            value={conc} onInput={(e) => setConc(e.currentTarget.value)}
          />
        </label>
        <button class="sm" onClick={() => void apply()}>应用</button>
      </div>
      {settings && (
        <div class="meta truncate" style="margin-top:6px" title={settings.download_path}>
          下载目录：{settings.download_path}
        </div>
      )}
    </div>
  )
}
