import { describe, expect, it } from 'vitest'
import { ALL_TYPES, fmtDuration, fmtSize, fmtSpeed, pct } from './format'

describe('fmtSize', () => {
  it('零与缺省值给出 0 B 而不是 NaN', () => {
    expect(fmtSize(0)).toBe('0 B')
    expect(fmtSize(undefined as unknown as number)).toBe('0 B')
  })

  it('按 1024 进位并逐级换单位', () => {
    expect(fmtSize(512)).toBe('512 B')
    expect(fmtSize(1024)).toBe('1.0 KB')
    expect(fmtSize(1024 * 1024)).toBe('1.0 MB')
    expect(fmtSize(1024 ** 4)).toBe('1.0 TB')
  })

  it('超出最大单位后不再进位', () => {
    expect(fmtSize(1024 ** 5)).toBe('1024.0 TB')
  })
})

describe('fmtSpeed', () => {
  // 速度为 0 时返回空串而非 "0 B/s"：任务卡片据此决定是否渲染这一列，
  // 返回 "0 B/s" 会让每个排队中的任务都挂一个假的速率
  it('无速度时返回空串', () => {
    expect(fmtSpeed(0)).toBe('')
    expect(fmtSpeed(undefined)).toBe('')
    expect(fmtSpeed(-1)).toBe('')
  })

  it('有速度时带 /s 后缀', () => {
    expect(fmtSpeed(1024)).toBe('1.0 KB/s')
  })
})

describe('fmtDuration', () => {
  it('不可估算时返回空串', () => {
    expect(fmtDuration(0)).toBe('')
    expect(fmtDuration(undefined)).toBe('')
    expect(fmtDuration(Infinity)).toBe('')
  })

  it('按秒/分/时分级', () => {
    expect(fmtDuration(45)).toBe('45 秒')
    expect(fmtDuration(90)).toBe('1 分 30 秒')
    expect(fmtDuration(3700)).toBe('1 时 1 分')
  })
})

describe('pct', () => {
  it('总数为 0 时不产生 NaN', () => {
    expect(pct(0, 0)).toBe(0)
  })

  it('不超过 100', () => {
    expect(pct(150, 100)).toBeLessThanOrEqual(100)
  })
})

describe('ALL_TYPES', () => {
  // 与后端 media.AllTypes 对应；贴纸可选但必须在列表里（用户要能显式勾选）
  it('包含全部八种媒体类型', () => {
    expect(ALL_TYPES).toHaveLength(8)
    expect(ALL_TYPES).toContain('sticker')
    expect(ALL_TYPES).toContain('video_note')
  })
})
