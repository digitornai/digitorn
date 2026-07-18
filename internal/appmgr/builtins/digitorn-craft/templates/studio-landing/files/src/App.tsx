import { useState } from 'react'
import { motion, AnimatePresence } from 'framer-motion'
import Navbar from './components/Navbar'
import Hero from './components/Hero'
import About from './components/About'
import Classes from './components/Classes'
import Gallery from './components/Gallery'
import Testimonials from './components/Testimonials'
import Contact from './components/Contact'
import Footer from './components/Footer'

function App() {
  const [menuOpen, setMenuOpen] = useState(false)

  return (
    <div className="min-h-screen bg-light">
      <Navbar menuOpen={menuOpen} setMenuOpen={setMenuOpen} />
      <AnimatePresence>
        {menuOpen && <MobileMenu setMenuOpen={setMenuOpen} />}
      </AnimatePresence>
      <Hero />
      <About />
      <Classes />
      <Gallery />
      <Testimonials />
      <Contact />
      <Footer />
    </div>
  )
}

function MobileMenu({ setMenuOpen }: { setMenuOpen: (v: boolean) => void }) {
  const links = [
    { label: 'Accueil', href: '#accueil' },
    { label: 'A propos', href: '#apropos' },
    { label: 'Cours', href: '#cours' },
    { label: 'Galerie', href: '#galerie' },
    { label: 'Avis', href: '#avis' },
    { label: 'Contact', href: '#contact' },
  ]

  return (
    <motion.div
      initial={{ opacity: 0, y: -20 }}
      animate={{ opacity: 1, y: 0 }}
      exit={{ opacity: 0, y: -20 }}
      className="fixed inset-0 z-40 bg-primary/95 backdrop-blur-md flex flex-col items-center justify-center gap-8"
    >
      {links.map((link) => (
        <a
          key={link.href}
          href={link.href}
          onClick={() => setMenuOpen(false)}
          className="text-2xl font-medium text-white hover:text-secondary transition-colors"
        >
          {link.label}
        </a>
      ))}
    </motion.div>
  )
}

export default App
