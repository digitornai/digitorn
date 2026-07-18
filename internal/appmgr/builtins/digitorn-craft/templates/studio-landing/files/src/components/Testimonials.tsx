import { motion } from 'framer-motion'
import { Star } from 'lucide-react'

interface Testimonial {
  name: string
  role: string
  text: string
  rating: number
}

const testimonials: Testimonial[] = [
  {
    name: 'Marie D.',
    role: 'Eleve salsa — 2 ans',
    text: "Un studio incroyable, pleine de vie. Les professeurs sont passionnes et l'ambiance est toujours geniale. Je recommande a 100% !",
    rating: 5,
  },
  {
    name: 'Thomas L.',
    role: 'Eleve ballet — 6 mois',
    text: "Debutant total, j'avais peur d'etre perdu. Mais l'equipe m'a mis a l'aise des le premier cours. Aujourd'hui je ne peux plus m'en passer.",
    rating: 5,
  },
  {
    name: 'Sophie M.',
    role: 'Eleve contemporain — 1 an',
    text: "La danse m'a changee. Ici on ne juge pas, on explore. Le studio est magnifique et les cours sont toujours stimulants.",
    rating: 5,
  },
]

export default function Testimonials() {
  return (
    <section id="avis" className="py-20 sm:py-28 bg-warm">
      <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
        <motion.div
          initial={{ opacity: 0, y: 20 }}
          whileInView={{ opacity: 1, y: 0 }}
          viewport={{ once: true }}
          className="text-center mb-16"
        >
          <p className="text-secondary font-semibold tracking-widest uppercase text-sm mb-4">
            Avis
          </p>
          <h2 className="text-4xl sm:text-5xl font-bold text-primary mb-6">
            Ils en parlent
          </h2>
          <p className="text-gray-600 text-lg max-w-2xl mx-auto">
            Decouvrez les temoignages de nos eleves qui ont trouvé leur
            passion chez nous.
          </p>
        </motion.div>

        <div className="grid md:grid-cols-3 gap-8">
          {testimonials.map((t, i) => (
            <motion.div
              key={t.name}
              initial={{ opacity: 0, y: 30 }}
              whileInView={{ opacity: 1, y: 0 }}
              viewport={{ once: true }}
              transition={{ duration: 0.5, delay: i * 0.15 }}
              className="bg-white rounded-2xl p-8 shadow-sm hover:shadow-md transition-shadow"
            >
              <div className="flex gap-1 mb-4">
                {Array.from({ length: t.rating }).map((_, j) => (
                  <Star key={j} className="w-5 h-5 fill-secondary text-secondary" />
                ))}
              </div>
              <p className="text-gray-700 leading-relaxed mb-6 italic">
                "{t.text}"
              </p>
              <div className="flex items-center gap-3">
                <div className="w-10 h-10 rounded-full bg-gradient-to-br from-secondary to-primary flex items-center justify-center text-white font-bold text-sm">
                  {t.name.charAt(0)}
                </div>
                <div>
                  <p className="font-bold text-primary">{t.name}</p>
                  <p className="text-sm text-gray-500">{t.role}</p>
                </div>
              </div>
            </motion.div>
          ))}
        </div>
      </div>
    </section>
  )
}
