import type { RunReport } from '../types/poligon'
import { Status } from './RunTable'

interface ScenarioReportProps {
  report: RunReport
  canRepeat: boolean
  onRepeat: () => void
}

export function ScenarioReportPage({ report: { run, expected, actual, events }, canRepeat, onRepeat }: ScenarioReportProps) {
  return (
    <>
      <div className="page-heading report-heading">
        <div>
          <p className="mono run-id">#{run.id}</p>
          <h1>{run.scenarioName}</h1>
          <div className="report-meta"><Status status={run.status} /><code>{run.agent}</code><span className="mono">{run.duration === null ? '—' : run.duration + ' с'}</span></div>
        </div>
        <button className="primary" disabled={!canRepeat} onClick={onRepeat}>Повторить прогон</button>
      </div>
      <div className="report-grid">
        <section className="panel report-panel"><h2>Ожидаемый результат</h2><p>{expected}</p></section>
        <section className={'panel report-panel ' + (run.status === 'defect' ? 'warning-panel' : '')}>
          <h2>Фактический результат</h2><p>{actual ?? 'Результат пока не получен.'}</p>
        </section>
      </div>
      <section className="block" aria-labelledby="timeline-title">
        <div className="section-heading"><h2 id="timeline-title">Хронология прогона</h2></div>
        {!events.length && <p className="empty panel">Событий пока нет.</p>}
        {events.length > 0 && <ol className="timeline panel">
          {events.map((event, index) => (
            <li className={event.warning ? 'event-warning' : undefined} key={index}>
              <time className="mono">{event.time}</time>
              <div><strong>{event.source}</strong>{event.operation && <code className="operation">{event.operation}</code>}
                <p>{event.message}</p>{event.parameters && <code className="parameters">{event.parameters}</code>}
              </div>
            </li>
          ))}
        </ol>}
      </section>
    </>
  )
}
