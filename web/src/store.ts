import { useEffect, useState } from 'preact/hooks'
import { APIError, api, withToken } from './api'
import type { Chat, LogEntry, StateSnapshot, Task } from './types'

// 极简全局 store：一个可订阅的值 + useStore hook。
// 引 preact/signals 只为这点状态并不划算，标准库够用。
class Store<T> {
  private listeners = new Set<(v: T) => void>()
  constructor(private value: T) {}
  get(): T { return this.value }
  set(v: T) {
    this.value = v
    this.listeners.forEach((fn) => fn(v))
  }
  subscribe(fn: (v: T) => void): () => void {
    this.listeners.add(fn)
    return () => { this.listeners.delete(fn) }
  }
}

export function useStore<T>(store: Store<T>): T {
  const [v, setV] = useState(store.get())
  useEffect(() => store.subscribe(setV), [store])
  return v
}

export const stateStore = new Store<StateSnapshot | null>(null)
export const tasksStore = new Store<Task[]>([])
export const chatsStore = new Store<Chat[]>([])
export const logsStore = new Store<LogEntry[]>([])
/**
 * historyFocusStore 是任务卡片下钻到历史页的传递通道：任务卡片写入 task_id，
 * 历史页读取后按它预置筛选。两个组件不相邻，走 store 比逐层传 props 简单。
 */
export const historyFocusStore = new Store<string>('')

export const toastStore = new Store<string>('')

const MAX_LOG_ENTRIES = 400

let toastTimer: number | undefined
export function toast(msg: string) {
  toastStore.set(msg)
  clearTimeout(toastTimer)
  toastTimer = window.setTimeout(() => toastStore.set(''), 4000)
}

export async function loadTasks() {
  try { tasksStore.set(await api.tasks()) } catch { /* SSE 会补上 */ }
}

export async function loadChats() {
  try { chatsStore.set(await api.chats()) } catch { /* 未登录时正常失败 */ }
}

export async function refreshChats() {
  chatsStore.set(await api.refreshChats())
}

export function clearLogs() {
  logsStore.set([])
}

// connectEvents 订阅 SSE。
//
// 与旧版的关键差异：task 事件的 payload 直接被消费（增量更新那一个任务），
// 而不是丢掉 payload 再去 /api/tasks 全量重拉一遍——那会在下载几千个文件时
// 把浏览器打满。
export type EventConnectionStatus = 'open' | 'reconnecting' | 'unauthorized'

export function connectEvents(onStatus?: (status: EventConnectionStatus) => void) {
  const es = new EventSource(withToken('/api/events'))
  let probeTimer: number | undefined

  es.onopen = () => onStatus?.('open')

  es.addEventListener('state', (e) => {
    try { stateStore.set(JSON.parse((e as MessageEvent).data)) } catch { /* 忽略坏帧 */ }
  })

  es.addEventListener('task', (e) => {
    try {
      const t = JSON.parse((e as MessageEvent).data) as Task
      const list = tasksStore.get()
      const i = list.findIndex((x) => x.id === t.id)
      if (i >= 0) {
        const next = list.slice()
        next[i] = t
        tasksStore.set(next)
      } else {
        tasksStore.set([t, ...list])
      }
    } catch { /* 忽略坏帧 */ }
  })

  es.addEventListener('log', (e) => {
    try {
      const entry = JSON.parse((e as MessageEvent).data) as LogEntry
      const next = logsStore.get().concat(entry)
      logsStore.set(next.length > MAX_LOG_ENTRIES ? next.slice(-MAX_LOG_ENTRIES) : next)
    } catch { /* 忽略坏帧 */ }
  })

  es.onerror = () => {
    // EventSource 自带重连，这里只在断流时把任务列表补齐，避免错过中间事件
    onStatus?.('reconnecting')
    setTimeout(loadTasks, 3000)
    if (probeTimer === undefined) {
      probeTimer = window.setTimeout(async () => {
        probeTimer = undefined
        try {
          stateStore.set(await api.state())
        } catch (e) {
          if (e instanceof APIError && e.status === 401) {
            onStatus?.('unauthorized')
            es.close()
          }
        }
      }, 1000)
    }
  }
  return () => {
    if (probeTimer !== undefined) clearTimeout(probeTimer)
    es.close()
  }
}
