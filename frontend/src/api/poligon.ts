import type { Catalog, Run, RunConfig, RunReport } from '../types/poligon'

const baseUrl = import.meta.env.VITE_API_BASE_URL?.trim().replace(/\/+$/, '')
export const apiConfigured = Boolean(baseUrl)

class ApiError extends Error {
  readonly status: number

  constructor(status: number) {
    super('API вернул ошибку ' + status)
    this.status = status
  }
}

async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const response = await fetch(baseUrl + path, {
    credentials: 'same-origin',
    ...options,
  })
  if (!response.ok) throw new ApiError(response.status)
  return response.json() as Promise<T>
}

export const poligonApi = {
  catalog: (signal: AbortSignal) => request<Catalog>('/catalog', { signal }),
  runs: (signal: AbortSignal) => request<Run[]>('/runs', { signal }),
  async report(id: string, signal: AbortSignal): Promise<RunReport | null> {
    try {
      return await request<RunReport>('/runs/' + encodeURIComponent(id), { signal })
    } catch (error) {
      if (error instanceof ApiError && error.status === 404) return null
      throw error
    }
  },
  start: (config: RunConfig) => request<{ id: string }>('/runs', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(config),
  }),
}
