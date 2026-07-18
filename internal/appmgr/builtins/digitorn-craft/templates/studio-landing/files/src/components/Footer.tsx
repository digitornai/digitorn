import { Heart } from 'lucide-react'

export default function Footer() {
  return (
    <footer className="bg-primary/95 text-white/60 py-12 border-t border-white/10">
      <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
        <div className="grid sm:grid-cols-2 lg:grid-cols-4 gap-8 mb-12">
          {/* Brand */}
          <div className="sm:col-span-2 lg:col-span-1">
            <a href="#accueil" className="text-2xl font-bold text-white mb-4 block">
              Studio<span className="text-secondary">Danse</span>
            </a>
            <p className="text-sm leading-relaxed">
              Votre studio de danse a Paris. Cours particuliers et collectifs
              dans une atmosphere chaleureuse et professionnelle.
            </p>
          </div>

          {/* Links */}
          <div>
            <h4 className="text-white font-semibold mb-4">Navigation</h4>
            <ul className="space-y-2 text-sm">
              {['Accueil', 'A propos', 'Cours', 'Galerie', 'Avis', 'Contact'].map((link) => (
                <li key={link}>
                  <a
                    href={`#${link.toLowerCase().replace(' ', '')}`}
                    className="hover:text-secondary transition-colors"
                  >
                    {link}
                  </a>
                </li>
              ))}
            </ul>
          </div>

          <div>
            <h4 className="text-white font-semibold mb-4">Cours populaires</h4>
            <ul className="space-y-2 text-sm">
              <li><a href="#cours" className="hover:text-secondary transition-colors">Ballet Classique</a></li>
              <li><a href="#cours" className="hover:text-secondary transition-colors">Salsa & Bachata</a></li>
              <li><a href="#cours" className="hover:text-secondary transition-colors">Danse Contemporaine</a></li>
              <li><a href="#cours" className="hover:text-secondary transition-colors">Hip Hop & Street</a></li>
              <li><a href="#cours" className="hover:text-secondary transition-colors">Tango Argentin</a></li>
            </ul>
          </div>

          <div>
            <h4 className="text-white font-semibold mb-4">Suivez-nous</h4>
            <p className="text-sm mb-4">
              Retrouvez-nous sur les reseaux sociaux pour ne rien rater de
              nos cours et evenements.
            </p>
            <div className="flex gap-3">
              {['Instagram', 'Facebook', 'TikTok'].map((social) => (
                <span
                  key={social}
                  className="bg-white/10 px-3 py-1.5 rounded-lg text-xs hover:bg-secondary/20 hover:text-secondary transition-all cursor-pointer"
                >
                  {social}
                </span>
              ))}
            </div>
          </div>
        </div>

        {/* Bottom */}
        <div className="border-t border-white/10 pt-8 flex flex-col sm:flex-row items-center justify-between gap-4 text-sm">
          <p>&copy; {new Date().getFullYear()} Studio Danse. Tous droits reserves.</p>
          <p className="flex items-center gap-1.5">
            Fait avec <Heart className="w-4 h-4 text-secondary fill-secondary" /> a Paris
          </p>
        </div>
      </div>
    </footer>
  )
}
