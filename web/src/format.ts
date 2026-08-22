export const MEDIA_TYPE_LABEL: Record<string, string> = {
  photo: '图片',
  video: '视频',
  document: '文档',
  animation: '动图',
  audio: '音频',
  voice: '语音',
  video_note: '视频消息',
  sticker: '贴纸',
}

// ALL_TYPES 与后端 media.AllTypes 对应；sticker 不在"下载全部"的默认集里
// （TDLib 没有贴纸的服务端检索，纳入默认会让每个任务退化成遍历整条历史）。
export const ALL_TYPES = Object.keys(MEDIA_TYPE_LABEL)

export const TASK_STATUS_LABEL: Record<string, string> = {
  queued: '排队中',
  running: '下载中',
  completed: '已完成',
  partial: '部分失败',
  failed: '失败',
  canceled: '已取消',
}

export const HISTORY_STATUS_LABEL: Record<string, string> = {
  queued: '排队中',
  downloading: '下载中',
  completed: '已完成',
  failed: '失败',
  skipped: '已跳过',
}

export const CONNECTION_LABEL: Record<string, string> = {
  connecting: '正在连接 Telegram…',
  updating: '正在同步…',
  waiting_network: '等待网络…',
}

export function fmtSize(bytes: number): string {
  if (!bytes) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = bytes
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

export function fmtSpeed(bps?: number): string {
  if (!bps || bps <= 0) return ''
  return fmtSize(bps) + '/s'
}

export function fmtDuration(sec?: number): string {
  if (!sec || sec <= 0 || !isFinite(sec)) return ''
  const s = Math.round(sec)
  if (s < 60) return `${s} 秒`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} 分 ${s % 60} 秒`
  const h = Math.floor(m / 60)
  return `${h} 时 ${m % 60} 分`
}

export function fmtTime(v?: string | number): string {
  if (!v) return '-'
  const d = typeof v === 'number' ? new Date(v * 1000) : new Date(v)
  if (isNaN(d.getTime())) return '-'
  return d.toLocaleString('zh-CN', { hour12: false })
}

export function fmtDate(v?: number): string {
  if (!v) return '-'
  return new Date(v * 1000).toLocaleDateString('zh-CN')
}

export function pct(done: number, total: number): number {
  if (!total || total <= 0) return 0
  return Math.min(100, Math.round((done / total) * 100))
}
