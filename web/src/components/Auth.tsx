import { useEffect, useState } from 'preact/hooks'
import { api } from '../api'
import { toast } from '../store'
import type { StateSnapshot } from '../types'

// Auth 覆盖未登录的三个阶段：填凭据 → 输验证码 → 输二步验证密码。
export function Auth({ snap }: { snap: StateSnapshot }) {
  const [lastError, setLastError] = useState('')
  useEffect(() => {
    if (snap.state === 'error') setLastError(snap.error || '未知错误')
  }, [snap.state, snap.error])

  switch (snap.state) {
    case 'need_credentials':
      return <Credentials />
    case 'waiting_code':
      return <CodeStep phone={snap.phone} />
    case 'waiting_password':
      return <PasswordStep />
    case 'error':
      return <Credentials error={snap.error || '未知错误'} />
    default:
      if (lastError) return <Credentials error={lastError} />
      return (
        <div class="card empty">
          正在连接 Telegram…
        </div>
      )
  }
}

function Credentials({ error = '' }: { error?: string }) {
  const [apiID, setApiID] = useState('')
  const [apiHash, setApiHash] = useState('')
  const [phone, setPhone] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (e: Event) => {
    e.preventDefault()
    const id = parseInt(apiID, 10)
    if (!id || !apiHash.trim() || !phone.trim()) {
      toast('api_id、api_hash、手机号均不能为空')
      return
    }
    setBusy(true)
    try {
      await api.submitCredentials(id, apiHash.trim(), phone.trim())
      toast('已提交，正在登录…')
    } catch (err) {
      toast((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <form class="card" onSubmit={submit}>
      <h2 style="margin-top:0">登录 Telegram</h2>
      {error && <div class="banner" style="color:var(--err)">连接失败：{error}</div>}
      <p class="meta">
        在 <a href="https://my.telegram.org" target="_blank" rel="noreferrer">my.telegram.org</a> 申请
        api_id / api_hash。凭据会写入 config.yaml。
      </p>
      <div class="row" style="margin-top:10px">
        <input
          type="number" min="1" max="2147483647" autoComplete="off" placeholder="api_id"
          value={apiID} onInput={(e) => setApiID(e.currentTarget.value)}
        />
        <input
          type="password" minLength={32} maxLength={32} autoComplete="off"
          placeholder="api_hash" style="flex:1;min-width:220px"
          value={apiHash} onInput={(e) => setApiHash(e.currentTarget.value)}
        />
        <input
          type="tel" autoComplete="tel" placeholder="手机号（+8613800138000）"
          value={phone} onInput={(e) => setPhone(e.currentTarget.value)}
        />
        <button class="accent" disabled={busy}>{busy ? '提交中…' : error ? '重新登录' : '登录'}</button>
      </div>
    </form>
  )
}

function CodeStep({ phone }: { phone?: string }) {
  const [code, setCode] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (e: Event) => {
    e.preventDefault()
    if (!code.trim()) return
    setBusy(true)
    try {
      await api.submitCode(code.trim())
    } catch (err) {
      toast((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <form class="card" onSubmit={submit}>
      <h2 style="margin-top:0">输入验证码</h2>
      <p class="meta">Telegram 已向 {phone || '你的账号'} 发送验证码。</p>
      <div class="row" style="margin-top:10px">
        <input
          placeholder="验证码" inputMode="numeric" autoComplete="one-time-code" autoFocus
          value={code} onInput={(e) => setCode(e.currentTarget.value)}
        />
        <button class="accent" disabled={busy}>提交</button>
        <button
          type="button" class="tint"
          onClick={() => api.abortAuth().catch((e) => toast((e as Error).message))}
        >返回上一步</button>
      </div>
    </form>
  )
}

function PasswordStep() {
  const [pw, setPw] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (e: Event) => {
    e.preventDefault()
    if (!pw) return
    setBusy(true)
    try {
      await api.submitPassword(pw)
    } catch (err) {
      toast((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <form class="card" onSubmit={submit}>
      <h2 style="margin-top:0">两步验证</h2>
      <p class="meta">该账号开启了两步验证密码。</p>
      <div class="row" style="margin-top:10px">
        <input
          type="password" autoComplete="current-password" placeholder="两步验证密码" autoFocus
          value={pw} onInput={(e) => setPw(e.currentTarget.value)}
        />
        <button class="accent" disabled={busy}>提交</button>
        <button
          type="button" class="tint"
          onClick={() => api.abortAuth().catch((e) => toast((e as Error).message))}
        >返回上一步</button>
      </div>
    </form>
  )
}
