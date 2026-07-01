import { useEffect, useRef } from 'react'
import { createRoot } from 'react-dom/client'
import { createDemoUniver } from './univer-setup.js'
import './excel-chrome.css'

function UniverDemo() {
  const containerRef = useRef(null)
  const univerRef = useRef(null)

  useEffect(() => {
    if (!containerRef.current || univerRef.current) return

    const { univer } = createDemoUniver(containerRef.current)
    univerRef.current = univer

    return () => {
      univer?.dispose?.()
      univerRef.current = null
    }
  }, [])

  return <div ref={containerRef} style={{ height: '100vh', width: '100%' }} />
}

createRoot(document.getElementById('root')).render(<UniverDemo />)
