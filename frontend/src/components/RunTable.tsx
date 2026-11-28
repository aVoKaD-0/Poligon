import { statusLabels } from '../data/statusLabels'
import type { Run, RunStatus } from '../types/poligon'

const dateFormat = new Intl.DateTimeFormat('ru-RU', {
  day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit',
})

export function Status({ status }: { status: RunStatus }) {
  return <span className={'status ' + status}>{statusLabels[status]}</span>
}

export function RunTable({ runs, emptyMessage = 'Прогонов пока нет.' }: { runs: Run[]; emptyMessage?: string }) {
  if (!runs.length) return <p className="empty panel" role="status">{emptyMessage}</p>

  return (
    <div className="table-panel" role="region" aria-label="Таблица прогонов" tabIndex={0}>
      <table>
        <thead><tr>
          <th scope="col">Прогон</th><th scope="col">Сценарий</th><th scope="col">Версия агента</th>
          <th scope="col">Результат</th><th scope="col">Запуск</th><th scope="col">Время</th>
        </tr></thead>
        <tbody>{runs.map(run => (
          <tr key={run.id}>
            <td data-label="Прогон"><a className="mono" href={'#run/' + encodeURIComponent(run.id)}>#{run.id}</a></td>
            <td data-label="Сценарий">{run.scenarioName}</td>
            <td data-label="Версия агента"><code>{run.agent}</code></td>
            <td data-label="Результат"><Status status={run.status} /></td>
            <td data-label="Запуск"><time dateTime={run.startedAt}>{dateFormat.format(new Date(run.startedAt))}</time></td>
            <td data-label="Время"><span className="mono">{run.duration === null ? '—' : run.duration + ' с'}</span></td>
          </tr>
        ))}</tbody>
      </table>
    </div>
  )
}
