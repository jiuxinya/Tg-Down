import { useEffect, useRef, useState } from 'preact/hooks'
import { clearLogs, logsStore, useStore } from '../store'

const LEVELS: Array<[string, string]> = [
  ['all', '全部'], ['error', '错误'], ['warn', '警告'], ['info', '信息'], ['debug', '调试'],
]

export function Logs() {
  const logs = useStore(logsStore)
  const [level, setLevel] = useState('all')
  const box = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const el = box.current
    if (el) el.scrollTop = el.scrollHeight
  }, [logs, level])

  // error/warn 是排查的重点入口；"错误"视图即报错信息的统一展示位
  const shown = level === 'all' ? logs : logs.filter((l) => l.level.toLowerCase() === level)

  return (
    <>
      <div class="row section-tools">
        <strong>运行日志</strong>
        <span class="meta">
          {level === 'all' ? `最近 ${logs.length} / 400 条` : `${shown.length} 条 ${LEVELS.find(([v]) => v === level)?.[1] ?? ''}`}
        </span>
        <span class="row" role="group">
          {LEVELS.map(([v, label]) => (
            <button key={v} class={'sm' + (level === v ? ' accent' : '')} onClick={() => setLevel(v)}>{label}</button>
          ))}
        </span>
        <span class="grow" />
        <button class="sm" disabled={logs.length === 0} onClick={clearLogs}>清空</button>
      </div>
      <div class="card log-list" ref={box}>
        {shown.length === 0 ? (
          <div class="empty">{level === 'all' ? '暂无运行日志' : '该级别暂无日志'}</div>
        ) : shown.map((entry, index) => (
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
