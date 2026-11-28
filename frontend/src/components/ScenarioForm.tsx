import { useEffect, useRef, useState } from 'react'
import type { Catalog, RunConfig } from '../types/poligon'

interface ScenarioFormProps {
  initial: RunConfig
  catalog: Catalog
  pending: boolean
  error: string
  onClose: () => void
  onSubmit: (config: RunConfig) => void
}

export function ScenarioForm({ initial, catalog: { scenarios, agentVersions }, pending, error, onClose, onSubmit }: ScenarioFormProps) {
  const dialog = useRef<HTMLDialogElement>(null)
  const [scenarioId, setScenarioId] = useState(initial.scenarioId)
  const [agent, setAgent] = useState(initial.agent)

  useEffect(() => {
    const element = dialog.current
    element?.showModal()
    return () => element?.close()
  }, [])

  return (
    <dialog ref={dialog} aria-labelledby="form-title" onCancel={event => { event.preventDefault(); if (!pending) onClose() }}>
      <form onSubmit={event => { event.preventDefault(); onSubmit({ scenarioId, agent }) }}>
        <h2 id="form-title">Новый прогон</h2>
        {error && <p role="alert">{error}</p>}
        <label><span id="form-scenario-label">Сценарий</span>
          <select disabled={pending} aria-labelledby="form-scenario-label" value={scenarioId} onChange={event => setScenarioId(event.target.value)}>
            {scenarios.map(scenario => <option key={scenario.id} value={scenario.id}>{scenario.name}</option>)}
          </select>
        </label>
        <label><span id="form-agent-label">Версия агента</span>
          <select disabled={pending} aria-labelledby="form-agent-label" className="mono" value={agent} onChange={event => {
            const version = agentVersions.find(item => item === event.target.value)
            if (version) setAgent(version)
          }}>
            {agentVersions.map(version => <option key={version}>{version}</option>)}
          </select>
        </label>
        <div className="form-actions">
          <button className="secondary" disabled={pending} type="button" onClick={onClose}>Отмена</button>
          <button className="primary" disabled={pending} type="submit">{pending ? 'Запускаем…' : 'Запустить'}</button>
        </div>
      </form>
    </dialog>
  )
}
