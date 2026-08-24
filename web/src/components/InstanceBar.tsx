import { useEffect, useState } from 'preact/hooks'
import {
  LOCAL_INSTANCE, desktopApi, selectedInstance, setSelectedInstance, storedInstance,
} from '../desktop'
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
      if (!alive) return
      setInstances(r.instances)
      reconcileSelection(r.selected)
    }).catch(() => {})
    return () => { alive = false }
  }, [])

  if (!info) return null

  const refreshList = () => {
    desktopApi.instances().then((r) => setInstances(r.instances)).catch(() => {})
  }

  const switchTo = (id: string) => {
    if (id === selected) return
    const previous = selected
    setSelected(id)
    desktopApi.select(id).then(() => {
      setSelectedInstance(id)
      // 整页重载：清空各 store 并让 SSE/认证流程按新实例重新建立
      location.reload()
    }).catch((e) => {
      setSelected(previous)
      toast('切换实例失败：' + (e as Error).message)
    })
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
          info={info}
          onClose={() => { setManaging(false); refreshList() }}
        />
      )}
    </>
  )
}

// reconcileSelection 对齐两处选中项。启动时用的是 localStorage（同步可读，不用等请求），
// 因此以它为准把注册表推平；localStorage 不可用时反过来采纳注册表的值并重载一次。
function reconcileSelection(registrySelected: string) {
  const local = storedInstance()
  if (local === null) {
    if (registrySelected === LOCAL_INSTANCE) return
    if (!setSelectedInstance(registrySelected)) return // 隐私模式写不进去，重载也没用
    location.reload()
    return
  }
  if (local !== registrySelected) {
    desktopApi.select(local).catch(() => {
      // 记录的实例可能已被删除，回落本机
      setSelectedInstance(LOCAL_INSTANCE)
    })
  }
}

function ManageModal({ info, onClose }: { info: DesktopInfo; onClose: () => void }) {
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
      // 用新增接口返回的实例：服务端会规范化地址（补 scheme、补结尾斜杠），
      // 按提交的原文回列表里找是找不着的，令牌会被静默丢掉，实例随后一直 401。
      const added = await desktopApi.addInstance(name.trim(), url.trim())
      if (token.trim()) {
        try {
          await desktopApi.updateInstance(added.id, { token: token.trim() })
        } catch (e) {
          // 实例已经建好了，报错要说清楚失败的是哪一步
          toast('实例已添加，但令牌保存失败：' + (e as Error).message)
          reload()
          return
        }
      }
      setName(''); setUrl(''); setToken('')
      reload()
      toast('实例已添加')
    } catch (e) {
      toast('添加实例失败：' + (e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const remove = async (id: string, label: string) => {
    if (!confirm(`删除实例「${label}」？仅移除本机记录，不影响远端服务。`)) return
    try {
      await desktopApi.deleteInstance(id)
      if (selectedInstance() === id) {
        setSelectedInstance(LOCAL_INSTANCE)
        await desktopApi.select(LOCAL_INSTANCE).catch(() => {})
      }
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
      reload()
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

        <AutostartRow info={info} />
      </div>
    </div>
  )
}

// AutostartRow 开机自启开关。此前只有托盘菜单能改，--no-tray 或托盘不可用的桌面环境下
// 完全没有入口。
function AutostartRow({ info }: { info: DesktopInfo }) {
  const [enabled, setEnabled] = useState(info.autostart)
  const [error, setError] = useState(info.autostart_error ?? '')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    desktopApi.autostart()
      .then((r) => { setEnabled(r.enabled); setError('') })
      .catch((e) => setError((e as Error).message))
  }, [])

  const toggle = async (next: boolean) => {
    setBusy(true)
    try {
      // 回显服务端回读的真实状态，而不是这次点击的意图
      const r = await desktopApi.setAutostart(next)
      setEnabled(r.enabled)
      setError('')
      toast(r.enabled ? '已开启开机自启' : '已关闭开机自启')
    } catch (e) {
      setError((e as Error).message)
      toast('设置开机自启失败：' + (e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div class="setting-row">
      <div>
        <strong>开机自启</strong>
        <div class="meta">登录系统时自动启动 Tg-Down（{info.goos}）</div>
        {error && <div class="meta" style="color:var(--danger,#c33)">状态不可用：{error}</div>}
      </div>
      <label class="row" style="gap:6px">
        <input type="checkbox" checked={enabled} disabled={busy}
          onChange={(e) => void toggle(e.currentTarget.checked)} />
        <span>{enabled ? '已开启' : '未开启'}</span>
      </label>
    </div>
  )
}
