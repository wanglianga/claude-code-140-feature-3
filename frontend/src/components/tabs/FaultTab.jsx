import React, { useEffect, useState } from 'react'
import { api, fmtMoney, fmtNum } from '../../lib/api'
import { Badge, handleErr, useToast } from '../ui'

const CAUSE_LABEL = {
  undetermined: '待判定', quality: '设备质量', channel: '通道占用', mixed: '混合责任',
}
const CAUSE_STYLE = { quality: 'red', channel: 'amber', mixed: 'amber' }

function CauseBadge({ cause }) {
  if (!cause || cause === 'undetermined') return <span className="badge gray">待判定</span>
  return <span className={'badge ' + (CAUSE_STYLE[cause] || 'gray')}>{CAUSE_LABEL[cause]}</span>
}

const dt = (s) => (s ? new Date(s).toLocaleString('zh-CN', { hour12: false }) : '—')
const day = (s) => (s ? new Date(s).toLocaleDateString('zh-CN') : '—')

export default function FaultTab({ user, building }) {
  const { notify } = useToast()
  const isManager = user.role === 'admin' || user.role === 'property'
  const [faults, setFaults] = useState([])
  const [inverters, setInverters] = useState([])
  const [expanded, setExpanded] = useState({})
  const [form, setForm] = useState({
    inverter_id: 0, period_month: '2026-10', title: '', description: '',
    fault_at: '', baseline_daily_kwh: 70, grid_price: 0.4153,
    subsidy_per_kwh: 0.05, sla_response_hours: 24,
  })
  const [assessForm, setAssessForm] = useState({})
  const [curveForm, setCurveForm] = useState({})

  const load = async () => {
    const [f, iv] = await Promise.all([
      api('GET', `/buildings/${building.id}/faults`).catch(() => ({ items: [] })),
      api('GET', `/buildings/${building.id}/inverters`).catch(() => ({ items: [] })),
    ])
    setFaults(f.items || [])
    setInverters(iv.items || [])
    if (f.items && Object.keys(expanded).length === 0) {
      setExpanded({ [f.items[0].id]: true })
    }
  }
  useEffect(() => { load() }, [building.id])

  const act = async (fid, path, body) => {
    try {
      const r = await handleErr(api('POST', `/faults/${fid}${path}`, body || {}), notify)
      notify('操作成功')
      load()
      return r
    } catch { /* toast */ }
  }

  const create = async () => {
    if (!form.inverter_id) return notify('请选择故障逆变器', true)
    if (!form.title) return notify('请填写故障标题', true)
    try {
      await handleErr(api('POST', `/buildings/${building.id}/faults`, form), notify)
      notify('故障已登记，设备已标记停机')
      setForm({ ...form, title: '', description: '', fault_at: '' })
      load()
    } catch { /* toast */ }
  }

  const submitAssess = async (f) => {
    const a = assessForm[f.id] || {}
    if (!a.cause) return notify('请选择责任类型', true)
    const body = { cause: a.cause }
    if (a.cause === 'mixed') {
      const delay = Number(a.delay_hours)
      if (!(delay >= 0) || delay > f.downtime_hours) return notify(`通道延迟时长须介于 0 与 ${f.downtime_hours} 之间`, true)
      body.delay_hours = delay
    }
    if (a.warranty_covered !== '') body.warranty_covered = a.warranty_covered === 'true'
    if (a.note) body.warranty_note = a.note
    await act(f.id, '/assess', body)
    setAssessForm({ ...assessForm, [f.id]: {} })
  }

  const submitCurve = async (f) => {
    const c = curveForm[f.id] || {}
    const postDaily = Number(c.post_daily_kwh)
    if (!(postDaily > 0)) return notify('请填写修复后日均发电量', true)
    await act(f.id, '/compare-curve', { post_daily_kwh: postDaily, note: c.note || '' })
    setCurveForm({ ...curveForm, [f.id]: {} })
  }

  const saveProfile = async (iv) => {
    const model = window.prompt('设备型号', iv.model || '')
    if (model === null) return
    const supplier = window.prompt('供应商', iv.supplier || '')
    if (supplier === null) return
    const warrantyMonths = Number(window.prompt('整机保修期（月）', String(iv.warranty_months || 0)))
    if (Number.isNaN(warrantyMonths)) return
    const installDate = window.prompt('安装日期 YYYY-MM-DD（留空不变）', (iv.install_date || '').slice(0, 10))
    if (installDate === null) return
    const body = { model, supplier, warranty_months: warrantyMonths }
    if (installDate) body.install_date = installDate
    try {
      await handleErr(api('PATCH', `/inverters/${iv.id}`, body), notify)
      notify('设备档案已更新')
      load()
    } catch { /* toast */ }
  }

  return (
    <>
      <div className="card">
        <h2>逆变器故障影响分摊</h2>
        <p className="muted small">
          逆变器停机后其绑定组件发电为零。平台依据<b>设备绑定组件份额、停机时长、运维响应时间与保修条款</b>计算受影响住户收益损失：
          <b>设备质量</b>故障在保修期内由供应商赔付并直接进入住户收益补偿（未认购公共组件部分入公共账户）；
          <b>屋顶维护通道被占用</b>导致维修延迟的，物业赔偿先入楼栋公共账说明，再按组件比例代发给住户。
          修复后对比修复前后发电曲线确认补偿是否覆盖真实损失，并提醒运维复查同型号设备。
        </p>
      </div>

      <div className="card">
        <div className="card-title">
          <h2 style={{ margin: 0 }}>设备档案（型号 / 供应商 / 保修条款）</h2>
        </div>
        <table>
          <thead><tr><th>设备</th><th>型号</th><th>供应商</th><th className="num">保修期(月)</th><th>安装日期</th><th>状态</th>{isManager && <th>操作</th>}</tr></thead>
          <tbody>
            {inverters.map((iv) => (
              <tr key={iv.id}>
                <td>{iv.name}</td>
                <td>{iv.model || <span className="muted">未登记</span>}</td>
                <td className="small">{iv.supplier || '—'}</td>
                <td className="num">{iv.warranty_months || '—'}</td>
                <td className="small">{iv.install_date ? day(iv.install_date) : '—'}</td>
                <td><Badge value={iv.status} /></td>
                {isManager && <td><button className="sm" onClick={() => saveProfile(iv)}>档案</button></td>}
              </tr>
            ))}
          </tbody>
        </table>
        {inverters.length === 0 && <div className="list-empty">并网验收前由物业登记逆变器信息</div>}
      </div>

      {isManager && (
        <div className="card">
          <h2>登记停机故障</h2>
          <div className="form-row">
            <label className="field">
              <span>逆变器 *</span>
              <select value={form.inverter_id} onChange={(e) => setForm({ ...form, inverter_id: Number(e.target.value) })}>
                <option value={0}>请选择…</option>
                {inverters.map((iv) => <option key={iv.id} value={iv.id}>{iv.name}{iv.model ? `（${iv.model}）` : ''}</option>)}
              </select>
            </label>
            <label className="field">
              <span>归属月份</span>
              <input value={form.period_month} onChange={(e) => setForm({ ...form, period_month: e.target.value })} placeholder="YYYY-MM" />
            </label>
            <label className="field">
              <span>停机时间</span>
              <input value={form.fault_at} onChange={(e) => setForm({ ...form, fault_at: e.target.value })} placeholder="留空=现在，或 RFC3339" />
            </label>
            <label className="field">
              <span>SLA 响应(小时)</span>
              <input type="number" value={form.sla_response_hours} onChange={(e) => setForm({ ...form, sla_response_hours: Number(e.target.value) })} />
            </label>
          </div>
          <div className="form-row">
            <label className="field">
              <span>故障前日均发电 kWh</span>
              <input type="number" value={form.baseline_daily_kwh} onChange={(e) => setForm({ ...form, baseline_daily_kwh: Number(e.target.value) })} />
            </label>
            <label className="field">
              <span>上网电价</span>
              <input type="number" step="0.0001" value={form.grid_price} onChange={(e) => setForm({ ...form, grid_price: Number(e.target.value) })} />
            </label>
            <label className="field">
              <span>度电补贴</span>
              <input type="number" step="0.0001" value={form.subsidy_per_kwh} onChange={(e) => setForm({ ...form, subsidy_per_kwh: Number(e.target.value) })} />
            </label>
          </div>
          <div className="form-row">
            <label className="field" style={{ flex: 2 }}><span>故障标题 *</span><input value={form.title} onChange={(e) => setForm({ ...form, title: e.target.value })} placeholder="如：逆变模块过热保护停机" /></label>
            <button className="primary" onClick={create}>登记故障</button>
          </div>
          <label className="field"><span>故障描述</span><textarea value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} /></label>
        </div>
      )}

      <div className="card">
        <h2>故障台账与收益损失分摊</h2>
        {faults.map((f) => {
          const a = assessForm[f.id] || {}
          const c = curveForm[f.id] || {}
          return (
            <div key={f.id} className="muted-box" style={{ marginBottom: 12 }}>
              <div className="card-title" style={{ cursor: 'pointer' }} onClick={() => setExpanded({ ...expanded, [f.id]: !expanded[f.id] })}>
                <div>
                  <h3 style={{ margin: 0 }}>
                    {f.period_month} · {f.inverter_name} · {f.title}
                  </h3>
                  <div className="small muted" style={{ marginTop: 4 }}>
                    停机 {fmtNum(f.downtime_hours, 1)}h · 响应 {fmtNum(f.response_hours, 1)}h / SLA {fmtNum(f.sla_response_hours, 0)}h
                    {' '}· <span className={f.sla_met ? '' : ''}>{f.sla_met ? '响应达标' : '响应超时'}</span>
                  </div>
                </div>
                <div className="btn-row" onClick={(e) => e.stopPropagation()}>
                  <Badge value={f.status === 'open' ? 'open' : f.status === 'responded' ? 'processing' : 'resolved'} />
                  <CauseBadge cause={f.cause} />
                  {f.assessed && <Badge value={f.compare_status === 'covered' ? 'covered' : f.compare_status === 'insufficient' ? 'insufficient' : 'pending'} />}
                  {f.paid ? <span className="badge green">已补偿</span> : f.assessed ? <span className="badge amber">待发放</span> : null}
                  {f.review_needed && !f.review_done && <span className="badge amber">待复查同型号</span>}
                  {f.review_done && <span className="badge green">已复查</span>}
                </div>
              </div>

              {isManager && f.status !== 'resolved' && (
                <div className="btn-row" style={{ margin: '8px 0' }}>
                  {f.status === 'open' && <button className="sm" onClick={() => act(f.id, '/respond')}>运维到场响应</button>}
                  <button className="sm primary" onClick={() => act(f.id, '/resolve')}>维修完成（恢复设备）</button>
                </div>
              )}

              {expanded[f.id] && (
                <>
                  <table style={{ marginTop: 8 }}>
                    <tbody>
                      <tr><td className="small muted" style={{ width: 120 }}>停机时间线</td>
                        <td className="small">停机 {dt(f.fault_at)} ｜ 到场 {dt(f.responded_at)} ｜ 修复 {dt(f.resolved_at)}</td></tr>
                      {f.description && <tr><td className="small muted">故障描述</td><td className="small">{f.description}</td></tr>}
                      <tr><td className="small muted">设备/保修</td>
                        <td className="small">{f.model || '未登记型号'} · {f.supplier || '供应商未登记'} · 保修 {f.warranty_months || 0} 个月
                          （安装于 {day(f.install_date)}）{f.assessed && <> · {f.warranty_covered ? <b className="green-text">保修范围内</b> : <b>不在保修范围</b>}</>}</td></tr>
                      {f.warranty_note && <tr><td className="small muted">保修条款说明</td><td className="small">{f.warranty_note}</td></tr>}
                    </tbody>
                  </table>

                  {f.assessed && (
                    <>
                      <div className="grid grid-3" style={{ margin: '8px 0' }}>
                        <div className="card stat"><div className="label">损失电量</div><div className="value">{fmtNum(f.lost_kwh)} kWh</div>
                          <div className="sub">质量时段 {fmtNum(f.quality_lost_kwh)} · 通道延迟 {fmtNum(f.channel_lost_kwh)}（基准日均 {fmtNum(f.baseline_daily_kwh)} kWh）</div></div>
                        <div className="card stat accent"><div className="label">供应商赔付（入住户收益）</div><div className="value">¥{fmtMoney(f.supplier_compensation)}</div>
                          <div className="sub">设备质量时段，保修赔付</div></div>
                        <div className="card stat accent"><div className="label">物业赔偿（入楼栋公共账）</div><div className="value">¥{fmtMoney(f.property_compensation)}</div>
                          <div className="sub">通道占用致维修延迟 {fmtNum(f.delay_hours, 1)}h</div></div>
                      </div>

                      <table>
                        <thead><tr><th>受影响方</th><th className="num">组件比例</th><th className="num">损失电量</th><th className="num">真实损失</th><th className="num">供应商赔付</th><th className="num">物业赔偿</th><th className="num">合计补偿</th></tr></thead>
                        <tbody>
                          {(f.items || []).map((it) => (
                            <tr key={it.id}>
                              <td>{it.name}{it.party === 'public' && <span className="badge gray" style={{ marginLeft: 6 }}>公共</span>}</td>
                              <td className="num">{Number(it.ratio).toFixed(2)}%</td>
                              <td className="num">{fmtNum(it.lost_kwh)}</td>
                              <td className="num">¥{fmtMoney(it.loss_amount)}</td>
                              <td className="num">¥{fmtMoney(it.supplier_amount)}</td>
                              <td className="num">¥{fmtMoney(it.property_amount)}</td>
                              <td className="num"><b>¥{fmtMoney(it.total_compensation)}</b></td>
                            </tr>
                          ))}
                        </tbody>
                      </table>

                      {f.compare_status && (
                        <div className="muted-box" style={{ marginTop: 8 }}>
                          <b>修复前后发电曲线对比：</b>
                          修复前基准日均 <b>{fmtNum(f.pre_daily_kwh)}</b> kWh → 修复后实测 <b>{fmtNum(f.post_daily_kwh)}</b> kWh，
                          出力恢复率 <b className={f.recovery_ratio >= 95 ? 'green-text' : ''}>{Number(f.recovery_ratio).toFixed(2)}%</b>，
                          赔付覆盖率 <b>{Number(f.cover_ratio).toFixed(2)}%</b>。
                          <span className={'badge ' + (f.compare_status === 'covered' ? 'green' : 'red')} style={{ marginLeft: 8 }}>
                            {f.compare_status === 'covered' ? '补偿覆盖真实损失' : '补偿不足，需追偿/复查'}
                          </span>
                          <div className="small muted" style={{ marginTop: 4 }}>{f.compare_note}</div>
                          <div className="chart" style={{ marginTop: 8 }}>
                            <div className="col" title={`修复前 ${f.pre_daily_kwh}`}>
                              <div className="bar" style={{ height: Math.max(4, f.pre_daily_kwh / Math.max(f.pre_daily_kwh, f.post_daily_kwh) * 60) + 'px' }} />
                              <div className="lbl">修复前</div>
                            </div>
                            <div className="col" title={`修复后 ${f.post_daily_kwh}`}>
                              <div className={'bar ' + (f.recovery_ratio >= 95 ? 'subsidy' : '')} style={{ height: Math.max(4, f.post_daily_kwh / Math.max(f.pre_daily_kwh, f.post_daily_kwh) * 60) + 'px' }} />
                              <div className="lbl">修复后</div>
                            </div>
                          </div>
                        </div>
                      )}

                      {f.review_needed && (
                        <div className="muted-box" style={{ marginTop: 8 }}>
                          <b>同型号设备复查提醒：</b>
                          {f.review_done
                            ? <>已完成复查。{f.review_note}</>
                            : <>建议运维复查同型号设备：<b>{(f.same_model_names || []).join('、') || '本楼暂无其他同型号设备'}</b>。</>}
                        </div>
                      )}
                    </>
                  )}

                  {/* 物业操作区 */}
                  {isManager && f.status === 'resolved' && !f.paid && (
                    <div style={{ marginTop: 10 }}>
                      {!f.assessed && (
                        <div className="muted-box">
                          <b>责任判定与保修评估</b>
                          <div className="form-row" style={{ marginTop: 6 }}>
                            <label className="field"><span>责任类型</span>
                              <select value={a.cause || ''} onChange={(e) => setAssessForm({ ...assessForm, [f.id]: { ...a, cause: e.target.value } })}>
                                <option value="">请选择…</option>
                                <option value="quality">设备质量（供应商保修赔付）</option>
                                <option value="channel">维护通道占用延迟（物业赔偿）</option>
                                <option value="mixed">混合责任（按时长拆分）</option>
                              </select>
                            </label>
                            {a.cause === 'mixed' && (
                              <label className="field"><span>通道占用延迟(小时)</span>
                                <input type="number" value={a.delay_hours ?? ''} onChange={(e) => setAssessForm({ ...assessForm, [f.id]: { ...a, delay_hours: e.target.value } })} placeholder={`总停机 ${f.downtime_hours}h`} />
                              </label>
                            )}
                            <label className="field"><span>保修认定（可留空按档案自动）</span>
                              <select value={a.warranty_covered ?? ''} onChange={(e) => setAssessForm({ ...assessForm, [f.id]: { ...a, warranty_covered: e.target.value } })}>
                                <option value="">按设备档案自动认定</option>
                                <option value="true">保修范围内</option>
                                <option value="false">不在保修范围</option>
                              </select>
                            </label>
                            <button className="primary" onClick={() => submitAssess(f)}>评估损失与赔付</button>
                          </div>
                        </div>
                      )}

                      {f.assessed && (
                        <div className="btn-row">
                          {f.compare_status !== 'covered' && (
                            <span className="muted-box" style={{ display: 'flex', gap: 8, alignItems: 'flex-end', padding: 8 }}>
                              <label className="field" style={{ margin: 0 }}><span>修复后观测期日均 kWh</span>
                                <input type="number" style={{ width: 160 }} value={c.post_daily_kwh ?? ''} onChange={(e) => setCurveForm({ ...curveForm, [f.id]: { ...c, post_daily_kwh: e.target.value } })} /></label>
                              <button className="primary sm" onClick={() => submitCurve(f)}>对比修复前后曲线</button>
                            </span>
                          )}
                          {f.compare_status === 'covered' && (
                            <button className="primary" onClick={() => act(f.id, '/pay')}>发放补偿并入账</button>
                          )}
                          {f.review_needed && !f.review_done && (
                            <button onClick={() => act(f.id, '/review', { note: '运维已完成同型号设备巡检，未发现同类缺陷' })}>同型号设备复查完成</button>
                          )}
                        </div>
                      )}
                    </div>
                  )}
                </>
              )}
            </div>
          )
        })}
        {faults.length === 0 && <div className="list-empty">暂无逆变器故障记录</div>}
      </div>
    </>
  )
}
