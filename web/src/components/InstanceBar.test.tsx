import { fireEvent, render, screen, waitFor } from '@testing-library/preact'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

// desktopApi 在模块加载时就被组件捕获，必须在 import 组件之前打桩
const addInstance = vi.fn()
const updateInstance = vi.fn()
const instances = vi.fn()
const deleteInstance = vi.fn()
const select = vi.fn()
const testInstance = vi.fn()
const autostart = vi.fn()
const setAutostart = vi.fn()

vi.mock('../desktop', async () => {
  const actual = await vi.importActual<typeof import('../desktop')>('../desktop')
  return {
    ...actual,
    desktopApi: {
      instances,
      addInstance,
      updateInstance,
      deleteInstance,
      select,
      testInstance,
      setAutostart,
      autostart,
      show: vi.fn(),
    },
  }
})

const toast = vi.fn()
vi.mock('../store', async () => {
  const actual = await vi.importActual<typeof import('../store')>('../store')
  return { ...actual, toast: (msg: string) => toast(msg) }
})

const { InstanceBar } = await import('./InstanceBar')

const info = {
  version: 'v3.2.0', goos: 'darwin', goarch: 'arm64',
  app_dir: '/tmp', autostart: false, local: 'http://127.0.0.1:1',
}

async function openManager() {
  render(<InstanceBar info={info} />)
  await waitFor(() => expect(screen.getByText('实例…')).toBeInTheDocument())
  fireEvent.click(screen.getByText('实例…'))
  await waitFor(() => expect(screen.getByPlaceholderText(/地址/)).toBeInTheDocument())
}

function fillAndSubmit(url: string, token: string) {
  fireEvent.input(screen.getByPlaceholderText(/地址/), { target: { value: url } })
  if (token) {
    fireEvent.input(screen.getByPlaceholderText(/令牌/), { target: { value: token } })
  }
  fireEvent.click(screen.getByText('添加'))
}

describe('新增远程实例', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    instances.mockResolvedValue({ selected: 'local', instances: [] })
    autostart.mockResolvedValue({ enabled: false })
    setAutostart.mockResolvedValue({ enabled: false })
    localStorage.clear()
  })
  afterEach(() => vi.clearAllMocks())

  // 这是 v3.2 修复的那个 bug 的回归用例：此前代码丢弃 addInstance 的返回值，
  // 改用提交的原始 URL 去列表里匹配，而服务端会把地址规范化（补 scheme 与尾斜杠），
  // 匹配必然失败 → 令牌不写入、不报错、仍弹"实例已添加"，实例随后一直 401。
  it('令牌用新增接口返回的 id 保存，不受服务端地址规范化影响', async () => {
    addInstance.mockResolvedValue({
      id: 'inst-1',
      name: 'NAS',
      url: 'http://192.168.1.10:8080/', // 注意：服务端补了尾斜杠
      has_token: false,
    })
    updateInstance.mockResolvedValue({})

    await openManager()
    fillAndSubmit('http://192.168.1.10:8080', 'a-very-secret-token')

    await waitFor(() => expect(updateInstance).toHaveBeenCalledTimes(1))
    expect(updateInstance).toHaveBeenCalledWith('inst-1', { token: 'a-very-secret-token' })
    expect(toast).toHaveBeenCalledWith('实例已添加')
  })

  it('不填令牌时不调用更新接口', async () => {
    addInstance.mockResolvedValue({ id: 'inst-2', name: '', url: 'http://x/', has_token: false })

    await openManager()
    fillAndSubmit('http://x', '')

    await waitFor(() => expect(addInstance).toHaveBeenCalledTimes(1))
    expect(updateInstance).not.toHaveBeenCalled()
  })

  // 令牌保存失败必须说清楚是哪一步失败：实例已经建好了，
  // 笼统报"添加失败"会让用户以为要重新添加，结果建出两个实例
  it('令牌保存失败时给出区分性的提示，且不报成功', async () => {
    addInstance.mockResolvedValue({ id: 'inst-3', name: '', url: 'http://y/', has_token: false })
    updateInstance.mockRejectedValue(new Error('写入失败'))

    await openManager()
    fillAndSubmit('http://y', 'tok')

    await waitFor(() => expect(toast).toHaveBeenCalled())
    const messages = toast.mock.calls.map((c) => String(c[0]))
    expect(messages.some((m) => m.includes('令牌保存失败'))).toBe(true)
    expect(messages).not.toContain('实例已添加')
  })

  it('新增失败时不弹成功提示', async () => {
    addInstance.mockRejectedValue(new Error('地址不可达'))

    await openManager()
    fillAndSubmit('http://bad', 'tok')

    await waitFor(() => expect(toast).toHaveBeenCalled())
    const messages = toast.mock.calls.map((c) => String(c[0]))
    expect(messages.some((m) => m.includes('添加实例失败'))).toBe(true)
    expect(messages).not.toContain('实例已添加')
    expect(updateInstance).not.toHaveBeenCalled()
  })

  it('地址为空时不发请求', async () => {
    await openManager()
    fireEvent.click(screen.getByText('添加'))
    await waitFor(() => expect(toast).toHaveBeenCalledWith('地址不能为空'))
    expect(addInstance).not.toHaveBeenCalled()
  })
})
