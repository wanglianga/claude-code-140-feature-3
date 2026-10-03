import React, { useEffect, useState } from 'react'
import { api, fmtMoney, fmtNum, stageLabel, STAGES } from '../../lib/api'
import { Badge, handleErr, RevenueChart, useToast } from '../ui'

const REASONS = [
  '顶楼住户担心漏水，需补充防水方案',
  '物业担心维护通道被占用，重新规划',
  '住户质疑收益测算，需第三方复核',
  '设备供应延迟，组件到货推迟',
  '电网并网时间变化，验收顺延',
]

export default function OverviewTab({ user, building, dash, reload }) {
  const { notify } = useToast()
  const [logs, setLogs] = useState([])
  const isManager = user.role === 'admin' || user.role === 'property'
  const curIdx = STAGES.findIndex((s) => s.key === building.stage)
  const next = STAGES[curIdx + 1]
  const [reason, setReason] = useState('')

  const loadLogs = async () => {
    const r = await api('GET', `/buildings/${building.id}/stage-logs`).catch(() => ({ items: [] }))
    setLogs(r.items || [])
  }
  useEffect(() => { loadLogs() }, [building.id])

  const move = async (to, reasonText) => {
    try {
      await handleErr(api('POST', `/buildings/${building.id}/stage`, { to_stage: to, reason: reasonText }), notify)
      notify('阶段已调整为：' + stageLabel(to))
      setReason('')
      reload()
      loadLogs()
    } catch { /* toast */ }
  }

  const carbon = Number(dash?.total_kwh || 0) * 0.581

  return (
    <>
      <div className="grid grid-4">
        <div className="card stat accent">
          <div className="label">累计发电</div>
          <div className="value">{fmtNum(dash?.total_kwh)} <span className="small">kWh</span></div>
          <div className="sub">碳减排约 {fmtNum(carbon)} kg CO₂</div>
        </div>
        <div className="card stat accent">
          <div className="label">累计发电收入+补贴</div>
          <div className="value">¥{fmtMoney(Number(dash?.gross_revenue || 0) + Number(dash?.total_subsidy || 0))}</div>
          <div className="sub">电费 {fmtMoney(dash?.gross_revenue)} · 补贴 {fmtMoney(dash?.total_subsidy)}</div>
        </div>
        <div className="card stat">
          <div className="label">公共账户余额</div>
          <div className="value">¥{fmtMoney(dash?.public_balance)}</div>
          <div className="sub">维修基金 ¥{fmtMoney(dash?.maintenance_fund_balance)}</div>
        </div>
        <div className="card stat">
          <div className="label">维修工单</div>
          <div className="value">{dash?.work_orders_open || 0} <span className="small">待处理</span></div>
          <div className="sub">已解决 {dash?.work_orders_resolved || 0} 件</div>
        </div>
      </div>

      <div className="muted-box">
        <b>逆变器故障影响分摊：</b>
        {(dash?.faults_open || 0) > 0
          ? <span className="badge red" style={{ marginRight: 6 }}>{dash.faults_open} 台停机中</span>
          : <span className="badge green" style={{ marginRight: 6 }}>设备运行正常</span>}
        {(dash?.fault_compensation_pending || 0) > 0 &&
          <span className="badge amber" style={{ marginRight: 6 }}>{dash.fault_compensation_pending} 笔补偿待发放</span>}
        {(dash?.same_model_review_pending || 0) > 0 &&
          <span className="badge amber" style={{ marginRight: 6 }}>{dash.same_model_review_pending} 台同型号待复查</span>}
        <span className="small muted">
          已认定故障收益损失 ¥{fmtMoney(dash?.fault_total_loss)} · 已发放赔付 ¥{fmtMoney(dash?.fault_paid_compensation)}
          （设备质量由供应商赔付住户；通道占用延迟由物业赔偿入楼栋公共账）
        </span>
      </div>

      <div className="card">
        <div className="card-title"><h2>月度收益走势（定稿月份）</h2></div>
        <RevenueChart series={dash?.monthly_series || []} />
      </div>

      <div className="card">
        <div className="card-title">
          <h2>项目阶段管理</h2>
          <Badge value={building.stage} />
        </div>
        {isManager ? (
          <>
            <div className="btn-row" style={{ marginBottom: 10 }}>
              {next && (
                <button className="primary" onClick={() => move(next.key, '')}>
                  推进到：{next.label}
                </button>
              )}
              <select value={reason} onChange={(e) => setReason(e.target.value)} style={{ maxWidth: 320 }}>
                <option value="">选择回退/调整原因…</option>
                {REASONS.map((r) => <option key={r} value={r}>{r}</option>)}
              </select>
              {STAGES.filter((s) => s.key !== building.stage).map((s) => (
                <button key={s.key} disabled={!reason} onClick={() => move(s.key, reason)}>
                  调整到：{s.label}
                </button>
              ))}
            </div>
            <div className="muted-box">
              阶段回退属于计划调整，必须选择原因（漏水担忧 / 维护通道 / 收益质疑 / 供货延迟 / 并网变化）；
              进入「认购缴费」前必须先通过楼栋立项表决。
            </div>
          </>
        ) : (
          <div className="muted-box">阶段推进由物业/管理员操作。如对方案有异议，可在「公告/异议/年报」页发起住户异议投票。</div>
        )}
        <table style={{ marginTop: 12 }}>
          <thead><tr><th>时间</th><th>阶段变化</th><th>原因</th><th>操作人</th></tr></thead>
          <tbody>
            {logs.map((l) => (
              <tr key={l.id}>
                <td className="small">{new Date(l.at).toLocaleString('zh-CN')}</td>
                <td>{l.from ? stageLabel(l.from) + ' → ' : ''}{stageLabel(l.to)}</td>
                <td>{l.reason || '—'}</td>
                <td className="small">{l.operator}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div className="card">
        <h2>立项档案</h2>
        <dl className="kv">
          <dt>屋顶面积</dt><dd>{fmtNum(building.roof_area)} m²</dd>
          <dt>朝向</dt><dd>{building.orientation || '—'}</dd>
          <dt>承重评估</dt><dd>{building.load_assessment || '—'}</dd>
          <dt>遮挡情况</dt><dd>{building.shading || '—'}</dd>
          <dt>产权关系</dt><dd>{building.ownership_note || '—'}</dd>
          <dt>公共收益规则</dt><dd>
            公共净收益的 {building.public_revenue_rule?.maintenance_fund_ratio ?? 20}% 划入维修基金
            {building.public_revenue_rule?.note ? `（${building.public_revenue_rule.note}）` : ''}
          </dd>
          <dt>并网条件</dt><dd>{building.grid_condition || '—'}</dd>
          <dt>施工影响</dt><dd>{building.construction_impact || '—'}</dd>
          <dt>装机/组件</dt><dd>{fmtNum(building.total_capacity_kw)} kW · {building.panel_total} 块 · 单价 ¥{fmtMoney(building.unit_price)}</dd>
          <dt>住户数</dt><dd>{dash?.resident_count ?? '—'}</dd>
        </dl>
      </div>
    </>
  )
}
