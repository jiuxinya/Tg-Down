import { useEffect, useState } from 'preact/hooks'
import { LOCAL_INSTANCE, desktopApi, selectedInstance, setSelectedInstance } from '../desktop'
import type { DesktopInfo, RemoteInstance } from '../types'
import { toast } from '../store'

// InstanceBar 桌面环境下的实例切换器与管理入口。纯网页部署（probeDesktop 失败）不渲染。
export function InstanceBar({ info }: { info: DesktopInfo | null }) {
  const [instances, setInstances] = useState<RemoteInstance[]>([])
  const [selected, setSelected] = useState(selectedInstance())
  const [managing, setManaging] = useState(false)

  useEffect(() => {
    let alive = true
    desktopApi.instances().then((r) => {
      if (alive) setInstances(r.instances)
    }).catch(() => {})
    return () => { alive = false }
  }, [])

  if (!info) return null

  const refreshList = () => {
    desktopApi.instances().then((r) => setInstances(r.instances)).catch(() => {})
  }

  const switchTo = (id: string) => {
    if (id === selected) return
    setSelected(id)
    setSelectedInstance(id)
    // 整页重载：清空各 store 并让 SSE/认证流程按新实例重新建立
    location.reload()
  }

  return (
    <>
      <select value={selected} onChange={(e) => switchTo(e.currentTarget.value)} title="切换下载实例">
        <option value={LOCAL_INSTANCE}>本机</option>
        {instances.map((it) => (
          <option key={it.id} value={it.id}>{it.name}</option>
        ))}
      </select>
      <button class="sm" onClick={() => setManaging(true)}>实例…</button>
      {managing && (
        <ManageModal
          onClose={() => { setManaging(false); refreshList() }}
        />
      )}
    </>
  )
}

function ManageModal({ onClose }: { onClose: () => void }) {
  const [items, setItems] = useState<RemoteInstance[]>([])
  const [name, setName] = useState('')
  const [url, setUrl] = useState('')
  const [token, setToken] = useState('')
  const [busy, setBusy] = useState(false)

  const reload = () => {
    desktopApi.instances().then((r) => setItems(r.instances)).catch((e) => toast(e.message))
  }
  useEffect(reload, [])

  const add = async () => {
    if (!url.trim()) { toast('地址不能为空'); return }
    setBusy(true)
    try {
      await desktopApi.addInstance(name.trim(), url.trim())
      if (token.trim()) {
        const list = await desktopApi.instances()
        const added = list.instances.find((i) => i.url === url.trim())
        if (added) await desktopApi.updateInstance(added.id, { token: token.trim() })
      }
      setName(''); setUrl(''); setToken('')
      reload()
      toast('实例已添加')
    } catch (e) {
      toast((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const remove = async (id: string, label: string) => {
    if (!confirm(`删除实例「${label}」？仅移除本机记录，不影响远端服务。`)) return
    try {
      await desktopApi.deleteInstance(id)
      if (selectedInstance() === id) setSelectedInstance(LOCAL_INSTANCE)
      reload()
    } catch (e) {
      toast((e as Error).message)
    }
  }

  const test = async (id: string, label: string) => {
    try {
      const r = await desktopApi.testInstance(id)
      if (r.ok) toast(`「${label}」连接正常 (${r.version || '?'} / ${r.state || '?'})`)
      else toast(`「${label}」连接失败：${r.error}`)
    } catch (e) {
      toast((e as Error).message)
    }
  }

  const saveToken = async (id: string, label: string) => {
    const t = prompt(`「${label}」的访问令牌（TG_DOWN_WEB_TOKEN，留空清除）：`) ?? ''
    try {
      await desktopApi.updateInstance(id, { token: t })
      toast(t ? '令牌已保存' : '令牌已清除')
    } catch (e) {
      toast((e as Error).message)
    }
  }

  const rename = async (it: RemoteInstance) => {
    const n = prompt('实例名称：', it.name)
    if (n === null || !n.trim()) return
    try {
      await desktopApi.updateInstance(it.id, { name: n.trim() })
      reload()
    } catch (e) {
      toast((e as Error).message)
    }
  }

  return (
    <div
      style="position:fixed;inset:0;background:rgba(0,0,0,.35);display:flex;align-items:center;justify-content:center;z-index:50"
      onClick={(e) => { if (e.target === e.currentTarget) onClose() }}
    >
      <div class="card" style="width:min(620px,92vw);max-height:80vh;overflow:auto;margin:0">
        <div class="row section-tools">
          <strong>远程实例</strong>
          <span class="meta">管理 Docker 部署的 Tg-Down 实例</span>
          <span class="grow" />
          <button class="sm" onClick={onClose}>关闭</button>
        </div>

        {items.length === 0 && <div class="meta" style="padding:8px 0">还没有添加远程实例</div>}
        {items.map((it) => (
          <div class="setting-row" key={it.id}>
            <div>
              <strong>{it.name}</strong>
              <div class="meta mono">{it.url}</div>
              <div class="meta">{it.has_token ? '已配置令牌' : '未配置令牌'}</div>
            </div>
            <div class="row">
              <button class="sm" onClick={() => void test(it.id, it.name)}>测试</button>
              <button class="sm" onClick={() => void rename(it)}>改名</button>
              <button class="sm" onClick={() => void saveToken(it.id, it.name)}>令牌</button>
              <button class="sm danger" onClick={() => void remove(it.id, it.name)}>删除</button>
            </div>
          </div>
        ))}

        <div class="setting-row" style="align-items:flex-start">
          <div>
            <strong>添加实例</strong>
            <div class="meta">例如 http://192.168.1.10:8080</div>
          </div>
          <div class="col" style="gap:6px;min-width:260px">
            <input placeholder="名称（可选）" value={name} onInput={(e) => setName(e.currentTarget.value)} />
            <input placeholder="地址" value={url} onInput={(e) => setUrl(e.currentTarget.value)} />
            <input placeholder="访问令牌（可选）" type="password" autoComplete="off" value={token}
              onInput={(e) => setToken(e.currentTarget.value)} />
            <button class="sm accent" disabled={busy} onClick={() => void add()}>添加</button>
          </div>
        </div>
      </div>
    </div>
  )
}
