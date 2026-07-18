import { useState, useEffect } from 'react'
import { Menu, X } from 'lucide-react'
import clsx from 'clsx'

interface NavbarProps {
  menuOpen: boolean
  setMenuOpen: (v: boolean) => void
}

export default function Navbar({ menuOpen, setMenuOpen }: NavbarProps) {
  const [scrolled, setScrolled] = useState(false)

  useEffect(() => {
    const onScroll = () => setScrolled(window.scrollY > 50)
    window.addEventListener('scroll', onScroll)
    return () => window.removeEventListener('scroll', onScroll)
  }, [])

  const links = [
    { label: 'Accueil', href: '#accueil' },
    { label: 'A propos', href: '#apropos' },
    { label: 'Cours', href: '#cours' },
    { label: 'Galerie', href: '#galerie' },
    { label: 'Avis', href: '#avis' },
    { label: 'Contact', href: '#contact' },
  ]

  return (
    <nav
      className={clsx(
        'fixed top-0 left-0 right-0 z-50 transition-all duration-300',
        scrolled ? 'bg-white/95 backdrop-blur-md shadow-md py-3' : 'bg-transparent py-5'
      )}
    >
      <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 flex items-center justify-between">
        <a href="#accueil" className="text-2xl font-bold tracking-tight">
          <span className={scrolled ? 'text-primary' : 'text-white'}>Studio</span>
          <span className={clsx(scrolled ? 'text-secondary' : 'text-secondary')}>Danse</span>
        </a>

        {/* Desktop links */}
        <div className="hidden md:flex items-center gap-8">
          {links.map((link) => (
            <a
              key={link.href}
              href={link.href}
              className={clsx(
                'text-sm font-medium transition-colors hover:text-secondary',
                scrolled ? 'text-primary' : 'text-white'
              )}
            >
              {link.label}
            </a>
          ))}
          <a
            href="#contact"
            className="bg-secondary text-white px-5 py-2 rounded-full text-sm font-semibold hover:bg-secondary/90 transition-colors"
          >
            S'inscrire
          </a>
        </div>

        {/* Mobile toggle */}
        <button
          className={clsx('md:hidden', scrolled ? 'text-primary' : 'text-white')}
          onClick={() => setMenuOpen(!menuOpen)}
          aria-label="Menu"
        >
          {menuOpen ? <X size={28} /> : <Menu size={28} />}
        </button>
      </div>
    </nav>
  )
}
