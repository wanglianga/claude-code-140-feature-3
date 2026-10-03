import React, { useEffect, useMemo, useState } from 'react'
import { api, fmtMoney, fmtNum, CAUSE_LABELS, COVER_LABELS } from '../../lib/api'
import { Badge, handleErr, useToast } from '../ui'

const dtLocal = (s) => (s ? new Date(s).toLocaleString('zh-CN', { hour12: false }) : '—')

function CurveBars({ points }) {
  const max = Math.max(1, ...points.map((p) => Number(p.kwh)))
  if (!points.length) return <div className="list-empty">暂无日曲线，请在修复前后录入日发电量</div>
  return (
    <div style={{ display: 'flex', alignItems: 'flex-end', gap: 2, height: 90, paddingTop: 8, overflowX: 'auto' }}>
      {points.map((p) => (
        <div key={p.day} style={{ flex: '0 0 14px', display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'flex-end' }}
          title={`${p.day}：${fmtNum(p.kwh, 1)} kWh（${p.phase === 'outage' ? '停机' : p.phase === 'recovery' ? '修复后' : '正常'}）`}>
          <div style={{
            width: '70%', minHeight: 2, borderRadius: '3px 3px 0 0',
            height: Math.max(2, (Number(p.kwh) / max) * 70) + 'px',
            background: p.phase === 'outage' ? '#d92d20' : p.phase === 'recovery' ? '#1d7a4e' : '#9ab8a7',
          }} />
        </div>
      ))}
    </div>
  )
}

