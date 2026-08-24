import { beforeEach, describe, expect, it, vi } from 'vitest'
import { historyFocusStore, logsStore, stateStore, tasksStore, useStore } from './store'
import type { Task } from './types'

// 一个最小的 EventSource 桩：记录注册的监听器，供测试直接投递事件
class FakeEventSource {
  static last: FakeEventSource | null = null
  listeners = new Map<string, (e: MessageEvent) => void>()
  onerror: (() => void) | null = null
  onopen: (() => void) | null = null
  constructor(readonly url: string) { FakeEventSource.last = this }
  addEventListener(type: string, fn: (e: MessageEvent) => void) { this.listeners.set(type, fn) }
  close() {}
  emit(type: string, data: string) {
    this.listeners.get(type)?.({ data } as MessageEvent)
  }
}

function task(id: string, status = 'running'): Task {
  return {
    id, kind: 'history', chat_id: 1, status, created_at: '2026-08-24T00:00:00Z',
    stats: { total: 1, downloaded: 0, failed: 0, skipped: 0, total_size: 0, downloaded_size: 0 },
  } as Task
}

describe('SSE task 事件', () => {
  beforeEach(() => {
    tasksStore.set([])
    logsStore.set([])
    stateStore.set(null)
    vi.stubGlobal('EventSource', FakeEventSource)
  })

  it('原地替换同 id 的任务而不是追加', async () => {
    const { connectEvents } = await import('./store')
    tasksStore.set([task('a'), task('b')])
    connectEvents()

    FakeEventSource.last!.emit('task', JSON.stringify(task('a', 'completed')))

    const list = tasksStore.get()
    expect(list).toHaveLength(2)
    expect(list.find((t) => t.id === 'a')?.status).toBe('completed')
  })

  it('未知 id 的任务插到列表最前', async () => {
    const { connectEvents } = await import('./store')
    tasksStore.set([task('a')])
    connectEvents()

    FakeEventSource.last!.emit('task', JSON.stringify(task('new')))

    expect(tasksStore.get()[0].id).toBe('new')
    expect(tasksStore.get()).toHaveLength(2)
  })

  // 坏帧不能让整条事件流崩掉：一次 JSON 解析失败之后，后续事件仍要能处理
  it('坏帧被忽略且不影响后续事件', async () => {
    const { connectEvents } = await import('./store')
    connectEvents()

    expect(() => FakeEventSource.last!.emit('task', '{不是 JSON')).not.toThrow()
    FakeEventSource.last!.emit('task', JSON.stringify(task('ok')))
    expect(tasksStore.get().map((t) => t.id)).toEqual(['ok'])
  })

  it('日志条数超过上限时保留最新的那批', async () => {
    const { connectEvents } = await import('./store')
    connectEvents()

    for (let i = 0; i < 1200; i++) {
      FakeEventSource.last!.emit('log', JSON.stringify({ time: '', level: 'info', msg: `m${i}` }))
    }
    const logs = logsStore.get()
    // 上限是 MAX_LOG_ENTRIES(400)：超出后保留最新的那批，而不是无限增长
    expect(logs.length).toBe(400)
    expect(logs[logs.length - 1].msg).toBe('m1199')
  })
})

describe('Store 订阅', () => {
  it('set 会通知全部订阅者，取消订阅后不再收到', () => {
    const seen: string[] = []
    const unsub = historyFocusStore.subscribe((v) => seen.push(v))
    historyFocusStore.set('t1')
    unsub()
    historyFocusStore.set('t2')
    expect(seen).toEqual(['t1'])
    historyFocusStore.set('')
  })
})

describe('useStore', () => {
  it('导出为函数供组件订阅', () => {
    expect(typeof useStore).toBe('function')
  })
})
