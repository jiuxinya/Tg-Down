import { useEffect, useState } from 'preact/hooks'
import { api } from '../api'
import { toast } from '../store'
import type { Settings } from '../types'

export function SettingsPanel({
  settings, onChange,
}: {
  settings: Settings | null
  onChange: (settings: Settings) => void
}) {
  const [concurrency, setConcurrency] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (settings) setConcurrency(String(settings.media_concurrency.max_concurrent))
  }, [settings])

  const applyConcurrency = async () => {
    if (!settings) return
    const value = parseInt(concurrency, 10)
    if (!value || value < 1) { toast('并发数必须是正整数'); return }
    setBusy(true)
    try {
      const media = await api.setConcurrency(value)
      onChange({ ...settings, media_concurrency: media })
      toast('并发设置已保存')
    } catch (e) {
      toast((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const toggleClassify = async () => {
    if (!settings) return
    setBusy(true)
    try {
      onChange(await api.setClassify(!settings.classify_by_type))
      toast('分类设置已保存')
    } catch (e) {
      toast((e as Error).message)
    } finally {
      setBusy(false)
    }
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
          <div class="meta">同时下载的媒体数量上限</div>
        </div>
        <div class="row">
          <input
            type="number" min="1" value={concurrency} style="width:90px"
            onInput={(e) => setConcurrency(e.currentTarget.value)}
          />
          <button class="sm" disabled={busy} onClick={() => void applyConcurrency()}>应用</button>
        </div>
      </div>
      <div class="setting-row">
        <div>
          <strong>按媒体类型分类存储</strong>
          <div class="meta">在聊天目录下按 photo、video、document 等类型分目录</div>
        </div>
        <label class="row">
          <input
            type="checkbox" checked={settings.classify_by_type} disabled={busy}
            onChange={() => void toggleClassify()}
          />
          {settings.classify_by_type ? '已开启' : '已关闭'}
        </label>
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
