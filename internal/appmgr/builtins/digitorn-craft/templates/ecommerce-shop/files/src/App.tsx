import { Routes, Route, useLocation } from 'react-router-dom';
import { useEffect } from 'react';
import Navbar from './components/Navbar';
import Footer from './components/Footer';
import Home from './pages/Home';
import Products from './pages/Products';
import ProductDetail from './pages/ProductDetail';
import Cart from './pages/Cart';
import Checkout from './pages/Checkout';

function NotifyRouteChange() {
  const loc = useLocation();
  useEffect(() => {
    window.parent?.postMessage(
      { type: 'digi:route-change', route: loc.pathname },
      '*',
    );
  }, [loc.pathname]);
  return null;
}

export default function App() {
  return (
    <div className="min-h-screen flex flex-col bg-white">
      <NotifyRouteChange />
      <Navbar />
      <main className="flex-1">
        <Routes>
          <Route path="/" element={<Home />} />
          <Route path="/products" element={<Products />} />
          <Route path="/products/:id" element={<ProductDetail />} />
          <Route path="/cart" element={<Cart />} />
          <Route path="/checkout" element={<Checkout />} />
        </Routes>
      </main>
      <Footer />
    </div>
  );
}
