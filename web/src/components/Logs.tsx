import { useEffect, useRef } from 'preact/hooks'
import { clearLogs, logsStore, useStore } from '../store'

export function Logs() {
  const logs = useStore(logsStore)
  const box = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const el = box.current
    if (el) el.scrollTop = el.scrollHeight
  }, [logs])

  return (
    <>
      <div class="row section-tools">
        <strong>运行日志</strong>
        <span class="meta">最近 {logs.length} / 400 条</span>
        <span class="grow" />
        <button class="sm" disabled={logs.length === 0} onClick={clearLogs}>清空</button>
      </div>
      <div class="card log-list" ref={box}>
        {logs.length === 0 ? (
          <div class="empty">暂无运行日志</div>
        ) : logs.map((entry, index) => (
          <div class="log-row" key={`${entry.time}-${index}`}>
            <span class="meta mono">{entry.time}</span>
            <strong class={'log-level ' + entry.level.toLowerCase()}>{entry.level}</strong>
            <span>{entry.msg}</span>
          </div>
        ))}
      </div>
    </>
  )
}