export default function OutageTab({ user, building }) {
  const { notify } = useToast()
  const isManager = user.role === 'admin' || user.role === 'property'
  const [inverters, setInverters] = useState([])
  const [bindings, setBindings] = useState([])
  const [outages, setOutages] = useState([])
  const [rechecks, setRechecks] = useState([])
  const [curve, setCurve] = useState([])
  const [selected, setSelected] = useState(null)
  const [detail, setDetail] = useState(null)

  const [newOutage, setNewOutage] = useState({ inverter_id: 0, fault_at: '', title: '', response_sla_hours: 24 })
  const [assessForm, setAssessForm] = useState({
    cause_type: 'quality', baseline_kwh_per_day: 40, grid_price: 0.4153, subsidy_per_kwh: 0.05,
    channel_blocked_at: '', channel_freed_at: '',
  })
  const [curveForm, setCurveForm] = useState({ day: '', inverter_id: 0, kwh: 0, phase: 'normal' })

  const load = async () => {
    const [iv, bd, ou, rc, cv] = await Promise.all([
      api('GET', `/buildings/${building.id}/inverters`),
      api('GET', `/buildings/${building.id}/panel-bindings`).catch(() => ({ items: [] })),
      api('GET', `/buildings/${building.id}/outages`).catch(() => ({ items: [] })),
      api('GET', `/buildings/${building.id}/rechecks`).catch(() => ({ items: [] })),
      api('GET', `/buildings/${building.id}/generation-curve`).catch(() => ({ items: [] })),
    ])
    setInverters(iv.items || [])
    setBindings(bd.items || [])
    setOutages(ou.items || [])
    setRechecks(rc.items || [])
    setCurve(cv.items || [])
    if (selected) {
      const d = await api('GET', `/outages/${selected}`).catch(() => null)
      setDetail(d)
    }
  }
  useEffect(() => { load() }, [building.id]) // eslint-disable-line

  const openRechecks = rechecks.filter((r) => r.status === 'open')

  const bindingsByInv = useMemo(() => {
    const m = {}
    for (const b of bindings) (m[b.inverter_id] ||= []).push(b)
    return m
  }, [bindings])

  const createOutage = async () => {
    if (!newOutage.inverter_id) return notify('请选择故障逆变器', true)
    try {
      const r = await handleErr(api('POST', `/buildings/${building.id}/outages`, newOutage), notify)
      notify('停机事件已登记')
      setSelected(r.id)
      setNewOutage({ ...newOutage, title: '', fault_at: '' })
      load()
    } catch { /* toast */ }
  }

  const saveWarranty = async (iv) => {
    const model = window.prompt('设备型号', iv.model || '')
    if (model === null) return
    const supplier = window.prompt('供应商', iv.supplier || '')
    if (supplier === null) return
    const warranty_until = window.prompt('保修截止日（YYYY-MM-DD，清空表示无保修信息）', iv.warranty_until || '')
    if (warranty_until === null) return
    try {
      await handleErr(api('PATCH', `/inverters/${iv.id}`, { model, supplier, warranty_until }), notify)
      notify('设备档案已更新')
      load()
    } catch { /* toast */ }
  }

  const autoBind = async () => {
    if (!window.confirm('按当前认购份额与逆变器装机比例自动生成组件绑定？已有绑定时不可用。')) return
    try {
      await handleErr(api('POST', `/buildings/${building.id}/panel-bindings/auto`), notify)
      notify('已按认购份额自动绑定')
      load()
    } catch { /* toast */ }
  }

  const editBinding = async (inv, b) => {
    const panels = Number(window.prompt(`绑定组件块数（${b ? b.name : inv.name}）`, b ? b.panel_count : 0))
    if (Number.isNaN(panels)) return
    const ratio = Number(window.prompt('占整楼组件比例 %', b ? b.ratio : 0))
    if (Number.isNaN(ratio)) return
    try {
      await handleErr(api('PUT', `/buildings/${building.id}/panel-bindings`, {
        inverter_id: inv, user_id: b ? b.user_id : 0, panel_count: panels, ratio,
      }), notify)
      notify('绑定已保存')
      load()
    } catch { /* toast */ }
  }

  const assess = async () => {
    try {
      const r = await handleErr(api('POST', `/outages/${selected}/assess`, {
        ...assessForm,
        channel_blocked_at: assessForm.cause_type === 'mixed' || assessForm.cause_type === 'channel'
          ? assessForm.channel_blocked_at : '',
        channel_freed_at: assessForm.cause_type === 'mixed' || assessForm.cause_type === 'channel'
          ? assessForm.channel_freed_at : '',
      }), notify)
      setDetail(r)
      notify('损失测算与责任分摊已更新')
      load()
    } catch { /* toast */ }
  }

  const setTimeline = async (patch, msg) => {
    try {
      await handleErr(api('POST', `/outages/${selected}/timeline`, patch), notify)
      notify(msg)
      load()
    } catch { /* toast */ }
  }

  const postComp = async () => {
    if (!window.confirm('确认把已登记赔付入账？住户部分进入住户收益补偿，公共部分与物业责任说明进入楼栋公共账。')) return
    try {
      await handleErr(api('POST', `/outages/${selected}/post-compensation`), notify)
      notify('赔付已入账')
      load()
    } catch { /* toast */ }
  }

  const review = async () => {
    try {
      const r = await handleErr(api('POST', `/outages/${selected}/review`, {
        baseline_days: 14, recovery_days: 14,
      }), notify)
      notify(`曲线复核完成：${COVER_LABELS[r.review.cover_status] || r.review.cover_status}`)
      load()
    } catch { /* toast */ }
  }

  const addCurvePoint = async () => {
    if (!curveForm.day) return notify('请选择日期', true)
    try {
      await handleErr(api('PUT', `/buildings/${building.id}/generation-curve`,
        { ...curveForm, inverter_id: detail.inverter_id }), notify)
      notify('日曲线已录入')
      setCurveForm({ ...curveForm, inverter_id: detail.inverter_id })
      load()
    } catch { /* toast */ }
  }

  const handleRecheck = async (rc, status) => {
    const result = status === 'done' ? window.prompt('复查结果说明', '同型号设备复查正常') : ''
    if (result === null) return
    try {
      await handleErr(api('POST', `/rechecks/${rc.id}`, { status, result: result || '' }), notify)
      notify(status === 'done' ? '复查已完成' : '已忽略')
      load()
    } catch { /* toast */ }
  }

  const eventCurve = useMemo(() => {
    if (!detail) return []
    return curve
      .filter((p) => p.inverter_id === detail.inverter_id)
      .sort((a, b) => a.day.localeCompare(b.day))
  }, [curve, detail])

  return (
    <>
      {openRechecks.length > 0 && (
        <div className="card" style={{ borderLeft: '3px solid #b45309' }}>
          <h2>⚠️ 同型号设备复查提醒（{openRechecks.length}）</h2>
          <table>
            <thead><tr><th>设备</th><th>型号</th><th>提醒原因</th><th>状态</th>{isManager && <th>操作</th>}</tr></thead>
            <tbody>
              {openRechecks.map((rc) => (
                <tr key={rc.id}>
                  <td>{rc.inverter_name}</td>
                  <td>{rc.model}</td>
                  <td className="small">{rc.reason}</td>
                  <td><Badge value={rc.status} /></td>
                  {isManager && (
                    <td>
                      <div className="btn-row">
                        <button className="sm primary" onClick={() => handleRecheck(rc, 'done')}>复查完成</button>
                        <button className="sm" onClick={() => handleRecheck(rc, 'dismissed')}>忽略</button>
                      </div>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="card">
        <div className="card-title">
          <h2>设备档案与保修条款</h2>
        </div>
        <table>
          <thead>
            <tr><th>设备</th><th>型号</th><th>供应商</th><th>装机</th><th>保修期至</th><th>状态</th>{isManager && <th>操作</th>}</tr>
          </thead>
          <tbody>
            {inverters.map((iv) => (
              <tr key={iv.id}>
                <td>{iv.name}</td>
                <td>{iv.model || '—'}</td>
                <td>{iv.supplier || '—'}</td>
                <td className="small">{fmtNum(iv.capacity_kw, 1)}kW · {Number(iv.capacity_ratio).toFixed(1)}%</td>
                <td>{iv.warranty_until || <span className="muted">未登记</span>}</td>
                <td><Badge value={iv.status} /></td>
                {isManager && <td><button className="sm" onClick={() => saveWarranty(iv)}>型号/保修</button></td>}
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div className="card">
        <div className="card-title">
          <h2>设备绑定组件（停机损失按绑定份额分摊）</h2>
          {isManager && <button className="sm" onClick={autoBind}>按认购自动绑定</button>}
        </div>
        {inverters.map((iv) => (
          <div key={iv.id} className="muted-box" style={{ marginBottom: 10 }}>
            <div className="card-title" style={{ marginBottom: 4 }}>
              <h3 style={{ margin: 0 }}>{iv.name}（装机占比 {Number(iv.capacity_ratio).toFixed(1)}%）</h3>
              {isManager && (
                <button className="sm" onClick={() => editBinding(iv.id, null)}>绑定公共组件</button>
              )}
            </div>
            <table>
              <tbody>
                {(bindingsByInv[iv.id] || []).map((b) => (
                  <tr key={b.id}>
                    <td>{b.name}{b.user_id === 0 && <span className="small muted">（公共部分）</span>}</td>
                    <td className="num">{b.panel_count} 块</td>
                    <td className="num">{Number(b.ratio).toFixed(2)}%</td>
                    {isManager && (
                      <td className="num">
                        <button className="sm" onClick={() => editBinding(iv.id, b)}>调整</button>
                      </td>
                    )}
                  </tr>
                ))}
                {(!bindingsByInv[iv.id] || bindingsByInv[iv.id].length === 0) && (
                  <tr><td className="muted small">尚未绑定组件</td></tr>
                )}
              </tbody>
            </table>
          </div>
        ))}
      </div>

      {isManager && (
        <div className="card">
          <h2>登记逆变器停机</h2>
          <p className="muted small">
            停机后其绑定组件发电为零。登记停机时间、到场时间与修复时间，系统按运维响应 SLA（小时）判断维修延迟，
            再依据保修条款与通道占用情况分摊住户收益损失。
          </p>
          <div className="form-row">
            <label className="field">
              <span>逆变器</span>
              <select value={newOutage.inverter_id} onChange={(e) => setNewOutage({ ...newOutage, inverter_id: Number(e.target.value) })}>
                <option value={0}>请选择…</option>
                {inverters.map((iv) => <option key={iv.id} value={iv.id}>{iv.name}</option>)}
              </select>
            </label>
            <label className="field">
              <span>停机时间</span>
              <input type="datetime-local" value={newOutage.fault_at} onChange={(e) => setNewOutage({ ...newOutage, fault_at: e.target.value })} />
            </label>
            <label className="field">
              <span>响应 SLA（小时）</span>
              <input type="number" value={newOutage.response_sla_hours} onChange={(e) => setNewOutage({ ...newOutage, response_sla_hours: Number(e.target.value) })} />
            </label>
            <label className="field" style={{ flex: 2 }}>
              <span>故障说明</span>
              <input value={newOutage.title} onChange={(e) => setNewOutage({ ...newOutage, title: e.target.value })} placeholder="如：逆变模块过热保护停机" />
            </label>
            <button className="primary" onClick={createOutage}>登记停机</button>
          </div>
        </div>
      )}

      <div className="card">
        <h2>停机故障与影响分摊</h2>
        <table>
          <thead>
            <tr>
              <th>设备/事件</th><th>停机时段</th><th>责任</th><th className="num">损失电量</th><th className="num">损失金额</th>
              <th>赔付</th><th>曲线复核</th><th>状态</th><th></th>
            </tr>
          </thead>
          <tbody>
            {outages.map((o) => (
              <tr key={o.id}>
                <td>
                  <b>{o.inverter_name}</b>
                  <div className="small muted">{o.title}</div>
                  <div className="small muted">{o.model} · {o.supplier}{o.under_warranty ? ' · 保修期内' : ''}</div>
                </td>
                <td className="small">{dtLocal(o.fault_at)}<br />→ {dtLocal(o.resolved_at)}</td>
                <td><Badge value={o.cause_type === 'pending' ? 'pending' : o.cause_type} /><div className="small muted">{CAUSE_LABELS[o.cause_type]}</div></td>
                <td className="num">{fmtNum(o.lost_kwh_total)} kWh</td>
                <td className="num">¥{fmtMoney(o.loss_amount_total)}</td>
                <td className="small">
                  供应商 ¥{fmtMoney(o.supplier_paid_amount)}
                  <br />物业 ¥{fmtMoney(o.property_paid_amount)}
                  {o.posted_to_ledger && <div className="small" style={{ color: 'var(--primary-dark)' }}>已入账</div>}
                </td>
                <td><Badge value={o.cover_status} /></td>
                <td><Badge value={o.status} /></td>
                <td><button className="sm" onClick={() => { setSelected(o.id); api('GET', `/outages/${o.id}`).then(setDetail) }}>详情</button></td>
              </tr>
            ))}
          </tbody>
        </table>
        {outages.length === 0 && <div className="list-empty">暂无停机故障记录</div>}
      </div>

      {detail && (
        <EventDetail
          key={detail.id}
          detail={detail}
          isManager={isManager}
          assessForm={assessForm}
          setAssessForm={setAssessForm}
          assess={assess}
          setTimeline={setTimeline}
          postComp={postComp}
          review={review}
          eventCurve={eventCurve}
          curveForm={curveForm}
          setCurveForm={setCurveForm}
          addCurvePoint={addCurvePoint}
          onClose={() => { setSelected(null); setDetail(null) }}
        />
      )}
    </>
  )
}

function EventDetail({
  detail, isManager, assessForm, setAssessForm, assess, setTimeline, postComp,
  review, eventCurve, curveForm, setCurveForm, addCurvePoint, onClose,
}) {
  const locked = detail.posted_to_ledger
  const totalSupplierDue = detail.allocations.reduce((a, x) => a + Number(x.supplier_due), 0)
  return (
    <div className="card" style={{ borderLeft: '3px solid var(--primary)' }}>
      <div className="card-title">
        <h2>事件详情：{detail.inverter_name} · {detail.title}</h2>
        <div className="btn-row">
          <Badge value={detail.status} />
          <button className="sm" onClick={onClose}>关闭详情</button>
        </div>
      </div>

      <dl className="kv">
        <dt>停机/到场/修复</dt>
        <dd>{dtLocal(detail.fault_at)} ／ {dtLocal(detail.responded_at)} ／ {dtLocal(detail.resolved_at)}</dd>
        <dt>响应 SLA</dt>
        <dd>{detail.response_sla_hours} 小时</dd>
        <dt>保修条款</dt>
        <dd>{detail.warranty_note || '—'}</dd>
        <dt>通道占用</dt>
        <dd>{detail.channel_blocked_at ? `${dtLocal(detail.channel_blocked_at)} → ${dtLocal(detail.channel_freed_at)}` : '—'}</dd>
      </dl>

      {isManager && !detail.resolved_at && (
        <div className="btn-row" style={{ margin: '10px 0' }}>
          <button className="sm" onClick={() => {
            const t = window.prompt('运维到场时间（YYYY-MM-DD HH:MM）', '')
            if (t) setTimeline({ responded_at: t }, '到场时间已登记')
          }}>登记到场</button>
          <button className="sm primary" onClick={() => {
            const t = window.prompt('修复完成时间（YYYY-MM-DD HH:MM）', '')
            if (t) setTimeline({ resolved_at: t }, '修复时间已登记，设备恢复正常')
          }}>登记修复完成</button>
        </div>
      )}

      {isManager && !locked && (
        <div className="muted-box" style={{ marginBottom: 12 }}>
          <h3>责任判定与损失测算</h3>
          <div className="form-row">
            <label className="field">
              <span>故障原因</span>
              <select value={assessForm.cause_type} onChange={(e) => setAssessForm({ ...assessForm, cause_type: e.target.value })}>
                <option value="pending">待判定（损失挂账）</option>
                <option value="quality">设备质量（保修赔付）</option>
                <option value="channel">维护通道占用延迟（物业责任）</option>
                <option value="mixed">质量 + 通道延迟（混合）</option>
              </select>
            </label>
            <label className="field">
              <span>正常日发电基线 kWh</span>
              <input type="number" value={assessForm.baseline_kwh_per_day} onChange={(e) => setAssessForm({ ...assessForm, baseline_kwh_per_day: Number(e.target.value) })} />
            </label>
            <label className="field">
              <span>上网电价</span>
              <input type="number" step="0.0001" value={assessForm.grid_price} onChange={(e) => setAssessForm({ ...assessForm, grid_price: Number(e.target.value) })} />
            </label>
            <label className="field">
              <span>度电补贴</span>
              <input type="number" step="0.0001" value={assessForm.subsidy_per_kwh} onChange={(e) => setAssessForm({ ...assessForm, subsidy_per_kwh: Number(e.target.value) })} />
            </label>
          </div>
          {(assessForm.cause_type === 'channel' || assessForm.cause_type === 'mixed') && (
            <div className="form-row">
              <label className="field">
                <span>通道开始占用</span>
                <input type="datetime-local" value={assessForm.channel_blocked_at} onChange={(e) => setAssessForm({ ...assessForm, channel_blocked_at: e.target.value })} />
              </label>
              <label className="field">
                <span>通道释放/可维修</span>
                <input type="datetime-local" value={assessForm.channel_freed_at} onChange={(e) => setAssessForm({ ...assessForm, channel_freed_at: e.target.value })} />
              </label>
            </div>
          )}
          <div className="small muted" style={{ marginBottom: 8 }}>
            规则：SLA 内的停机按设备质量主张保修赔付；超过 SLA 且由屋顶维护通道占用导致的延迟段计入物业责任。
            保修期以设备档案保修截止日自动判定，可在测算结果中体现。
          </div>
          <button className="primary" onClick={assess}>重新测算分摊</button>
        </div>
      )}

      <h3>住户收益损失与补偿分摊</h3>
      <table>
        <thead>
          <tr>
            <th>受影响方</th><th className="num">绑定占比</th><th className="num">损失电量</th>
            <th className="num">质量段损失</th><th className="num">通道延迟损失</th>
            <th className="num">供应商赔付</th><th className="num">物业赔付</th><th>状态/说明</th>
          </tr>
        </thead>
        <tbody>
          {detail.allocations.map((a) => (
            <tr key={a.id}>
              <td>{a.name}</td>
              <td className="num">{Number(a.ratio).toFixed(1)}%（{a.panel_count} 块）</td>
              <td className="num">{fmtNum(a.lost_kwh)} kWh</td>
              <td className="num">¥{fmtMoney(a.loss_quality)}</td>
              <td className="num">¥{fmtMoney(a.loss_channel)}</td>
              <td className="num">¥{fmtMoney(a.supplier_paid)}<span className="small muted"> / 应 {fmtMoney(a.supplier_due)}</span></td>
              <td className="num">¥{fmtMoney(a.property_paid)}</td>
              <td className="small">
                {a.settled ? <Badge value="resolved" /> : <Badge value="pending" />}
                {a.property_note && <div className="muted">{a.property_note}</div>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="small muted" style={{ margin: '8px 0' }}>
        设备质量赔付进入住户收益补偿；公共组件部分与物业（维护通道占用延迟）赔付款进入楼栋公共账并附说明，
        再由公共账把住户部分赔付到户。
      </div>
      {isManager && (
        <div className="btn-row">
          <button className="primary" disabled={locked || totalSupplierDue + detail.property_paid_amount <= 0} onClick={postComp}>
            {locked ? '赔付已入账' : '登记赔付并入账'}
          </button>
          {locked && <span className="small muted">入账后测算与时间轴已锁定</span>}
        </div>
      )}

      <div style={{ height: 14 }} />
      <div className="card-title"><h3>修复前后发电曲线对比</h3></div>
      <CurveBars points={eventCurve} />
      <dl className="kv" style={{ marginTop: 10 }}>
        <dt>故障前日均</dt><dd>{fmtNum(detail.recovery_baseline_avg, 2)} kWh</dd>
        <dt>修复后日均</dt><dd>{fmtNum(detail.recovery_after_avg, 2)} kWh</dd>
        <dt>恢复率</dt><dd>{Number(detail.recovery_rate || 0).toFixed(1)}%</dd>
        <dt>复核结论</dt>
        <dd>
          <Badge value={detail.cover_status} /> {COVER_LABELS[detail.cover_status]}
          {detail.review_note && <div className="small muted">{detail.review_note}</div>}
        </dd>
      </dl>
      {isManager && (
        <>
          <div className="form-row" style={{ marginTop: 8 }}>
            <label className="field">
              <span>日期</span>
              <input type="date" value={curveForm.day} onChange={(e) => setCurveForm({ ...curveForm, day: e.target.value, inverter_id: detail.inverter_id })} />
            </label>
            <label className="field">
              <span>日发电量 kWh</span>
              <input type="number" value={curveForm.kwh} onChange={(e) => setCurveForm({ ...curveForm, kwh: Number(e.target.value), inverter_id: detail.inverter_id })} />
            </label>
            <label className="field">
              <span>阶段标记</span>
              <select value={curveForm.phase} onChange={(e) => setCurveForm({ ...curveForm, phase: e.target.value, inverter_id: detail.inverter_id })}>
                <option value="normal">故障前正常</option>
                <option value="outage">停机</option>
                <option value="recovery">修复后</option>
              </select>
            </label>
            <button onClick={addCurvePoint}>录入日曲线</button>
          </div>
          <div className="btn-row" style={{ marginTop: 8 }}>
            <button className="primary" onClick={review} disabled={!detail.resolved_at}>
              执行修复后曲线复核
            </button>
            <span className="small muted">复核取故障前/修复后各至多 14 天日曲线，少于 3 个修复后样本将标记"待复核"</span>
          </div>
        </>
      )}

      {detail.rechecks?.length > 0 && (
        <>
          <h3 style={{ marginTop: 14 }}>同型号设备复查提醒</h3>
          <table>
            <thead><tr><th>设备</th><th>型号</th><th>原因</th><th>状态</th><th>结果</th></tr></thead>
            <tbody>
              {detail.rechecks.map((rc) => (
                <tr key={rc.id}>
                  <td>{rc.inverter_name}</td><td>{rc.model}</td>
                  <td className="small">{rc.reason}</td>
                  <td><Badge value={rc.status} /></td>
                  <td className="small">{rc.result || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </div>
  )
}
