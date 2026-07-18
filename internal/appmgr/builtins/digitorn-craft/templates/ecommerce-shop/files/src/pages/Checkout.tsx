import { useState } from 'react';
import { Link } from 'react-router-dom';
import { Check, CreditCard, Truck, Shield } from 'lucide-react';
import { useCartStore } from '../store/cart';

type CheckoutStep = 'info' | 'payment' | 'success';

export default function Checkout() {
  const { items, totalPrice, clearCart } = useCartStore();
  const total = totalPrice();
  const [step, setStep] = useState<CheckoutStep>('info');
  const [form, setForm] = useState({
    email: '',
    firstName: '',
    lastName: '',
    address: '',
    city: '',
    zip: '',
    cardNumber: '',
    expiry: '',
    cvc: '',
  });

  const update = (field: string, value: string) =>
    setForm((prev) => ({ ...prev, [field]: value }));

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (step === 'info') setStep('payment');
    else if (step === 'payment') {
      clearCart();
      setStep('success');
    }
  };

  if (items.length === 0 && step !== 'success') {
    return (
      <div className="max-w-7xl mx-auto px-4 py-20 text-center">
        <h2 className="text-2xl font-bold text-gray-900 mb-2">Panier vide</h2>
        <p className="text-gray-500 mb-6">Ajoutez des produits avant de passer commande.</p>
        <Link to="/products" className="text-gray-900 underline font-medium">
          Retour a la boutique
        </Link>
      </div>
    );
  }

  if (step === 'success') {
    return (
      <div className="max-w-7xl mx-auto px-4 py-20 text-center">
        <div className="w-16 h-16 bg-green-100 rounded-full flex items-center justify-center mx-auto mb-6">
          <Check size={32} className="text-green-600" />
        </div>
        <h2 className="text-3xl font-bold text-gray-900 mb-2">Commande confirmee !</h2>
        <p className="text-gray-500 mb-8">
          Merci pour votre achat. Vous recevrez un email de confirmation.
        </p>
        <Link
          to="/products"
          className="inline-flex items-center gap-2 bg-gray-900 text-white px-6 py-3 rounded-full font-semibold hover:bg-gray-800 transition-colors"
        >
          Continuer vos achats
        </Link>
      </div>
    );
  }

  return (
    <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8">
      <h1 className="text-3xl font-bold text-gray-900 mb-8">Commande</h1>

      {/* Steps Indicator */}
      <div className="flex items-center gap-4 mb-8">
        {[
          { key: 'info', label: 'Livraison', icon: Truck },
          { key: 'payment', label: 'Paiement', icon: CreditCard },
        ].map((s, i) => (
          <div key={s.key} className="flex items-center gap-2">
            {i > 0 && <div className="w-8 h-px bg-gray-200" />}
            <div
              className={`flex items-center gap-2 text-sm font-medium ${
                step === s.key ? 'text-gray-900' : 'text-gray-400'
              }`}
            >
              <div
                className={`w-8 h-8 rounded-full flex items-center justify-center text-xs font-bold ${
                  step === s.key ? 'bg-gray-900 text-white' : 'bg-gray-100 text-gray-400'
                }`}
              >
                {i + 1}
              </div>
              {s.label}
            </div>
          </div>
        ))}
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-8">
        <form onSubmit={handleSubmit} className="lg:col-span-2 space-y-6">
          {step === 'info' ? (
            <>
              <div className="space-y-4">
                <h3 className="font-semibold text-gray-900">Informations personnelles</h3>
                <input
                  type="email"
                  required
                  placeholder="Email"
                  value={form.email}
                  onChange={(e) => update('email', e.target.value)}
                  className="w-full border border-gray-200 rounded-lg px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-gray-900 focus:border-transparent"
                />
                <div className="grid grid-cols-2 gap-4">
                  <input
                    required
                    placeholder="Prenom"
                    value={form.firstName}
                    onChange={(e) => update('firstName', e.target.value)}
                    className="border border-gray-200 rounded-lg px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-gray-900 focus:border-transparent"
                  />
                  <input
                    required
                    placeholder="Nom"
                    value={form.lastName}
                    onChange={(e) => update('lastName', e.target.value)}
                    className="border border-gray-200 rounded-lg px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-gray-900 focus:border-transparent"
                  />
                </div>
              </div>
              <div className="space-y-4">
                <h3 className="font-semibold text-gray-900">Adresse de livraison</h3>
                <input
                  required
                  placeholder="Adresse"
                  value={form.address}
                  onChange={(e) => update('address', e.target.value)}
                  className="w-full border border-gray-200 rounded-lg px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-gray-900 focus:border-transparent"
                />
                <div className="grid grid-cols-2 gap-4">
                  <input
                    required
                    placeholder="Ville"
                    value={form.city}
                    onChange={(e) => update('city', e.target.value)}
                    className="border border-gray-200 rounded-lg px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-gray-900 focus:border-transparent"
                  />
                  <input
                    required
                    placeholder="Code postal"
                    value={form.zip}
                    onChange={(e) => update('zip', e.target.value)}
                    className="border border-gray-200 rounded-lg px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-gray-900 focus:border-transparent"
                  />
                </div>
              </div>
              <button
                type="submit"
                className="w-full bg-gray-900 text-white px-6 py-3 rounded-full font-semibold hover:bg-gray-800 transition-colors"
              >
                Continuer vers le paiement
              </button>
            </>
          ) : (
            <>
              <div className="space-y-4">
                <h3 className="font-semibold text-gray-900">Paiement</h3>
                <input
                  type="text"
                  required
                  placeholder="Numero de carte"
                  value={form.cardNumber}
                  onChange={(e) => update('cardNumber', e.target.value)}
                  className="w-full border border-gray-200 rounded-lg px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-gray-900 focus:border-transparent"
                />
                <div className="grid grid-cols-2 gap-4">
                  <input
                    type="text"
                    required
                    placeholder="MM/AA"
                    value={form.expiry}
                    onChange={(e) => update('expiry', e.target.value)}
                    className="border border-gray-200 rounded-lg px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-gray-900 focus:border-transparent"
                  />
                  <input
                    type="text"
                    required
                    placeholder="CVC"
                    value={form.cvc}
                    onChange={(e) => update('cvc', e.target.value)}
                    className="border border-gray-200 rounded-lg px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-gray-900 focus:border-transparent"
                  />
                </div>
              </div>
              <div className="flex gap-3">
                <button
                  type="button"
                  onClick={() => setStep('info')}
                  className="flex-1 border border-gray-200 text-gray-700 px-6 py-3 rounded-full font-semibold hover:bg-gray-50 transition-colors"
                >
                  Retour
                </button>
                <button
                  type="submit"
                  className="flex-1 bg-gray-900 text-white px-6 py-3 rounded-full font-semibold hover:bg-gray-800 transition-colors"
                >
                  Payer {(total + (total >= 50 ? 0 : 4.99)).toFixed(2)} €
                </button>
              </div>
            </>
          )}
        </form>

        {/* Order Summary */}
        <div className="lg:col-span-1">
          <div className="bg-gray-50 rounded-xl p-6 sticky top-24">
            <h2 className="font-bold text-gray-900 mb-4">Votre commande</h2>
            <div className="space-y-3 mb-4">
              {items.map((item) => (
                <div key={item.product.id} className="flex gap-3">
                  <img
                    src={item.product.image}
                    alt={item.product.name}
                    className="w-12 h-12 object-cover rounded-lg"
                  />
                  <div className="flex-1 min-w-0">
                    <p className="text-sm font-medium text-gray-900 truncate">{item.product.name}</p>
                    <p className="text-xs text-gray-500">x{item.quantity}</p>
                  </div>
                  <p className="text-sm font-medium">{(item.product.price * item.quantity).toFixed(2)} €</p>
                </div>
              ))}
            </div>
            <div className="border-t border-gray-200 pt-3 space-y-2 text-sm">
              <div className="flex justify-between">
                <span className="text-gray-500">Sous-total</span>
                <span>{total.toFixed(2)} €</span>
              </div>
              <div className="flex justify-between">
                <span className="text-gray-500">Livraison</span>
                <span className={total >= 50 ? 'text-green-600' : ''}>
                  {total >= 50 ? 'Offerte' : '4.99 €'}
                </span>
              </div>
              <div className="flex justify-between font-bold text-gray-900 pt-2">
                <span>Total</span>
                <span>{(total + (total >= 50 ? 0 : 4.99)).toFixed(2)} €</span>
              </div>
            </div>
            <div className="mt-4 flex items-center gap-2 text-xs text-gray-400">
              <Shield size={14} />
              <span>Paiement securise (prototype - pas de vraie transaction)</span>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
