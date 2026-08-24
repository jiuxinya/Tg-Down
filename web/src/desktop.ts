import type { AutostartState, DesktopInfo, InstancesResponse, RemoteInstance, TestResult } from './types'

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

// storedInstance 返回本地记录的选中项；从未记录过（首次启动或隐私模式）时为 null。
// 壳层注册表才是权威存档，localStorage 只是免去启动时等一次请求的快路径。
export function storedInstance(): string | null {
  try { return localStorage.getItem(SELECTED_KEY) } catch { return null }
}

export function selectedInstance(): string {
  return storedInstance() || LOCAL_INSTANCE
}

// setSelectedInstance 写入快路径；返回是否写成功（隐私模式下会失败）
export function setSelectedInstance(id: string): boolean {
  try {
    localStorage.setItem(SELECTED_KEY, id)
    return localStorage.getItem(SELECTED_KEY) === id
  } catch { return false }
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
  select: (id: string) => post<{ status: string; selected: string }>('/desktop/api/select', { id }),
  testInstance: (id: string) => post<TestResult>(`/desktop/api/instances/${encodeURIComponent(id)}/test`),
  autostart: () => get<AutostartState>('/desktop/api/autostart'),
  setAutostart: (enabled: boolean) => put<AutostartState>('/desktop/api/autostart', { enabled }),
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

// 壳层要求 application/json：不显式声明时浏览器会发 text/plain，那是一个无需预检的
// 简单请求，正是壳层要挡住的跨站写入形态。
const jsonInit = (method: string, body?: unknown): RequestInit =>
  body === undefined
    ? { method }
    : { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }
const post = <T,>(p: string, body?: unknown) => request<T>(p, jsonInit('POST', body))
const put = <T,>(p: string, body?: unknown) => request<T>(p, jsonInit('PUT', body))
