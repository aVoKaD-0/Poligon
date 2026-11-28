import { useState } from 'react'
import { statusLabels } from '../data/statusLabels'
import type { Run, RunStatus } from '../types/poligon'
import { RunTable } from './RunTable'

interface HistoryProps {
  runs: Run[]
}

export function HistoryPage({ runs }: HistoryProps) {
  const [query, setQuery] = useState('')
  const [status, setStatus] = useState('all')
  const search = query.trim().toLowerCase()
  const matches = runs.filter(run => (status === 'all' || status === run.status)
    && ('#' + run.id + ' ' + run.scenarioName + ' ' + run.agent).toLowerCase().includes(search))

  return (
    <>
      <div className="page-heading"><h1>История прогонов</h1><p>Найдите прогон по сценарию, версии агента или идентификатору.</p></div>
      <div className="filters">
        <label>Поиск <input type="search" value={query} onChange={event => setQuery(event.target.value)} placeholder="Сценарий, версия или ID" /></label>
        <label><span id="history-status-label">Результат</span><select aria-labelledby="history-status-label" value={status} onChange={event => setStatus(event.target.value)}>
          <option value="all">Все результаты</option>
          {(Object.keys(statusLabels) as RunStatus[]).map(key => <option key={key} value={key}>{statusLabels[key]}</option>)}
        </select></label>
      </div>
      <p className="result-count" role="status">Найдено: <span className="mono">{matches.length}</span></p>
      <RunTable runs={matches} emptyMessage={search || status !== 'all' ? 'Прогоны по этим условиям не найдены.' : undefined} />
    </>
  )
}
