import type { Section } from '../types/poligon'

export const navigation: { section: Section; label: string }[] = [
  { section: 'overview', label: 'Обзор' },
  { section: 'scenarios', label: 'Сценарии' },
  { section: 'history', label: 'История прогонов' },
]

interface HeaderProps {
  section: Section
}

export function Header({ section }: HeaderProps) {
  return (
    <header className="site-header">
      <div className="container header-inner">
        <a className="brand" href="#overview">ПОЛИГОН</a>
        <nav aria-label="Основная навигация">
          {navigation.map(({ section: id, label }) => (
            <a key={id} href={'#' + id} aria-current={id === section ? 'page' : undefined}>{label}</a>
          ))}
        </nav>
      </div>
    </header>
  )
}
