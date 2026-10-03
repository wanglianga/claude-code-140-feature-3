import React, { useEffect, useState } from 'react'
import { api, STAGES } from '../lib/api'
import { handleErr, useToast } from '../components/ui'
import OverviewTab from '../components/tabs/OverviewTab'
import VoteTab from '../components/tabs/VoteTab'
import SubscriptionTab from '../components/tabs/SubscriptionTab'
import GenerationTab from '../components/tabs/GenerationTab'
import FaultTab from '../components/tabs/FaultTab'
import WorkOrderTab from '../components/tabs/WorkOrderTab'
import SettlementTab from '../components/tabs/SettlementTab'
import LedgerTab from '../components/tabs/LedgerTab'
import TransferTab from '../components/tabs/TransferTab'
import CommunityTab from '../components/tabs/CommunityTab'

const TABS = [
  { key: 'overview', label: '概览与阶段' },
  { key: 'vote', label: '楼栋表决' },
  { key: 'subscription', label: '认购缴费' },
  { key: 'generation', label: '发电计量' },
  { key: 'fault', label: '故障分摊' },
  { key: 'workorder', label: '维修工单' },
  { key: 'settlement', label: '月度结算' },
  { key: 'ledger', label: '账目台账' },
  { key: 'transfer', label: '房屋转让' },
  { key: 'community', label: '公告/异议/年报' },
]

export default function BuildingDetail({ user, id }) {
  const { notify } = useToast()
  const [tab, setTab] = useState('overview')
  const [building, setBuilding] = useState(null)
  const [dash, setDash] = useState(null)

  const load = async () => {
    try {
      const [b, d] = await Promise.all([
        handleErr(api('GET', `/buildings/${id}`), notify),
        api('GET', `/buildings/${id}/dashboard`).catch(() => ({})),
      ])
      setBuilding(b)
      setDash(d)
    } catch { /* toast */ }
  }
  useEffect(() => { load() }, [id])

  if (!building) return <div className="container"><div className="muted">加载中…</div></div>
  const curIdx = STAGES.findIndex((s) => s.key === building.stage)

  return (
    <div className="container">
      <div className="card">
        <div className="card-title">
          <div>
            <h1 style={{ marginBottom: 2 }}>{building.name}</h1>
            <span className="muted small">{building.address}</span>
          </div>
          <div className="btn-row">
            <button onClick={load}>刷新</button>
          </div>
        </div>
        <div className="stepper">
          {STAGES.map((s, i) => (
            <div
              key={s.key}
              className={'step ' + (i < curIdx ? 'done' : i === curIdx ? 'current' : '')}
            >
              <span className="dot" />
              <span className="name">{s.label}</span>
            </div>
          ))}
        </div>
      </div>

      <div className="tabs">
        {TABS.map((t) => (
          <button key={t.key} className={tab === t.key ? 'active' : ''} onClick={() => setTab(t.key)}>
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'overview' && <OverviewTab user={user} building={building} dash={dash} reload={load} />}
      {tab === 'vote' && <VoteTab user={user} building={building} reload={load} />}
      {tab === 'subscription' && <SubscriptionTab user={user} building={building} reload={load} />}
      {tab === 'generation' && <GenerationTab user={user} building={building} />}
      {tab === 'fault' && <FaultTab user={user} building={building} />}
      {tab === 'workorder' && <WorkOrderTab user={user} building={building} />}
      {tab === 'settlement' && <SettlementTab user={user} building={building} />}
      {tab === 'ledger' && <LedgerTab user={user} building={building} />}
      {tab === 'transfer' && <TransferTab user={user} building={building} />}
      {tab === 'community' && <CommunityTab user={user} building={building} />}
    </div>
  )
}
