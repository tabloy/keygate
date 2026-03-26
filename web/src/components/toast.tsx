import { createContext, type ReactNode, useCallback, useContext, useEffect, useState } from "react"

interface Toast {
  id: number
  message: string
  type: "error" | "success"
}

interface ToastContextType {
  addToast: (message: string, type?: "error" | "success") => void
}

const ToastContext = createContext<ToastContextType>({ addToast: () => {} })

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])

  const addToast = useCallback((message: string, type: "error" | "success" = "error") => {
    const id = Date.now()
    setToasts((prev) => [...prev, { id, message, type }])
    setTimeout(() => setToasts((prev) => prev.filter((t) => t.id !== id)), 5000)
  }, [])

  return (
    <ToastContext.Provider value={{ addToast }}>
      {children}
      <div className="fixed bottom-4 right-4 z-50 space-y-2 max-w-sm">
        {toasts.map((t) => (
          <div
            key={t.id}
            className={`rounded-lg border px-4 py-3 text-sm shadow-lg animate-in slide-in-from-bottom-2 ${
              t.type === "error"
                ? "bg-red-50 border-red-200 text-red-800"
                : "bg-emerald-50 border-emerald-200 text-emerald-800"
            }`}
          >
            {t.message}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

export function useToast() {
  return useContext(ToastContext)
}

// Global reference for use outside React (in QueryClient config)
let globalAddToast: ((message: string, type?: "error" | "success") => void) | null = null

export function setGlobalToast(fn: typeof globalAddToast) {
  globalAddToast = fn
}

export function showToast(message: string, type: "error" | "success" = "error") {
  if (globalAddToast) globalAddToast(message, type)
}

/** Bridge component that wires up the global toast ref inside the React tree */
export function ToastBridge() {
  const { addToast } = useToast()
  useEffect(() => {
    setGlobalToast(addToast)
  }, [addToast])
  return null
}
