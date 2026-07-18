import { useState } from 'react';
import { products, categories } from '../data/products';
import ProductCard from '../components/ProductCard';

export default function Products() {
  const [selectedCategory, setSelectedCategory] = useState('Tous');
  const [sortBy, setSortBy] = useState<'default' | 'price-asc' | 'price-desc' | 'rating'>('default');

  let filtered = selectedCategory === 'Tous'
    ? products
    : products.filter((p) => p.category === selectedCategory);

  if (sortBy === 'price-asc') filtered = [...filtered].sort((a, b) => a.price - b.price);
  if (sortBy === 'price-desc') filtered = [...filtered].sort((a, b) => b.price - a.price);
  if (sortBy === 'rating') filtered = [...filtered].sort((a, b) => b.rating - a.rating);

  return (
    <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8">
      <div className="mb-8">
        <h1 className="text-3xl font-bold text-gray-900 mb-2">Boutique</h1>
        <p className="text-gray-500">{filtered.length} produit{filtered.length > 1 ? 's' : ''}</p>
      </div>

      {/* Filters */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4 mb-8">
        <div className="flex flex-wrap gap-2">
          {categories.map((cat) => (
            <button
              key={cat}
              onClick={() => setSelectedCategory(cat)}
              className={`px-4 py-2 rounded-full text-sm font-medium transition-colors ${
                selectedCategory === cat
                  ? 'bg-gray-900 text-white'
                  : 'bg-gray-100 text-gray-600 hover:bg-gray-200'
              }`}
            >
              {cat}
            </button>
          ))}
        </div>
        <select
          value={sortBy}
          onChange={(e) => setSortBy(e.target.value as typeof sortBy)}
          className="border border-gray-200 rounded-lg px-3 py-2 text-sm text-gray-700 bg-white"
        >
          <option value="default">Tri par defaut</option>
          <option value="price-asc">Prix croissant</option>
          <option value="price-desc">Prix decroissant</option>
          <option value="rating">Meilleures notes</option>
        </select>
      </div>

      {/* Products Grid */}
      {filtered.length > 0 ? (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-6">
          {filtered.map((product) => (
            <ProductCard key={product.id} product={product} />
          ))}
        </div>
      ) : (
        <div className="text-center py-20">
          <p className="text-gray-400 text-lg">Aucun produit dans cette categorie.</p>
        </div>
      )}
    </div>
  );
}
