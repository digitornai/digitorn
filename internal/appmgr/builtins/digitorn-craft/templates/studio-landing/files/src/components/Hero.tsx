import { motion } from 'framer-motion'
import { ChevronDown } from 'lucide-react'

export default function Hero() {
  return (
    <section
      id="accueil"
      className="relative min-h-screen flex items-center justify-center overflow-hidden"
    >
      {/* Background gradient */}
      <div className="absolute inset-0 bg-gradient-to-br from-primary via-accent to-primary" />

      {/* Animated shapes */}
      <motion.div
        className="absolute top-20 left-10 w-72 h-72 bg-secondary/20 rounded-full blur-3xl"
        animate={{ x: [0, 30, 0], y: [0, -20, 0] }}
        transition={{ duration: 8, repeat: Infinity, ease: 'easeInOut' }}
      />
      <motion.div
        className="absolute bottom-20 right-10 w-96 h-96 bg-secondary/15 rounded-full blur-3xl"
        animate={{ x: [0, -40, 0], y: [0, 30, 0] }}
        transition={{ duration: 10, repeat: Infinity, ease: 'easeInOut' }}
      />

      <div className="relative z-10 text-center px-4 max-w-4xl mx-auto">
        <motion.p
          initial={{ opacity: 0, y: 20 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.6 }}
          className="text-secondary font-semibold tracking-widest uppercase mb-4 text-sm"
        >
          Studio de danse prive
        </motion.p>

        <motion.h1
          initial={{ opacity: 0, y: 30 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.8, delay: 0.2 }}
          className="text-5xl sm:text-6xl lg:text-8xl font-bold text-white leading-tight mb-6 text-balance"
        >
          L'art du mouvement,{' '}
          <span className="text-secondary">revele par la danse</span>
        </motion.h1>

        <motion.p
          initial={{ opacity: 0, y: 20 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.6, delay: 0.4 }}
          className="text-lg sm:text-xl text-white/70 max-w-2xl mx-auto mb-10 text-balance"
        >
          Decouvrez la magie de la danse dans notre studio intimiste. Cours
          particuliers et collectifs pour tous niveaux, tous ages.
        </motion.p>

        <motion.div
          initial={{ opacity: 0, y: 20 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.6, delay: 0.6 }}
          className="flex flex-col sm:flex-row items-center justify-center gap-4"
        >
          <a
            href="#cours"
            className="bg-secondary text-white px-8 py-4 rounded-full font-semibold text-lg hover:bg-secondary/90 transition-all hover:scale-105 shadow-lg shadow-secondary/25"
          >
            Decouvrir nos cours
          </a>
          <a
            href="#apropos"
            className="border-2 border-white/30 text-white px-8 py-4 rounded-full font-semibold text-lg hover:bg-white/10 transition-all"
          >
            En savoir plus
          </a>
        </motion.div>
      </div>

      {/* Scroll indicator */}
      <motion.div
        className="absolute bottom-8 left-1/2 -translate-x-1/2"
        animate={{ y: [0, 10, 0] }}
        transition={{ duration: 2, repeat: Infinity }}
      >
        <ChevronDown className="text-white/50" size={32} />
      </motion.div>
    </section>
  )
}
