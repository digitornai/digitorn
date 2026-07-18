import { Link } from 'react-router-dom';
import { Minus, Plus, Trash2, ShoppingBag, ArrowRight } from 'lucide-react';
import { useCartStore } from '../store/cart';

export default function Cart() {
  const { items, removeItem, updateQuantity, totalPrice, totalItems, clearCart } = useCartStore();
  const total = totalPrice();
  const count = totalItems();

  if (items.length === 0) {
    return (
      <div className="max-w-7xl mx-auto px-4 py-20 text-center">
        <ShoppingBag size={48} className="mx-auto text-gray-300 mb-4" />
        <h2 className="text-2xl font-bold text-gray-900 mb-2">Votre panier est vide</h2>
        <p className="text-gray-500 mb-6">Ajoutez des produits pour commencer vos achats.</p>
        <Link
          to="/products"
          className="inline-flex items-center gap-2 bg-gray-900 text-white px-6 py-3 rounded-full font-semibold hover:bg-gray-800 transition-colors"
        >
          Parcourir la boutique <ArrowRight size={18} />
        </Link>
      </div>
    );
  }

  return (
    <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8">
      <div className="flex items-center justify-between mb-8">
        <h1 className="text-3xl font-bold text-gray-900">Panier ({count})</h1>
        <button
          onClick={clearCart}
          className="text-sm text-gray-500 hover:text-red-600 transition-colors"
        >
          Vider le panier
        </button>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-8">
        {/* Items */}
        <div className="lg:col-span-2 space-y-4">
          {items.map((item) => (
            <div
              key={item.product.id}
              className="flex gap-4 p-4 bg-white border border-gray-100 rounded-xl"
            >
              <img
                src={item.product.image}
                alt={item.product.name}
                className="w-24 h-24 object-cover rounded-lg flex-shrink-0"
              />
              <div className="flex-1 min-w-0">
                <div className="flex justify-between items-start">
                  <div>
                    <h3 className="font-semibold text-gray-900 truncate">{item.product.name}</h3>
                    <p className="text-sm text-gray-500">{item.product.category}</p>
                  </div>
                  <button
                    onClick={() => removeItem(item.product.id)}
                    className="p-1 text-gray-400 hover:text-red-500 transition-colors"
                  >
                    <Trash2 size={16} />
                  </button>
                </div>
                <div className="flex items-center justify-between mt-3">
                  <div className="flex items-center border border-gray-200 rounded-lg">
                    <button
                      onClick={() => updateQuantity(item.product.id, item.quantity - 1)}
                      className="p-1.5 hover:bg-gray-50 transition-colors"
                    >
                      <Minus size={14} />
                    </button>
                    <span className="px-3 text-sm font-medium">{item.quantity}</span>
                    <button
                      onClick={() => updateQuantity(item.product.id, item.quantity + 1)}
                      className="p-1.5 hover:bg-gray-50 transition-colors"
                    >
                      <Plus size={14} />
                    </button>
                  </div>
                  <p className="font-bold text-gray-900">
                    {(item.product.price * item.quantity).toFixed(2)} €
                  </p>
                </div>
              </div>
            </div>
          ))}
        </div>

        {/* Summary */}
        <div className="lg:col-span-1">
          <div className="bg-gray-50 rounded-xl p-6 sticky top-24">
            <h2 className="font-bold text-gray-900 mb-4">Resume</h2>
            <div className="space-y-3 text-sm">
              <div className="flex justify-between">
                <span className="text-gray-500">Sous-total ({count} articles)</span>
                <span className="font-medium">{total.toFixed(2)} €</span>
              </div>
              <div className="flex justify-between">
                <span className="text-gray-500">Livraison</span>
                <span className="font-medium text-green-600">
                  {total >= 50 ? 'Offerte' : '4.99 €'}
                </span>
              </div>
              <div className="border-t border-gray-200 pt-3 flex justify-between">
                <span className="font-bold text-gray-900">Total</span>
                <span className="font-bold text-gray-900">
                  {(total + (total >= 50 ? 0 : 4.99)).toFixed(2)} €
                </span>
              </div>
            </div>
            {total < 50 && (
              <p className="text-xs text-gray-500 mt-3 text-center">
                Ajoutez {(50 - total).toFixed(2)} € pour la livraison offerte
              </p>
            )}
            <Link
              to="/checkout"
              className="block w-full mt-4 bg-gray-900 text-white text-center px-6 py-3 rounded-full font-semibold hover:bg-gray-800 transition-colors"
            >
              Commander
            </Link>
          </div>
        </div>
      </div>
    </div>
  );
}
