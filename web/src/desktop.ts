import type { DesktopInfo, InstancesResponse, RemoteInstance, TestResult } from './types'

// 桌面壳 API 客户端。纯网页部署下这些请求 404，调用方以 probeDesktop() 探测后
// 才启用桌面相关 UI；其余组件无需感知桌面环境。

const SELECTED_KEY = 'tg_down_selected_instance'

export const LOCAL_INSTANCE = 'local'

export async function probeDesktop(): Promise<DesktopInfo | null> {
  try {
    const res = await fetch('/desktop/api/info')
    if (!res.ok) return null
    return (await res.json()) as DesktopInfo
  } catch {
    return null
  }
}

export function selectedInstance(): string {
  try { return localStorage.getItem(SELECTED_KEY) || LOCAL_INSTANCE } catch { return LOCAL_INSTANCE }
}

export function setSelectedInstance(id: string) {
  try { localStorage.setItem(SELECTED_KEY, id) } catch { /* 隐私模式，忽略 */ }
}

// apiBase 返回当前实例的引擎 API 前缀：本机为空串（同源直连），远程走壳层反代
export function apiBase(id?: string): string {
  const cur = id ?? selectedInstance()
  return cur === LOCAL_INSTANCE ? '' : `/api/remote/${encodeURIComponent(cur)}`
}

export const desktopApi = {
  instances: () => get<InstancesResponse>('/desktop/api/instances'),
  addInstance: (name: string, url: string) =>
    post<RemoteInstance>('/desktop/api/instances', { name, url }),
  updateInstance: (id: string, patch: { name?: string; url?: string; token?: string }) =>
    put<RemoteInstance>(`/desktop/api/instances/${encodeURIComponent(id)}`, patch),
  deleteInstance: (id: string) =>
    request<void>(`/desktop/api/instances/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  select: (id: string) => post<{ status: string }>('/desktop/api/select', { id }),
  testInstance: (id: string) => post<TestResult>(`/desktop/api/instances/${encodeURIComponent(id)}/test`),
  setAutostart: (enabled: boolean) => put<{ enabled: boolean }>('/desktop/api/autostart', { enabled }),
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, init)
  const text = await res.text()
  let data: unknown = null
  if (text) { try { data = JSON.parse(text) } catch { data = null } }
  if (!res.ok) {
    const msg = (data as { error?: string } | null)?.error
    throw new Error(msg || `HTTP ${res.status}`)
  }
  return data as T
}
const get = <T,>(p: string) => request<T>(p)
const post = <T,>(p: string, body?: unknown) =>
  request<T>(p, { method: 'POST', body: body === undefined ? undefined : JSON.stringify(body) })
const put = <T,>(p: string, body?: unknown) =>
  request<T>(p, { method: 'PUT', body: body === undefined ? undefined : JSON.stringify(body) })
