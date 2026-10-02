import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App.tsx'
import './styles/index.css'

const container = document.getElementById('root')
if (!container) {
  // Fail loudly rather than rendering into a null root.
  throw new Error('root container #root is missing from index.html')
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
