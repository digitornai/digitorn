import { motion } from 'framer-motion'
import { Heart, Music, Users, Award } from 'lucide-react'

const stats = [
  { icon: Users, value: '500+', label: 'Eleves inscrits' },
  { icon: Award, value: '15', label: 'Annees d\'experience' },
  { icon: Music, value: '20+', label: 'Styles enseignes' },
  { icon: Heart, value: '100%', label: 'Passion' },
]

export default function About() {
  return (
    <section id="apropos" className="py-20 sm:py-28 bg-light">
      <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
        <div className="grid lg:grid-cols-2 gap-16 items-center">
          {/* Image side */}
          <motion.div
            initial={{ opacity: 0, x: -40 }}
            whileInView={{ opacity: 1, x: 0 }}
            viewport={{ once: true }}
            transition={{ duration: 0.8 }}
            className="relative"
          >
            <div className="aspect-[4/5] rounded-3xl bg-gradient-to-br from-accent to-primary overflow-hidden relative">
              <div className="absolute inset-0 bg-gradient-to-t from-primary/60 to-transparent" />
              <div className="absolute bottom-8 left-8 right-8">
                <p className="text-white/60 text-sm tracking-widest uppercase mb-2">
                  Notre philosophie
                </p>
                <p className="text-white text-2xl font-bold">
                  "La danse est le langage du corps et de l'ame"
                </p>
              </div>
            </div>
            <div className="absolute -bottom-6 -right-6 w-32 h-32 bg-secondary rounded-2xl -z-10" />
          </motion.div>

          {/* Text side */}
          <motion.div
            initial={{ opacity: 0, x: 40 }}
            whileInView={{ opacity: 1, x: 0 }}
            viewport={{ once: true }}
            transition={{ duration: 0.8, delay: 0.2 }}
          >
            <p className="text-secondary font-semibold tracking-widest uppercase text-sm mb-4">
              A propos
            </p>
            <h2 className="text-4xl sm:text-5xl font-bold text-primary mb-6 leading-tight">
              Un espace dedie a l'expression artistique
            </h2>
            <p className="text-gray-600 text-lg leading-relaxed mb-8">
              Fondé en 2010, notre studio offre un environnement chaleureux et
              professionnel pour explorer tous les styles de danse. Nos
              professeurs diplomes guident chaque eleve avec passion, du debutant
              absolu au danseur confirme.
            </p>
            <p className="text-gray-600 text-lg leading-relaxed mb-10">
              Que vous souhaitiez decouvrir le ballet, vous initier au tango,
              ou perfectionner votre salsa, notre studio vous accueille avec les
              bras ouverts.
            </p>

            {/* Stats */}
            <div className="grid grid-cols-2 sm:grid-cols-4 gap-6">
              {stats.map((stat) => (
                <motion.div
                  key={stat.label}
                  initial={{ opacity: 0, y: 20 }}
                  whileInView={{ opacity: 1, y: 0 }}
                  viewport={{ once: true }}
                  transition={{ duration: 0.5 }}
                  className="text-center"
                >
                  <stat.icon className="w-6 h-6 text-secondary mx-auto mb-2" />
                  <p className="text-3xl font-bold text-primary">{stat.value}</p>
                  <p className="text-sm text-gray-500">{stat.label}</p>
                </motion.div>
              ))}
            </div>
          </motion.div>
        </div>
      </div>
    </section>
  )
}
