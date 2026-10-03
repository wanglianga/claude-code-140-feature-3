import React, { useEffect, useState } from 'react'
import { api, fmtMoney } from '../../lib/api'
import { handleErr, useToast } from '../ui'

const CAT_LABEL = {
  subscription: '认购款', revenue: '发电收入', subsidy: '发电补贴',
  public: '公共收益', maintenance_fund: '维修基金', repair: '维修支出',
  om: '运维费', tax: '税费', refund: '退款', compensation: '责任赔偿',
  transfer_in: '权益转入', fault_comp: '故障赔付',
}

export default function LedgerTab({ user, building }) {
  const { notify } = useToast()
  const [ledger, setLedger] = useState([])
  const [payments, setPayments] = useState([])
  const [summary, setSummary] = useState({})
  const [month, setMonth] = useState('')

  const load = async () => {
    try {
      const q = month ? `?period_month=${month}` : ''
      const [l, p] = await Promise.all([
        handleErr(api('GET', `/buildings/${building.id}/ledger${q}`), notify),
        api('GET', `/buildings/${building.id}/payments`).catch(() => ({ items: [] })),
      ])
      setLedger(l.items || [])
      setSummary(l)
      setPayments(p.items || [])
    } catch { /* toast */ }
  }
  useEffect(() => { load() }, [building.id, month])

  return (
    <>
      <div className="grid grid-3">
        <div className="card stat accent">
          <div className="label">公共账户净额</div>
          <div className="value">¥{fmtMoney(summary.public_balance)}</div>
          <div className="sub">认购款、发电收入、补贴、支出轧差</div>
        </div>
        <div className="card stat accent">
          <div className="label">维修基金余额</div>
          <div className="value">¥{fmtMoney(summary.maintenance_fund_balance)}</div>
          <div className="sub">公共收益计提 − 维修动用</div>
        </div>
        <div className="card stat">
          <div className="label">台账流水</div>
          <div className="value">{ledger.length} <span className="small">条（最多 500）</span></div>
        </div>
      </div>

      <div className="card">
        <div className="card-title">
          <h2>公共账户 / 住户台账流水</h2>
          <div className="btn-row">
            <input style={{ width: 130 }} placeholder="YYYY-MM 筛选" value={month} onChange={(e) => setMonth(e.target.value)} />
            {month && <button className="sm" onClick={() => setMonth('')}>清除筛选</button>}
          </div>
        </div>
        <table>
          <thead>
            <tr><th>时间</th><th>月份</th><th>账户</th><th>收支类别</th><th>方向</th><th className="num">金额</th><th>摘要</th></tr>
          </thead>
          <tbody>
            {ledger.map((l) => (
              <tr key={l.id}>
                <td className="small">{new Date(l.created_at).toLocaleString('zh-CN')}</td>
                <td className="small">{l.period_month || '—'}</td>
                <td>{l.party}</td>
                <td>{CAT_LABEL[l.category] || l.category}</td>
                <td>{l.direction === 'in'
                  ? <span className="badge green">收入</span>
                  : <span className="badge red">支出</span>}</td>
                <td className="num">¥{fmtMoney(l.amount)}</td>
                <td className="small muted">{l.memo}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {ledger.length === 0 && <div className="list-empty">暂无流水</div>}
      </div>

      <div className="card">
        <h2>收付款记录（电网/补贴/认购/退款）</h2>
        <table>
          <thead><tr><th>时间</th><th>对象</th><th>类型</th><th>状态</th><th className="num">金额</th><th>备注</th></tr></thead>
          <tbody>
            {payments.map((p) => (
              <tr key={p.id}>
                <td className="small">{new Date(p.created_at).toLocaleString('zh-CN')}</td>
                <td>{p.party}</td>
                <td>{CAT_LABEL[p.pay_type] || p.pay_type}</td>
                <td>{p.status}</td>
                <td className="num">¥{fmtMoney(p.amount)}</td>
                <td className="small muted">{p.memo}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  )
}
