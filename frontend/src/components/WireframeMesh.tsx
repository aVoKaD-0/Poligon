import { useId } from 'react'

const divisions = 32

function project(u: number, v: number): string {
  const peak = (x: number, y: number, spread: number) =>
    Math.exp(-((u - x) ** 2 + (v - y) ** 2) / spread)
  const z = 155 * peak(.26, .27, .025) + 92 * peak(.78, .48, .017)
    + 62 * peak(.57, .83, .026) - 24 * peak(.48, .53, .043)
  return (70 + 760 * u - 105 * v).toFixed(1) + ',' + (115 + 135 * v + 40 * u - z).toFixed(1)
}

const contours = Array.from({ length: divisions + 1 }, (_, index) => {
  const fixed = index / divisions
  const samples = Array.from({ length: 81 }, (_, point) => point / 80)
  return [
    samples.map(sample => project(fixed, sample)).join(' '),
    samples.map(sample => project(sample, fixed)).join(' '),
  ]
}).flat()

export function WireframeMesh() {
  const id = useId()
  return (
    <svg className="wireframe" viewBox="0 0 805 300" preserveAspectRatio="xMidYMid slice" aria-hidden="true">
      <defs>
        <pattern id={id + '-dots'} width="19" height="19" patternUnits="userSpaceOnUse">
          <circle cx="2" cy="2" r=".85" fill="currentColor" opacity=".17" />
        </pattern>
        <linearGradient id={id + '-fade'}>
          <stop stopColor="white" stopOpacity="0" />
          <stop offset=".27" stopColor="white" />
          <stop offset="1" stopColor="white" stopOpacity=".72" />
        </linearGradient>
        <mask id={id + '-mask'}><rect width="100%" height="100%" fill={'url(#' + id + '-fade)'} /></mask>
      </defs>
      <g mask={'url(#' + id + '-mask)'}>
        <rect width="100%" height="100%" fill={'url(#' + id + '-dots)'} />
        <g fill="none" stroke="currentColor" strokeWidth="1" opacity=".42">
          {contours.map(points => <polyline key={points} points={points} />)}
        </g>
        {[[.26, .27], [.78, .48], [.57, .83]].map(([u, v]) => (
          <rect key={u} x="-3" y="-3" width="6" height="6" rx="1" className="mesh-marker"
            transform={'translate(' + project(u, v) + ')'} />
        ))}
      </g>
    </svg>
  )
}
