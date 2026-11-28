export type RunStatus = 'queued' | 'running' | 'passed' | 'defect' | 'error'
export type Section = 'overview' | 'scenarios' | 'history'

export interface Scenario {
  id: string
  name: string
  description: string
  expected: string
}

export interface Catalog {
  scenarios: Scenario[]
  agentVersions: string[]
}

export interface RunConfig {
  scenarioId: string
  agent: string
}

export interface Run extends RunConfig {
  id: string
  startedAt: string
  scenarioName: string
  duration: number | null
  status: RunStatus
}

export interface RunEvent {
  time: string
  source: 'Клиент' | 'Агент' | 'API' | 'Стенд'
  message: string
  operation?: string
  parameters?: string
  warning?: boolean
}

export interface RunReport {
  run: Run
  expected: string
  actual: string | null
  events: RunEvent[]
}
