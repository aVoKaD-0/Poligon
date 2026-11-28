import type { Run } from '../types/poligon'
import { RunTable } from './RunTable'
import { WireframeMesh } from './WireframeMesh'

interface OverviewProps {
  runs: Run[]
  canStart: boolean
  onStart: () => void
}

export function OverviewPage({ runs, canStart, onStart }: OverviewProps) {
  const passed = runs.filter(run => run.status === 'passed').length
  const defects = runs.filter(run => run.status === 'defect').length
  const measured = runs.flatMap(run => run.duration === null ? [] : [run.duration])
  const duration = measured.length ? Math.round(measured.reduce((total, seconds) => total + seconds, 0) / measured.length) : '—'
  const recent = runs.slice(0, 16).reverse()
  const metrics = [
    { label: 'Всего прогонов', value: runs.length, unit: '', color: 'blue', bars: recent.map((_, i) => 45 + i * 3.5) },
    { label: 'Успешность', value: passed + defects ? Math.round(passed * 100 / (passed + defects)) : '—', unit: passed + defects ? '%' : '', color: 'mint', bars: recent.map(run => run.status === 'passed' ? 90 : 6) },
    { label: 'С дефектами', value: defects, unit: '', color: 'amber', bars: recent.map(run => run.status === 'defect' ? 90 : 6) },
    { label: 'Среднее время', value: duration, unit: measured.length ? ' с' : '', color: 'violet', bars: recent.map(run => Math.min((run.duration ?? 0) * 3, 100)) },
  ]
  return (
    <>
      <section className="hero" aria-labelledby="hero-title">
        <WireframeMesh />
        <div className="hero-content">
          <p className="eyebrow">Испытательный стенд</p>
          <h1 id="hero-title">Обзор стенда</h1>
          <p className="hero-description">Испытания ИИ-агента в среде выездного обслуживания.</p>
          <button className="primary" disabled={!canStart} onClick={onStart}>
            <svg width="17" height="17" viewBox="0 0 18 18" fill="none" aria-hidden="true"><path d="M5 2.5L15 9 5 15.5V2.5Z" stroke="currentColor" strokeWidth="1.5" /></svg>
            Новый прогон
          </button>
        </div>
      </section>
      <section className="block" aria-labelledby="metrics-title">
        <div className="section-heading"><h2 id="metrics-title">Показатели стенда</h2><span>За всё время</span></div>
        <div className="metric-grid">
          {metrics.map(metric => (
            <article className="metric panel" key={metric.label}>
              <p>{metric.label}</p>
              <div className="metric-bottom">
                <strong className="mono">{metric.value}{metric.unit}</strong>
                <div className={'spark ' + metric.color} aria-hidden="true">
                  {metric.bars.map((height, index) => <i key={index} style={{ height: height + '%', opacity: .42 + index / 28 }} />)}
                </div>
              </div>
            </article>
          ))}
        </div>
      </section>
      <section className="block" aria-labelledby="latest-title">
        <div className="section-heading"><h2 id="latest-title">Последние прогоны</h2><a href="#history">Вся история</a></div>
        <RunTable runs={runs.slice(0, 5)} />
      </section>
    </>
  )
}
