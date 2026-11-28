import type { RunStatus } from '../types/poligon'

export const statusLabels: Record<RunStatus, string> = {
  queued: 'В очереди',
  running: 'Выполняется',
  passed: 'Успешно',
  defect: 'Дефект',
  error: 'Ошибка стенда',
}
