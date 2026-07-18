import { Link } from 'react-router-dom';
import { ArrowRight } from 'lucide-react';
import { products } from '../data/products';
import ProductCard from '../components/ProductCard';

export default function Home() {
  const featured = products.slice(0, 4);

  return (
    <div>
      {/* Hero */}
      <section className="relative bg-gradient-to-br from-gray-900 via-gray-800 to-gray-900 text-white overflow-hidden">
        <div className="absolute inset-0 opacity-20">
          <div className="absolute top-20 left-10 w-72 h-72 bg-purple-500 rounded-full blur-3xl" />
          <div className="absolute bottom-10 right-20 w-96 h-96 bg-blue-500 rounded-full blur-3xl" />
        </div>
        <div className="relative max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-24 md:py-32">
          <div className="max-w-2xl">
            <p className="text-sm font-medium text-purple-400 uppercase tracking-widest mb-4">
              Nouvelle collection
            </p>
            <h1 className="text-4xl md:text-6xl font-bold leading-tight mb-6">
              Trouvez ce qui
              <br />
              <span className="text-transparent bg-clip-text bg-gradient-to-r from-purple-400 to-blue-400">
                vous definit
              </span>
            </h1>
            <p className="text-lg text-gray-300 mb-8 max-w-lg">
              Decouvrez notre selection de produits soigneusement choisis pour allier style et qualifie.
            </p>
            <Link
              to="/products"
              className="inline-flex items-center gap-2 bg-white text-gray-900 px-6 py-3 rounded-full font-semibold hover:bg-gray-100 transition-colors"
            >
              Voir la collection
              <ArrowRight size={18} />
            </Link>
          </div>
        </div>
      </section>

      {/* Featured Products */}
      <section className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-16">
        <div className="flex items-center justify-between mb-8">
          <h2 className="text-2xl font-bold text-gray-900">Produits populaires</h2>
          <Link
            to="/products"
            className="text-sm font-medium text-gray-500 hover:text-gray-900 transition-colors flex items-center gap-1"
          >
            Tout voir <ArrowRight size={14} />
          </Link>
        </div>
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-6">
          {featured.map((product) => (
            <ProductCard key={product.id} product={product} />
          ))}
        </div>
      </section>

      {/* CTA Banner */}
      <section className="bg-gray-50 border-y border-gray-100">
        <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-16 text-center">
          <h2 className="text-2xl md:text-3xl font-bold text-gray-900 mb-4">
            Livraison offerte des 50€ d'achat
          </h2>
          <p className="text-gray-500 mb-6 max-w-md mx-auto">
            Profitez de la livraison gratuite sur toutes vos commandes superieures a 50 euros.
          </p>
          <Link
            to="/products"
            className="inline-flex items-center gap-2 bg-gray-900 text-white px-6 py-3 rounded-full font-semibold hover:bg-gray-800 transition-colors"
          >
            Commencer vos achats
            <ArrowRight size={18} />
          </Link>
        </div>
      </section>
    </div>
  );
}
