import { Link } from 'react-router-dom';
import { Star } from 'lucide-react';
import type { Product } from '../types';

interface ProductCardProps {
  product: Product;
}

export default function ProductCard({ product }: ProductCardProps) {
  return (
    <Link
      to={`/products/${product.id}`}
      className="group block bg-white rounded-2xl overflow-hidden border border-gray-100 hover:shadow-lg transition-all duration-300"
    >
      <div className="aspect-square overflow-hidden bg-gray-50">
        <img
          src={product.image}
          alt={product.name}
          className="w-full h-full object-cover group-hover:scale-105 transition-transform duration-500"
        />
      </div>
      <div className="p-4">
        <p className="text-xs font-medium text-gray-400 uppercase tracking-wider mb-1">
          {product.category}
        </p>
        <h3 className="font-semibold text-gray-900 mb-1 truncate">{product.name}</h3>
        <div className="flex items-center gap-1 mb-2">
          {Array.from({ length: 5 }).map((_, i) => (
            <Star
              key={i}
              size={12}
              className={i < Math.floor(product.rating) ? 'fill-amber-400 text-amber-400' : 'text-gray-200'}
            />
          ))}
          <span className="text-xs text-gray-400 ml-1">{product.rating}</span>
        </div>
        <p className="text-lg font-bold text-gray-900">{product.price.toFixed(2)} €</p>
      </div>
    </Link>
  );
}
