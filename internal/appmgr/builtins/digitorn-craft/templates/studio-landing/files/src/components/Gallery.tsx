import { useState } from 'react'
import { motion, AnimatePresence } from 'framer-motion'
import { X, Expand } from 'lucide-react'
import clsx from 'clsx'

interface GalleryItem {
  id: number
  title: string
  category: string
  color: string
}

const items: GalleryItem[] = [
  { id: 1, title: 'Ballet en scene', category: 'Spectacles', color: 'from-secondary/80 to-primary/80' },
  { id: 2, title: 'Cours collectif', category: 'Cours', color: 'from-accent/80 to-primary/80' },
  { id: 3, title: 'Salsa party', category: 'Evenements', color: 'from-secondary/80 to-accent/80' },
  { id: 4, title: 'Studio interieur', category: 'Espace', color: 'from-primary/80 to-accent/80' },
  { id: 5, title: 'Danse contemporaine', category: 'Spectacles', color: 'from-accent/80 to-secondary/80' },
  { id: 6, title: 'Workshop tango', category: 'Evenements', color: 'from-primary/80 to-secondary/80' },
]

const categories = ['Tous', 'Spectacles', 'Cours', 'Evenements', 'Espace']

export default function Gallery() {
  const [filter, setFilter] = useState('Tous')
  const [selected, setSelected] = useState<GalleryItem | null>(null)

  const filtered = items.filter(
    (item) => filter === 'Tous' || item.category === filter
  )

  return (
    <section id="galerie" className="py-20 sm:py-28 bg-light">
      <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
        <motion.div
          initial={{ opacity: 0, y: 20 }}
          whileInView={{ opacity: 1, y: 0 }}
          viewport={{ once: true }}
          className="text-center mb-16"
        >
          <p className="text-secondary font-semibold tracking-widest uppercase text-sm mb-4">
            Galerie
          </p>
          <h2 className="text-4xl sm:text-5xl font-bold text-primary mb-6">
            Nos moments
          </h2>
          <p className="text-gray-600 text-lg max-w-2xl mx-auto">
            Un apercu de l'atmosphere unique de notre studio, nos cours et
            nos spectacles.
          </p>
        </motion.div>

        {/* Filters */}
        <div className="flex flex-wrap justify-center gap-3 mb-12">
          {categories.map((cat) => (
            <button
              key={cat}
              onClick={() => setFilter(cat)}
              className={clsx(
                'px-5 py-2 rounded-full text-sm font-medium transition-all',
                filter === cat
                  ? 'bg-secondary text-white'
                  : 'bg-gray-100 text-gray-600 hover:bg-gray-200'
              )}
            >
              {cat}
            </button>
          ))}
        </div>

        {/* Grid */}
        <motion.div layout className="grid grid-cols-2 lg:grid-cols-3 gap-4">
          <AnimatePresence mode="popLayout">
            {filtered.map((item) => (
              <motion.div
                key={item.id}
                layout
                initial={{ opacity: 0, scale: 0.9 }}
                animate={{ opacity: 1, scale: 1 }}
                exit={{ opacity: 0, scale: 0.9 }}
                transition={{ duration: 0.4 }}
                className={clsx(
                  'relative aspect-square rounded-2xl overflow-hidden cursor-pointer group',
                  'bg-gradient-to-br',
                  item.color
                )}
                onClick={() => setSelected(item)}
              >
                <div className="absolute inset-0 bg-gradient-to-t from-black/60 to-transparent opacity-0 group-hover:opacity-100 transition-opacity" />
                <div className="absolute bottom-0 left-0 right-0 p-4 translate-y-full group-hover:translate-y-0 transition-transform">
                  <p className="text-white font-bold text-lg">{item.title}</p>
                  <p className="text-white/70 text-sm">{item.category}</p>
                </div>
                <div className="absolute top-4 right-4 opacity-0 group-hover:opacity-100 transition-opacity">
                  <Expand className="text-white" size={20} />
                </div>
              </motion.div>
            ))}
          </AnimatePresence>
        </motion.div>
      </div>

      {/* Lightbox */}
      <AnimatePresence>
        {selected && (
          <motion.div
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            className="fixed inset-0 z-50 bg-black/90 flex items-center justify-center p-4"
            onClick={() => setSelected(null)}
          >
            <motion.div
              initial={{ scale: 0.9 }}
              animate={{ scale: 1 }}
              exit={{ scale: 0.9 }}
              className={clsx(
                'relative w-full max-w-2xl aspect-video rounded-3xl bg-gradient-to-br overflow-hidden',
                selected.color
              )}
              onClick={(e) => e.stopPropagation()}
            >
              <div className="absolute inset-0 bg-gradient-to-t from-black/60 to-transparent" />
              <div className="absolute bottom-8 left-8">
                <p className="text-white text-3xl font-bold">{selected.title}</p>
                <p className="text-white/70">{selected.category}</p>
              </div>
              <button
                onClick={() => setSelected(null)}
                className="absolute top-4 right-4 bg-white/20 backdrop-blur-sm rounded-full p-2 text-white hover:bg-white/30 transition-colors"
                aria-label="Fermer"
              >
                <X size={20} />
              </button>
            </motion.div>
          </motion.div>
        )}
      </AnimatePresence>
    </section>
  )
}
