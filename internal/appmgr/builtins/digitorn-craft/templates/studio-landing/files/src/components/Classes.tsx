import { motion } from 'framer-motion'
import { Clock, Flame, Star, Users } from 'lucide-react'
import clsx from 'clsx'
import { useState } from 'react'

interface ClassItem {
  title: string
  level: string
  duration: string
  time: string
  price: string
  description: string
  icon: typeof Flame
  popular?: boolean
}

const classes: ClassItem[] = [
  {
    title: 'Ballet Classique',
    level: 'Debutant a avance',
    duration: '1h15',
    time: 'Lun / Mer / Ven — 18h00',
    price: '25 € / cours',
    description: 'Technique classique dans un cadre elegant. Posture, grace et discipline au programme.',
    icon: Star,
    popular: true,
  },
  {
    title: 'Salsa & Bachata',
    level: 'Tous niveaux',
    duration: '1h30',
    time: 'Mar / Jeu — 19h30',
    price: '22 € / cours',
    description: 'Plongez dans les rythmes latins. Cours dynamique et convivial, solo ou en couple.',
    icon: Flame,
    popular: true,
  },
  {
    title: 'Danse Contemporaine',
    level: 'Intermediaire',
    duration: '1h30',
    time: 'Mer / Sam — 17h00',
    price: '25 € / cours',
    description: 'Expression corporelle libre et creative. Explorez le mouvement moderne avec assurance.',
    icon: Users,
  },
  {
    title: 'Hip Hop & Street',
    level: 'Debutant a avance',
    duration: '1h',
    time: 'Lun / Ven — 20h00',
    price: '20 € / cours',
    description: 'Energie brute et style urbain. Choreographies modernes et esprit d\'equipe.',
    icon: Flame,
  },
  {
    title: 'Tango Argentin',
    level: 'Tous niveaux',
    duration: '1h30',
    time: 'Sam — 19h00',
    price: '28 € / cours',
    description: 'Intimite et connection. Apprenez les bases du tango dans un cadre raffine.',
    icon: Star,
  },
  {
    title: 'Cours Particulier',
    level: 'Sur mesure',
    duration: '1h',
    time: 'Sur rendez-vous',
    price: '60 € / cours',
    description: 'Un accompagnement 100% personnalise. Ideal pour un preparation ou un projet special.',
    icon: Clock,
  },
]

const filters = ['Tous', 'Debutant', 'Intermediaire', 'Populaire']

export default function Classes() {
  const [activeFilter, setActiveFilter] = useState('Tous')

  const filtered = classes.filter((c) => {
    if (activeFilter === 'Tous') return true
    if (activeFilter === 'Populaire') return c.popular
    if (activeFilter === 'Debutant') return c.level.toLowerCase().includes('debutant') || c.level.toLowerCase().includes('tous')
    if (activeFilter === 'Intermediaire') return c.level.toLowerCase().includes('intermediaire') || c.level.toLowerCase().includes('avance')
    return true
  })

  return (
    <section id="cours" className="py-20 sm:py-28 bg-primary">
      <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
        <motion.div
          initial={{ opacity: 0, y: 20 }}
          whileInView={{ opacity: 1, y: 0 }}
          viewport={{ once: true }}
          className="text-center mb-16"
        >
          <p className="text-secondary font-semibold tracking-widest uppercase text-sm mb-4">
            Nos cours
          </p>
          <h2 className="text-4xl sm:text-5xl font-bold text-white mb-6">
            Trouvez votre rhythm
          </h2>
          <p className="text-white/60 text-lg max-w-2xl mx-auto">
            Du ballet classique au street dance, explorez notre programme
            et trouvez le cours qui vous correspond.
          </p>
        </motion.div>

        {/* Filters */}
        <div className="flex flex-wrap justify-center gap-3 mb-12">
          {filters.map((f) => (
            <button
              key={f}
              onClick={() => setActiveFilter(f)}
              className={clsx(
                'px-5 py-2 rounded-full text-sm font-medium transition-all',
                activeFilter === f
                  ? 'bg-secondary text-white'
                  : 'bg-white/10 text-white/70 hover:bg-white/20'
              )}
            >
              {f}
            </button>
          ))}
        </div>

        {/* Cards */}
        <div className="grid sm:grid-cols-2 lg:grid-cols-3 gap-6">
          {filtered.map((c, i) => (
            <motion.div
              key={c.title}
              initial={{ opacity: 0, y: 30 }}
              whileInView={{ opacity: 1, y: 0 }}
              viewport={{ once: true }}
              transition={{ duration: 0.5, delay: i * 0.1 }}
              className={clsx(
                'group relative bg-white/5 backdrop-blur-sm rounded-2xl p-6 border border-white/10 hover:border-secondary/50 transition-all hover:bg-white/10',
                c.popular && 'ring-2 ring-secondary/30'
              )}
            >
              {c.popular && (
                <span className="absolute -top-3 right-6 bg-secondary text-white text-xs font-bold px-3 py-1 rounded-full">
                  Populaire
                </span>
              )}
              <c.icon className="w-8 h-8 text-secondary mb-4" />
              <h3 className="text-xl font-bold text-white mb-2">{c.title}</h3>
              <p className="text-white/50 text-sm mb-4">{c.description}</p>
              <div className="space-y-2 text-sm text-white/60">
                <p>{c.level}</p>
                <p className="flex items-center gap-2">
                  <Clock className="w-4 h-4" /> {c.duration} — {c.time}
                </p>
              </div>
              <div className="mt-6 flex items-center justify-between">
                <span className="text-secondary font-bold text-lg">{c.price}</span>
                <a
                  href="#contact"
                  className="text-sm text-white/70 hover:text-secondary transition-colors font-medium"
                >
                  Reservez →
                </a>
              </div>
            </motion.div>
          ))}
        </div>
      </div>
    </section>
  )
}
