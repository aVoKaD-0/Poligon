
import { useEffect, useRef, useState } from 'react'
import { apiConfigured, poligonApi } from './api/poligon'
import { Header, navigation } from './components/Header'
import { HistoryPage } from './components/HistoryPage'
import { OverviewPage } from './components/OverviewPage'
import { ScenarioForm } from './components/ScenarioForm'
import { ScenarioReportPage } from './components/ScenarioReportPage'
import { ScenariosPage } from './components/ScenariosPage'
import type { Catalog, Run, RunConfig, RunReport } from './types/poligon'

const emptyCatalog: Catalog = { scenarios: [], agentVersions: [] }
const getRoute = () => window.location.hash.slice(1) || 'overview'
const errorMessage = (error: unknown) => error instanceof Error ? error.message : 'Неизвестная ошибка'

function getRunId(route: string) {
  if (!route.startsWith('run/')) return null
  try { return decodeURIComponent(route.slice(4)) } catch { return null }
}

export function App() {
  const [route, setRoute] = useState(getRoute)
  const [runs, setRuns] = useState<Run[]>([])
  const [catalog, setCatalog] = useState<Catalog>(emptyCatalog)
  const [config, setConfig] = useState<RunConfig | null>(null)
  const [pending, setPending] = useState(false)
  const submitting = useRef(false)
  const [submitError, setSubmitError] = useState('')
  const [listError, setListError] = useState('')
  const [loading, setLoading] = useState(apiConfigured)
  const [revision, setRevision] = useState(0)
  const [reportState, setReportState] = useState<{ id: string; report?: RunReport | null; error?: string }>()
  const reportRoute = route.startsWith('run/')
  const runId = getRunId(route)
  const report = runId === reportState?.id ? reportState?.report : undefined
  const reportError = runId === reportState?.id ? reportState?.error : undefined
  const section = reportRoute ? 'history' : navigation.find(item => item.section === route)?.section ?? 'overview'
  const canStart = catalog.scenarios.length > 0 && catalog.agentVersions.length > 0

  useEffect(() => {
    const update = () => {
      setRoute(getRoute())
      setConfig(null)
      window.scrollTo(0, 0)
      document.getElementById('main')?.focus({ preventScroll: true })
    }
    window.addEventListener('hashchange', update)
    return () => window.removeEventListener('hashchange', update)
  }, [])

  useEffect(() => {
    if (!apiConfigured) return
    const controller = new AbortController()
    const { signal } = controller
    setLoading(true)
    setListError('')
    Promise.all([poligonApi.catalog(signal), poligonApi.runs(signal)])
      .then(([nextCatalog, nextRuns]) => {
        if (signal.aborted) return
        setCatalog(nextCatalog)
        setRuns(nextRuns.sort((a, b) => Date.parse(b.startedAt) - Date.parse(a.startedAt)))
      })
      .catch(error => { if (!signal.aborted) setListError(errorMessage(error)) })
      .finally(() => { if (!signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [revision, section])

  useEffect(() => {
    if (!apiConfigured || !runId) return
    const controller = new AbortController()
    const { signal } = controller
    let timer: number | undefined
    async function loadReport(id: string) {
      try {
        const result = await poligonApi.report(id, signal)
        if (signal.aborted) return
        setReportState({ id, report: result })
        if (result?.run.status === 'queued' || result?.run.status === 'running') {
          timer = window.setTimeout(() => { void loadReport(id) }, 2500)
        }
      } catch (error) {
        if (!signal.aborted) setReportState({ id, error: errorMessage(error) })
      }
    }
    void loadReport(runId)
    return () => { controller.abort(); window.clearTimeout(timer) }
  }, [runId, revision])

  function openForm(scenarioId = catalog.scenarios[0]?.id, agent = catalog.agentVersions[0]) {
    if (!catalog.scenarios.some(item => item.id === scenarioId) || !catalog.agentVersions.includes(agent)) return
    setSubmitError('')
    setConfig({ scenarioId, agent })
  }

  async function startRun(selection: RunConfig) {
    if (submitting.current) return
    submitting.current = true
    setPending(true)
    setSubmitError('')
    try {
      const { id } = await poligonApi.start(selection)
      setConfig(null)
      setRevision(value => value + 1)
      window.location.hash = 'run/' + encodeURIComponent(id)
    } catch (error) {
      setSubmitError('Не удалось запустить прогон. ' + errorMessage(error))
    } finally {
      submitting.current = false
      setPending(false)
    }
  }

  const refresh = () => setRevision(value => value + 1)
  const canRepeat = report && catalog.scenarios.some(item => item.id === report.run.scenarioId)
    && catalog.agentVersions.includes(report.run.agent)

  return (
    <>
      <a className="skip-link" href="#main" onClick={event => {
        event.preventDefault()
        document.getElementById('main')?.focus()
      }}>Перейти к содержимому</a>
      <Header section={section} />
      <main id="main" className="container workspace" tabIndex={-1}>
        <nav className="breadcrumb" aria-label="Хлебные крошки">
          <span>Рабочая область</span><span aria-hidden="true">/</span>
          {reportRoute ? <><a href="#history">История прогонов</a><span aria-hidden="true">/</span><span className="mono" aria-current="page">{runId ? '#' + runId : 'Отчёт'}</span></>
            : <span aria-current="page">{navigation.find(item => item.section === section)?.label}</span>}
        </nav>
        {!apiConfigured && <p className="panel empty" role="status">API стенда пока не подключён. Запуск прогонов станет доступен после подключения.</p>}
        {loading && <p role="status">Загружаем данные стенда…</p>}
        {listError && <div className="panel empty"><p role="alert">Не удалось загрузить данные стенда. {listError}</p><button className="secondary" onClick={refresh}>Повторить загрузку</button></div>}
        {reportRoute
          ? report
            ? <ScenarioReportPage report={report} canRepeat={Boolean(canRepeat)} onRepeat={() => openForm(report.run.scenarioId, report.run.agent)} />
            : <div className="page-heading">
                <h1>Отчёт о прогоне</h1>
                {reportError ? <><p role="alert">Не удалось загрузить отчёт. {reportError}</p><button className="secondary" onClick={refresh}>Повторить загрузку</button></>
                  : apiConfigured && runId && report !== null ? <p role="status">Загружаем отчёт…</p>
                    : <>{apiConfigured && <p>Прогон не найден.</p>}<p><a href="#history">Открыть историю прогонов</a></p></>}
              </div>
          : section === 'history'
            ? <HistoryPage runs={runs} />
            : section === 'scenarios'
              ? <ScenariosPage scenarios={catalog.scenarios} canStart={canStart} onStart={openForm} />
              : <OverviewPage runs={runs} canStart={canStart} onStart={() => openForm()} />}
      </main>
      {config && <ScenarioForm initial={config} catalog={catalog} pending={pending} error={submitError} onClose={() => setConfig(null)} onSubmit={startRun} />}
    </>
  )
}
