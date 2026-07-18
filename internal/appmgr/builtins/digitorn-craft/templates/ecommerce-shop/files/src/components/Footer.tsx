import { Link } from 'react-router-dom';
import { Mail, Globe, Phone } from 'lucide-react';

export default function Footer() {
  return (
    <footer className="bg-gray-950 text-gray-400 mt-auto">
      <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-12">
        <div className="grid grid-cols-1 md:grid-cols-3 gap-8">
          <div>
            <h3 className="text-white font-bold text-lg mb-3">MARCHÉ</h3>
            <p className="text-sm leading-relaxed">
              Votre destination pour des produits de qualifie, selectionnes avec soin.
            </p>
          </div>
          <div>
            <h4 className="text-white font-semibold mb-3">Navigation</h4>
            <ul className="space-y-2 text-sm">
              <li><Link to="/" className="hover:text-white transition-colors">Accueil</Link></li>
              <li><Link to="/products" className="hover:text-white transition-colors">Boutique</Link></li>
              <li><Link to="/cart" className="hover:text-white transition-colors">Panier</Link></li>
            </ul>
          </div>
          <div>
            <h4 className="text-white font-semibold mb-3">Suivez-nous</h4>
            <div className="flex gap-4">
              <a href="#" className="hover:text-white transition-colors"><Globe size={20} /></a>
              <a href="#" className="hover:text-white transition-colors"><Mail size={20} /></a>
              <a href="#" className="hover:text-white transition-colors"><Phone size={20} /></a>
            </div>
          </div>
        </div>
        <div className="border-t border-gray-800 mt-8 pt-8 text-center text-sm">
          <p>&copy; 2025 MARCHE. Tous droits reserves.</p>
        </div>
      </div>
    </footer>
  );
}
