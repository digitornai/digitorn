import { useState } from 'react'
import { motion } from 'framer-motion'
import { Mail, Phone, MapPin, Send } from 'lucide-react'

export default function Contact() {
  const [form, setForm] = useState({ name: '', email: '', message: '', course: '' })
  const [submitted, setSubmitted] = useState(false)

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    // Wire to real backend later
    console.log('Form submitted:', form)
    setSubmitted(true)
    setForm({ name: '', email: '', message: '', course: '' })
  }

  return (
    <section id="contact" className="py-20 sm:py-28 bg-primary">
      <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
        <motion.div
          initial={{ opacity: 0, y: 20 }}
          whileInView={{ opacity: 1, y: 0 }}
          viewport={{ once: true }}
          className="text-center mb-16"
        >
          <p className="text-secondary font-semibold tracking-widest uppercase text-sm mb-4">
            Contact
          </p>
          <h2 className="text-4xl sm:text-5xl font-bold text-white mb-6">
            Rejoignez-nous
          </h2>
          <p className="text-white/60 text-lg max-w-2xl mx-auto">
            Envie de commencer votre aventure dans la danse ? Contactez-nous
            pour un cours d'essai gratuit.
          </p>
        </motion.div>

        <div className="grid lg:grid-cols-2 gap-12">
          {/* Contact form */}
          <motion.div
            initial={{ opacity: 0, x: -30 }}
            whileInView={{ opacity: 1, x: 0 }}
            viewport={{ once: true }}
            transition={{ duration: 0.6 }}
          >
            {submitted ? (
              <div className="bg-white/10 backdrop-blur-sm rounded-2xl p-12 text-center border border-white/10">
                <div className="w-16 h-16 bg-secondary rounded-full flex items-center justify-center mx-auto mb-6">
                  <Send className="text-white" size={24} />
                </div>
                <h3 className="text-2xl font-bold text-white mb-3">Merci !</h3>
                <p className="text-white/60">
                  Votre message a ete envoye. Nous vous recontacterons dans les
                  plus brefs delais.
                </p>
                <button
                  onClick={() => setSubmitted(false)}
                  className="mt-6 text-secondary hover:text-secondary/80 transition-colors font-medium"
                >
                  Envoyer un autre message
                </button>
              </div>
            ) : (
              <form
                onSubmit={handleSubmit}
                className="bg-white/5 backdrop-blur-sm rounded-2xl p-8 border border-white/10 space-y-5"
              >
                <div className="grid sm:grid-cols-2 gap-5">
                  <div>
                    <label className="text-white/70 text-sm mb-1.5 block">Nom</label>
                    <input
                      type="text"
                      required
                      value={form.name}
                      onChange={(e) => setForm({ ...form, name: e.target.value })}
                      className="w-full bg-white/10 border border-white/10 rounded-xl px-4 py-3 text-white placeholder:text-white/30 focus:outline-none focus:ring-2 focus:ring-secondary/50 transition-all"
                      placeholder="Votre nom"
                    />
                  </div>
                  <div>
                    <label className="text-white/70 text-sm mb-1.5 block">Email</label>
                    <input
                      type="email"
                      required
                      value={form.email}
                      onChange={(e) => setForm({ ...form, email: e.target.value })}
                      className="w-full bg-white/10 border border-white/10 rounded-xl px-4 py-3 text-white placeholder:text-white/30 focus:outline-none focus:ring-2 focus:ring-secondary/50 transition-all"
                      placeholder="votre@email.com"
                    />
                  </div>
                </div>
                <div>
                  <label className="text-white/70 text-sm mb-1.5 block">Cours interest</label>
                  <select
                    value={form.course}
                    onChange={(e) => setForm({ ...form, course: e.target.value })}
                    className="w-full bg-white/10 border border-white/10 rounded-xl px-4 py-3 text-white focus:outline-none focus:ring-2 focus:ring-secondary/50 transition-all"
                  >
                    <option value="" className="bg-primary">Choisir un cours</option>
                    <option value="ballet" className="bg-primary">Ballet Classique</option>
                    <option value="salsa" className="bg-primary">Salsa & Bachata</option>
                    <option value="contemporain" className="bg-primary">Danse Contemporaine</option>
                    <option value="hiphop" className="bg-primary">Hip Hop & Street</option>
                    <option value="tango" className="bg-primary">Tango Argentin</option>
                    <option value="prive" className="bg-primary">Cours Particulier</option>
                  </select>
                </div>
                <div>
                  <label className="text-white/70 text-sm mb-1.5 block">Message</label>
                  <textarea
                    rows={4}
                    required
                    value={form.message}
                    onChange={(e) => setForm({ ...form, message: e.target.value })}
                    className="w-full bg-white/10 border border-white/10 rounded-xl px-4 py-3 text-white placeholder:text-white/30 focus:outline-none focus:ring-2 focus:ring-secondary/50 transition-all resize-none"
                    placeholder="Dites-nous en plus sur vous et votre souhait..."
                  />
                </div>
                <button
                  type="submit"
                  className="w-full bg-secondary text-white py-4 rounded-xl font-semibold text-lg hover:bg-secondary/90 transition-all hover:scale-[1.02] shadow-lg shadow-secondary/25"
                >
                  Envoyer le message
                </button>
              </form>
            )}
          </motion.div>

          {/* Contact info */}
          <motion.div
            initial={{ opacity: 0, x: 30 }}
            whileInView={{ opacity: 1, x: 0 }}
            viewport={{ once: true }}
            transition={{ duration: 0.6, delay: 0.2 }}
            className="space-y-8"
          >
            <div className="bg-white/5 backdrop-blur-sm rounded-2xl p-8 border border-white/10">
              <h3 className="text-xl font-bold text-white mb-6">Nos coordonnees</h3>
              <div className="space-y-5">
                <div className="flex items-start gap-4">
                  <div className="w-10 h-10 bg-secondary/20 rounded-xl flex items-center justify-center shrink-0">
                    <MapPin className="text-secondary w-5 h-5" />
                  </div>
                  <div>
                    <p className="text-white font-medium">Adresse</p>
                    <p className="text-white/50 text-sm">
                      12, rue de la Danse<br />75011 Paris, France
                    </p>
                  </div>
                </div>
                <div className="flex items-start gap-4">
                  <div className="w-10 h-10 bg-secondary/20 rounded-xl flex items-center justify-center shrink-0">
                    <Phone className="text-secondary w-5 h-5" />
                  </div>
                  <div>
                    <p className="text-white font-medium">Telephone</p>
                    <p className="text-white/50 text-sm">+33 1 42 00 00 00</p>
                  </div>
                </div>
                <div className="flex items-start gap-4">
                  <div className="w-10 h-10 bg-secondary/20 rounded-xl flex items-center justify-center shrink-0">
                    <Mail className="text-secondary w-5 h-5" />
                  </div>
                  <div>
                    <p className="text-white font-medium">Email</p>
                    <p className="text-white/50 text-sm">contact@studio-danse.fr</p>
                  </div>
                </div>
              </div>
            </div>

            <div className="bg-white/5 backdrop-blur-sm rounded-2xl p-8 border border-white/10">
              <h3 className="text-xl font-bold text-white mb-4">Horaires</h3>
              <div className="space-y-2 text-white/60 text-sm">
                <div className="flex justify-between">
                  <span>Lundi — Vendredi</span>
                  <span className="text-white/80">9h00 — 22h00</span>
                </div>
                <div className="flex justify-between">
                  <span>Samedi</span>
                  <span className="text-white/80">10h00 — 20h00</span>
                </div>
                <div className="flex justify-between">
                  <span>Dimanche</span>
                  <span className="text-white/80">Ferme</span>
                </div>
              </div>
            </div>
          </motion.div>
        </div>
      </div>
    </section>
  )
}
