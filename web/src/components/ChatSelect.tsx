import { useState } from 'preact/hooks'
import { chatsStore, refreshChats, useStore } from '../store'

/**
 * ChatSelect 是聊天下拉的唯一实现。
 *
 * 此前 Tasks / History / Gallery / Schedules 四处各写了一遍，占位文案、样式与是否带搜索
 * 各不相同——同一个控件在不同页面行为不一致，改一处也不会自动同步到其余三处。
 */
export function ChatSelect({
  value,
  onChange,
  placeholder,
  searchable = false,
  refreshable = false,
  style,
}: {
  value: number
  onChange: (id: number) => void
  placeholder: string
  /** 聊天很多时给一个本地过滤框；仅建任务这类需要精确定位的场景需要 */
  searchable?: boolean
  /** 是否附带刷新按钮 */
  refreshable?: boolean
  style?: string
}) {
  const chats = useStore(chatsStore)
  const [filter, setFilter] = useState('')
  const [refreshing, setRefreshing] = useState(false)

  const visible = searchable && filter.trim()
    ? chats.filter((c) => c.title.toLowerCase().includes(filter.trim().toLowerCase()))
    : chats

  const doRefresh = async () => {
    setRefreshing(true)
    try {
      await refreshChats()
    } finally {
      setRefreshing(false)
    }
  }

  return (
    <>
      {searchable && (
        <input
          placeholder="筛选聊天…"
          value={filter}
          onInput={(e) => setFilter(e.currentTarget.value)}
          style="max-width:160px"
        />
      )}
      <select
        value={String(value)}
        onChange={(e) => onChange(parseInt(e.currentTarget.value, 10))}
        style={style}
      >
        <option value="0">{placeholder}</option>
        {visible.map((c) => <option key={c.id} value={String(c.id)}>{c.title}</option>)}
      </select>
      {refreshable && (
        <button class="sm" disabled={refreshing} onClick={() => void doRefresh()}>
          {refreshing ? '刷新中…' : '刷新聊天'}
        </button>
      )}
    </>
  )
}
