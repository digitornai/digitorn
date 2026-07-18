import { useState } from 'react';
import { Link, useLocation } from 'react-router-dom';
import { ShoppingBag, Menu, X, Search } from 'lucide-react';
import { useCartStore } from '../store/cart';
import { cn } from '../lib/utils';

export default function Navbar() {
  const [mobileOpen, setMobileOpen] = useState(false);
  const totalItems = useCartStore((s) => s.totalItems());
  const location = useLocation();

  const links = [
    { to: '/', label: 'Accueil' },
    { to: '/products', label: 'Boutique' },
  ];

  return (
    <nav className="sticky top-0 z-50 bg-white/80 backdrop-blur-md border-b border-gray-100">
      <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
        <div className="flex items-center justify-between h-16">
          <Link to="/" className="text-xl font-bold tracking-tight text-gray-900">
            MARCHÉ
          </Link>

          <div className="hidden md:flex items-center gap-8">
            {links.map((link) => (
              <Link
                key={link.to}
                to={link.to}
                className={cn(
                  'text-sm font-medium transition-colors hover:text-gray-900',
                  location.pathname === link.to ? 'text-gray-900' : 'text-gray-500'
                )}
              >
                {link.label}
              </Link>
            ))}
          </div>

          <div className="flex items-center gap-4">
            <Link
              to="/products"
              className="p-2 text-gray-500 hover:text-gray-900 transition-colors"
            >
              <Search size={20} />
            </Link>
            <Link
              to="/cart"
              className="relative p-2 text-gray-500 hover:text-gray-900 transition-colors"
            >
              <ShoppingBag size={20} />
              {totalItems > 0 && (
                <span className="absolute -top-1 -right-1 bg-gray-900 text-white text-xs font-bold rounded-full h-5 w-5 flex items-center justify-center">
                  {totalItems}
                </span>
              )}
            </Link>
            <button
              onClick={() => setMobileOpen(!mobileOpen)}
              className="md:hidden p-2 text-gray-500"
            >
              {mobileOpen ? <X size={20} /> : <Menu size={20} />}
            </button>
          </div>
        </div>
      </div>

      {mobileOpen && (
        <div className="md:hidden border-t border-gray-100 bg-white">
          <div className="px-4 py-3 space-y-1">
            {links.map((link) => (
              <Link
                key={link.to}
                to={link.to}
                onClick={() => setMobileOpen(false)}
                className={cn(
                  'block px-3 py-2 rounded-lg text-sm font-medium',
                  location.pathname === link.to
                    ? 'bg-gray-100 text-gray-900'
                    : 'text-gray-600 hover:bg-gray-50'
                )}
              >
                {link.label}
              </Link>
            ))}
          </div>
        </div>
      )}
    </nav>
  );
}
