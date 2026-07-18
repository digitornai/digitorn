import { useParams, Link } from 'react-router-dom';
import { ArrowLeft, ShoppingBag, Star, Minus, Plus, Check } from 'lucide-react';
import { useState } from 'react';
import { products } from '../data/products';
import { useCartStore } from '../store/cart';

export default function ProductDetail() {
  const { id } = useParams<{ id: string }>();
  const product = products.find((p) => p.id === id);
  const addItem = useCartStore((s) => s.addItem);
  const [quantity, setQuantity] = useState(1);
  const [added, setAdded] = useState(false);

  if (!product) {
    return (
      <div className="max-w-7xl mx-auto px-4 py-20 text-center">
        <h2 className="text-2xl font-bold text-gray-900 mb-4">Produit introuvable</h2>
        <Link to="/products" className="text-gray-500 hover:text-gray-900 underline">
          Retour a la boutique
        </Link>
      </div>
    );
  }

  const handleAdd = () => {
    for (let i = 0; i < quantity; i++) addItem(product);
    setAdded(true);
    setTimeout(() => setAdded(false), 2000);
  };

  return (
    <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8">
      <Link
        to="/products"
        className="inline-flex items-center gap-1 text-sm text-gray-500 hover:text-gray-900 mb-8 transition-colors"
      >
        <ArrowLeft size={16} /> Retour a la boutique
      </Link>

      <div className="grid grid-cols-1 md:grid-cols-2 gap-12">
        <div className="aspect-square rounded-2xl overflow-hidden bg-gray-50">
          <img
            src={product.image}
            alt={product.name}
            className="w-full h-full object-cover"
          />
        </div>

        <div>
          <p className="text-sm font-medium text-purple-600 uppercase tracking-wider mb-2">
            {product.category}
          </p>
          <h1 className="text-3xl font-bold text-gray-900 mb-3">{product.name}</h1>

          <div className="flex items-center gap-2 mb-4">
            <div className="flex">
              {Array.from({ length: 5 }).map((_, i) => (
                <Star
                  key={i}
                  size={16}
                  className={i < Math.floor(product.rating) ? 'fill-amber-400 text-amber-400' : 'text-gray-200'}
                />
              ))}
            </div>
            <span className="text-sm text-gray-500">{product.rating}/5</span>
          </div>

          <p className="text-3xl font-bold text-gray-900 mb-4">
            {product.price.toFixed(2)} €
          </p>

          <p className="text-gray-600 leading-relaxed mb-8">{product.description}</p>

          <p className="text-sm text-gray-500 mb-6">
            {product.stock > 0 ? `${product.stock} en stock` : 'Rupture de stock'}
          </p>

          {/* Quantity Selector */}
          <div className="flex items-center gap-4 mb-6">
            <span className="text-sm font-medium text-gray-700">Quantite :</span>
            <div className="flex items-center border border-gray-200 rounded-lg">
              <button
                onClick={() => setQuantity(Math.max(1, quantity - 1))}
                className="p-2 hover:bg-gray-50 transition-colors"
              >
                <Minus size={16} />
              </button>
              <span className="px-4 py-2 font-medium text-sm">{quantity}</span>
              <button
                onClick={() => setQuantity(Math.min(product.stock, quantity + 1))}
                className="p-2 hover:bg-gray-50 transition-colors"
              >
                <Plus size={16} />
              </button>
            </div>
          </div>

          <button
            onClick={handleAdd}
            disabled={product.stock === 0}
            className={`w-full flex items-center justify-center gap-2 px-6 py-3 rounded-full font-semibold transition-colors ${
              added
                ? 'bg-green-600 text-white'
                : 'bg-gray-900 text-white hover:bg-gray-800'
            } disabled:bg-gray-300 disabled:cursor-not-allowed`}
          >
            {added ? (
              <>
                <Check size={18} /> Ajoute au panier
              </>
            ) : (
              <>
                <ShoppingBag size={18} /> Ajouter au panier
              </>
            )}
          </button>
        </div>
      </div>
    </div>
  );
}
