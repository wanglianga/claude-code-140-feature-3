import React, { createContext, useCallback, useContext, useState } from 'react'

// ---------- Toast ----------
const ToastCtx = createContext({ notify: () => {} })

export function ToastProvider({ children }) {
  const [toast, setToast] = useState(null)
  const notify = useCallback((msg, isErr = false) => {
    setToast({ msg, isErr })
    setTimeout(() => setToast(null), 2600)
  }, [])
  return (
    <ToastCtx.Provider value={{ notify }}>
      {children}
      {toast && <div className={'toast' + (toast.isErr ? ' err' : '')}>{toast.msg}</div>}
    </ToastCtx.Provider>
  )
}
export const useToast = () => useContext(ToastCtx)

export async function handleErr(promise, notify) {
  try {
    return await promise
  } catch (e) {
    notify(e.message, true)
    throw e
  }
}

// ---------- Badges ----------
const STATUS_STYLE = {
  // stage
  draft: 'gray', voting: 'amber', subscribing: 'blue', construction: 'amber',
  grid_acceptance: 'blue', operating: 'green',
  // generic
  paid: 'green', pending: 'amber', refunded: 'red', transferred: 'blue',
  open: 'amber', closed: 'gray', resolved: 'green', processing: 'blue',
  approved: 'green', rejected: 'red',
  finalized: 'green',
  normal: 'green', faulty: 'red', maintenance: 'amber',
  explained: 'green',
  covered: 'green', insufficient: 'red',
  shareholder: 'blue', public_only: 'gray',
}
export function Badge({ value }) {
  return <span className={'badge ' + (STATUS_STYLE[value] || 'gray')}>{value}</span>
}

// ---------- Progress bar ----------
export function RatioBar({ ratio }) {
  const cls = ratio >= 100 ? 'red' : ratio >= 70 ? 'amber' : ''
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
      <div className="bar-track" style={{ flex: 1 }}>
        <div className={'bar-fill ' + cls} style={{ width: Math.min(100, ratio) + '%' }} />
      </div>
      <span className="small">{Number(ratio || 0).toFixed(2)}%</span>
    </div>
  )
}

// ---------- Refresh hook ----------
export function useRefresh() {
  const [tick, setTick] = useState(0)
  return [tick, () => setTick((t) => t + 1)]
}

// ---------- Money chart ----------
export function RevenueChart({ series }) {
  if (!series?.length) return <div className="list-empty">暂无结算数据</div>
  const max = Math.max(...series.map((s) => Number(s.gross_revenue) + Number(s.subsidy)), 1)
  return (
    <div className="chart">
      {series.map((s) => {
        const rev = Number(s.gross_revenue)
        const sub = Number(s.subsidy)
        return (
          <div className="col" key={s.period_month} title={`收入 ${rev.toFixed(2)} / 补贴 ${sub.toFixed(2)}`}>
            <div className="bar" style={{ height: (rev / max) * 100 + 'px' }} />
            <div className="bar subsidy" style={{ height: Math.max(2, (sub / max) * 100) + 'px', marginTop: -4 }} />
            <div className="lbl">{s.period_month.slice(5)}月</div>
          </div>
        )
      })}
    </div>
  )
}
