import { useEffect, useState } from 'preact/hooks'
import { api } from '../api'
import { toast } from '../store'
import type { Settings, SettingsUpdate } from '../types'

const LOG_LEVELS: Array<[string, string]> = [
  ['debug', '调试'], ['info', '信息'], ['warn', '警告'], ['error', '错误'],
]

export function SettingsPanel({
  settings, onChange,
}: {
  settings: Settings | null
  onChange: (settings: Settings) => void
}) {
  const [concurrency, setConcurrency] = useState('')
  const [taskConc, setTaskConc] = useState('')
  const [autoRetry, setAutoRetry] = useState('')
  const [proxy, setProxy] = useState('')
  const [webhook, setWebhook] = useState('')
  const [logLevel, setLogLevel] = useState('info')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!settings) return
    setConcurrency(String(settings.media_concurrency.max_concurrent))
    setTaskConc(settings.task_concurrency != null ? String(settings.task_concurrency) : '')
    setAutoRetry(settings.auto_retry != null ? String(settings.auto_retry) : '')
    setProxy(settings.proxy ?? '')
    setWebhook(settings.notify_webhook_url ?? '')
    setLogLevel(settings.log_level ?? 'info')
  }, [settings])

  // apply 提交部分更新并回写快照；后端附带的提示（重启生效等）逐条弹出
  const apply = async (patch: SettingsUpdate, okMsg: string) => {
    setBusy(true)
    try {
      const resp = await api.updateSettings(patch)
      onChange(resp.settings)
      if (resp.notices?.length) {
        toast(resp.notices.join('；'))
      } else {
        toast(okMsg)
      }
    } catch (e) {
      toast((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const toggleClassify = () => {
    if (settings) void apply({ classify_by_type: !settings.classify_by_type }, '分类设置已保存')
  }

  const toggleMetadata = () => {
    if (settings) void apply({ save_metadata: !settings.save_metadata }, '元数据设置已保存')
  }

  const toggleNotifySelf = () => {
    if (settings) void apply({ notify_telegram_self: !settings.notify_telegram_self }, '通知设置已保存')
  }

  const logout = async () => {
    if (!confirm('确定要登出并清除本地会话吗？')) return
    setBusy(true)
    try {
      await api.logout()
      location.reload()
    } catch (e) {
      toast((e as Error).message)
      setBusy(false)
    }
  }

  if (!settings) return <div class="card empty">正在加载设置…</div>

  return (
    <div class="card settings-list">
      <div class="setting-row">
        <div>
          <strong>下载目录</strong>
          <div class="meta">媒体文件的保存位置</div>
        </div>
        <code class="truncate" title={settings.download_path}>{settings.download_path}</code>
      </div>
      <div class="setting-row">
        <div>
          <strong>最大并发下载</strong>
          <div class="meta">同时下载的媒体数量上限，立即生效</div>
        </div>
        <div class="row">
          <input
            type="number" min="1" value={concurrency} style="width:90px"
            onInput={(e) => setConcurrency(e.currentTarget.value)}
          />
          <button class="sm" disabled={busy}
            onClick={() => void apply({ max_concurrent: parseInt(concurrency, 10) || 0 }, '并发设置已保存')}>
            应用
          </button>
        </div>
      </div>
      <div class="setting-row">
        <div>
          <strong>按媒体类型分类存储</strong>
          <div class="meta">在聊天目录下按 photo、video、document 等类型分目录</div>
        </div>
        <label class="row">
          <input type="checkbox" checked={settings.classify_by_type} disabled={busy} onChange={toggleClassify} />
          {settings.classify_by_type ? '已开启' : '已关闭'}
        </label>
      </div>
      <div class="setting-row">
        <div>
          <strong>元数据 sidecar</strong>
          <div class="meta">为每个下载文件旁写 .json 元数据，对后续任务生效</div>
        </div>
        <label class="row">
          <input type="checkbox" checked={!!settings.save_metadata} disabled={busy} onChange={toggleMetadata} />
          {settings.save_metadata ? '已开启' : '已关闭'}
        </label>
      </div>
      <div class="setting-row">
        <div>
          <strong>并行任务数 / 自动重试</strong>
          <div class="meta">同时运行的历史任务数与失败重试上限，重启生效</div>
        </div>
        <div class="row">
          <input
            type="number" min="1" title="并行任务数" placeholder="任务数" style="width:90px"
            value={taskConc} onInput={(e) => setTaskConc(e.currentTarget.value)}
          />
          <input
            type="number" min="0" title="自动重试次数" placeholder="重试" style="width:90px"
            value={autoRetry} onInput={(e) => setAutoRetry(e.currentTarget.value)}
          />
          <button class="sm" disabled={busy}
            onClick={() => void apply({
              task_concurrency: parseInt(taskConc, 10) || undefined,
              auto_retry: autoRetry === '' ? undefined : parseInt(autoRetry, 10),
            }, '队列设置已保存')}>
            应用
          </button>
        </div>
      </div>
      <div class="setting-row">
        <div>
          <strong>连接代理</strong>
          <div class="meta mono">socks5:// 或 http://，留空直连；重启生效</div>
        </div>
        <div class="row">
          <input
            type="text" spellcheck={false} style="width:min(300px,50vw)"
            placeholder={settings.proxy ? '' : '未配置'}
            value={proxy} onInput={(e) => setProxy(e.currentTarget.value)}
          />
          <button class="sm" disabled={busy} onClick={() => void apply({ proxy }, '代理设置已保存')}>应用</button>
        </div>
      </div>
      <div class="setting-row">
        <div>
          <strong>日志级别</strong>
          <div class="meta">立即生效并持久化</div>
        </div>
        <div class="row">
          <select value={logLevel} onChange={(e) => setLogLevel(e.currentTarget.value)}>
            {LOG_LEVELS.map(([v, label]) => <option key={v} value={v}>{label}</option>)}
          </select>
          <button class="sm" disabled={busy} onClick={() => void apply({ log_level: logLevel }, '日志级别已保存')}>应用</button>
        </div>
      </div>
      <div class="setting-row">
        <div>
          <strong>完成通知</strong>
          <div class="meta">任务完成或最终失败时推送到 Saved Messages 与 webhook，对后续任务生效</div>
        </div>
        <div class="col" style="gap:6px;min-width:260px">
          <label class="row">
            <input type="checkbox" checked={!!settings.notify_telegram_self} disabled={busy} onChange={toggleNotifySelf} />
            发送到我的 Saved Messages
          </label>
          <div class="row">
            <input
              type="text" spellcheck={false} style="flex:1;min-width:180px"
              placeholder="webhook 地址" value={webhook}
              onInput={(e) => setWebhook(e.currentTarget.value)}
            />
            <button class="sm" disabled={busy}
              onClick={() => void apply({ notify_webhook_url: webhook }, 'webhook 已保存')}>应用</button>
          </div>
        </div>
      </div>
      <div class="setting-row">
        <div>
          <strong>Telegram 会话</strong>
          <div class="meta">停止活动任务并清除当前登录会话</div>
        </div>
        <button class="sm danger" disabled={busy} onClick={() => void logout()}>登出</button>
      </div>
    </div>
  )
}
