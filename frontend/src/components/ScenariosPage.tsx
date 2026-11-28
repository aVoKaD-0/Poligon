import type { Scenario } from '../types/poligon'

interface ScenariosProps {
  scenarios: Scenario[]
  canStart: boolean
  onStart: (scenarioId: string) => void
}

export function ScenariosPage({ scenarios, canStart, onStart }: ScenariosProps) {
  return (
    <>
      <div className="page-heading"><h1>Сценарии</h1><p>Выберите, какое поведение агента проверить в тестовой CRM.</p></div>
      {!scenarios.length && <p className="empty panel">Сценариев пока нет.</p>}
      <div className="scenario-grid">
        {scenarios.map(scenario => (
          <article className="panel scenario-card" key={scenario.id}>
            <h2>{scenario.name}</h2><p>{scenario.description}</p>
            <button className="secondary" disabled={!canStart} onClick={() => onStart(scenario.id)}>Настроить прогон</button>
          </article>
        ))}
      </div>
    </>
  )
}
