import React, { useEffect, useState } from 'react'
import { api, fmtMoney, fmtNum } from '../../lib/api'
import { Badge, handleErr, useToast } from '../ui'

export default function CommunityTab({ user, building }) {
  const { notify } = useToast()
  const isManager = user.role === 'admin' || user.role === 'property'

  const [ann, setAnn] = useState([])
  const [annForm, setAnnForm] = useState({ title: '', content: '', pinned: false })
  const [polls, setPolls] = useState([])
  const [pollForm, setPollForm] = useState({ title: '', content: '' })
  const [reports, setReports] = useState([])
  const [report, setReport] = useState(null)
  const [year, setYear] = useState(2026)

  const load = async () => {
    const [a, p, r] = await Promise.all([
      api('GET', `/buildings/${building.id}/announcements`),
      api('GET', `/buildings/${building.id}/polls`),
      api('GET', `/buildings/${building.id}/annual-reports`),
    ])
    setAnn(a.items || []); setPolls(p.items || []); setReports(r.items || [])
  }
  useEffect(() => { load() }, [building.id])

  const postAnn = async () => {
    if (!annForm.title) return notify('公告标题必填', true)
    try {
      await handleErr(api('POST', `/buildings/${building.id}/announcements`, annForm), notify)
      notify('公告已发布')
      setAnnForm({ title: '', content: '', pinned: false })
      load()
    } catch { /* toast */ }
  }
  const delAnn = async (id) => {
    if (!window.confirm('删除该公告？')) return
    await api('DELETE', `/announcements/${id}`).then(load).catch((e) => notify(e.message, true))
  }
  const postPoll = async () => {
    if (!pollForm.title) return notify('异议议题必填', true)
    try {
      await handleErr(api('POST', `/buildings/${building.id}/polls`, pollForm), notify)
      notify('异议投票已发起')
      setPollForm({ title: '', content: '' })
      load()
    } catch { /* toast */ }
  }
  const votePoll = async (id, support) => {
    await api('POST', `/polls/${id}/vote`, { support }).then(load).catch((e) => notify(e.message, true))
  }
  const closePoll = async (id) => {
    const r = await api('POST', `/polls/${id}/close`).catch((e) => { notify(e.message, true); return null })
    if (r) { notify(r.result === 'approved' ? '异议成立' : '异议未通过'); load() }
  }
  const genReport = async () => {
    try {
      const r = await handleErr(api('POST', `/buildings/${building.id}/annual-reports`, { year }), notify)
      notify('年度报告已生成')
      setReport(r); load()
    } catch { /* toast */ }
  }
  const openReport = async (id) => {
    const r = await api('GET', `/annual-reports/${id}`)
    setReport(r)
  }
  const publish = async (id) => {
    await api('POST', `/annual-reports/${id}/publish`).then(() => { notify('报告已向全楼发布'); load(); openReport(id) })
      .catch((e) => notify(e.message, true))
  }

  return (
    <>
      {/* 公告 */}
      <div className="card">
        <h2>楼栋公告</h2>
        {isManager && (
          <div style={{ marginBottom: 14 }}>
            <label className="field"><span>标题</span><input value={annForm.title} onChange={(e) => setAnnForm({ ...annForm, title: e.target.value })} /></label>
            <label className="field"><span>内容</span><textarea value={annForm.content} onChange={(e) => setAnnForm({ ...annForm, content: e.target.value })} /></label>
            <div className="btn-row">
              <label className="small" style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                <input type="checkbox" style={{ width: 'auto' }} checked={annForm.pinned} onChange={(e) => setAnnForm({ ...annForm, pinned: e.target.checked })} /> 置顶
              </label>
              <button className="primary" onClick={postAnn}>发布公告</button>
            </div>
          </div>
        )}
        {ann.map((a) => (
          <div key={a.id} className="muted-box" style={{ marginBottom: 10 }}>
            <div className="card-title" style={{ marginBottom: 4 }}>
              <h3 style={{ margin: 0 }}>{a.pinned && '📌 '}{a.title}</h3>
              {isManager && <button className="sm danger" onClick={() => delAnn(a.id)}>删除</button>}
            </div>
            <div style={{ whiteSpace: 'pre-wrap' }}>{a.content}</div>
            <div className="small muted" style={{ marginTop: 6 }}>{a.author} · {new Date(a.created_at).toLocaleString('zh-CN')}</div>
          </div>
        ))}
        {ann.length === 0 && <div className="list-empty">暂无公告</div>}
      </div>

      {/* 异议投票 */}
      <div className="card">
        <h2>住户异议投票</h2>
        <p className="muted small">对收益测算、施工、维修、公共支出等有异议的住户可发起议题，住户实名投票；同意多于反对即异议成立，物业须回应处理。</p>
        <div style={{ marginBottom: 14 }}>
          <label className="field"><span>异议议题 *</span><input value={pollForm.title} onChange={(e) => setPollForm({ ...pollForm, title: e.target.value })} /></label>
          <label className="field"><span>具体诉求</span><textarea value={pollForm.content} onChange={(e) => setPollForm({ ...pollForm, content: e.target.value })} /></label>
          <button className="primary" onClick={postPoll}>发起异议投票</button>
        </div>
        {polls.map((p) => (
          <div key={p.id} className="muted-box" style={{ marginBottom: 10 }}>
            <div className="card-title" style={{ marginBottom: 4 }}>
              <h3 style={{ margin: 0 }}>{p.title}</h3>
              {p.status === 'open' ? <Badge value="open" /> : <Badge value={p.result} />}
            </div>
            {p.content && <div className="small" style={{ marginBottom: 6 }}>{p.content}</div>}
            <div className="small muted" style={{ marginBottom: 6 }}>发起人：{p.creator} · 支持 {p.agree} / 反对 {p.disagree}</div>
            <div className="btn-row">
              {p.status === 'open' && (
                <>
                  <button className="sm primary" onClick={() => votePoll(p.id, true)}>支持异议</button>
                  <button className="sm danger" onClick={() => votePoll(p.id, false)}>不支持</button>
                  <button className="sm" onClick={() => closePoll(p.id)}>结束投票</button>
                </>
              )}
              {p.status === 'closed' && (
                <span className="small muted">结论：{p.result === 'approved' ? '✅ 异议成立，需物业回应处理' : '❌ 异议未通过'}</span>
              )}
            </div>
          </div>
        ))}
        {polls.length === 0 && <div className="list-empty">暂无异议投票</div>}
      </div>

      {/* 年度报告 */}
      <div className="card">
        <div className="card-title">
          <h2>年度收益报告</h2>
          {isManager && (
            <div className="btn-row">
              <input type="number" style={{ width: 100 }} value={year} onChange={(e) => setYear(Number(e.target.value))} />
              <button className="primary" onClick={genReport}>生成/重算年报</button>
            </div>
          )}
        </div>
        <table>
          <thead><tr><th>年度</th><th>状态</th><th>生成时间</th><th></th></tr></thead>
          <tbody>
            {reports.map((r) => (
              <tr key={r.id}>
                <td>{r.year} 年</td>
                <td>{r.published ? <Badge value="approved" /> : <Badge value="pending" />}<span className="small" style={{ marginLeft: 4 }}>{r.published ? '已发布' : '草稿'}</span></td>
                <td className="small">{new Date(r.created_at).toLocaleString('zh-CN')}</td>
                <td><button className="sm" onClick={() => openReport(r.id)}>查看</button></td>
              </tr>
            ))}
          </tbody>
        </table>
        {reports.length === 0 && <div className="list-empty">暂无年度报告</div>}

        {report && (
          <div className="muted-box" style={{ marginTop: 14 }}>
            <div className="card-title">
              <h3>{report.year} 年度收益报告</h3>
              <div className="btn-row">
                {isManager && !report.published && <button className="sm primary" onClick={() => publish(report.id)}>发布给全楼住户</button>}
                <button className="sm" onClick={() => setReport(null)}>关闭</button>
              </div>
            </div>
            <AnnualSummary s={report.summary} />
          </div>
        )}
      </div>
    </>
  )
}

function AnnualSummary({ s }) {
  if (!s) return null
  return (
    <div>
      <div className="grid grid-4">
        <div><div className="small muted">结算月份</div><b>{s.finalized_months} 个</b></div>
        <div><div className="small muted">年发电量</div><b>{fmtNum(s.total_kwh)} kWh</b></div>
        <div><div className="small muted">碳减排</div><b>{fmtNum(s.carbon_kg)} kg CO₂</b></div>
        <div><div className="small muted">可分配净额</div><b>¥{fmtMoney(s.distributable)}</b></div>
        <div><div className="small muted">电费收入</div><b>¥{fmtMoney(s.gross_revenue)}</b></div>
        <div><div className="small muted">补贴</div><b>¥{fmtMoney(s.total_subsidy)}</b></div>
        <div><div className="small muted">运维费</div><b>¥{fmtMoney(s.om_fee)}</b></div>
        <div><div className="small muted">税费 / 维修</div><b>¥{fmtMoney(s.tax)} / ¥{fmtMoney(s.work_order_cost)}</b></div>
      </div>
      <h4 style={{ margin: '12px 0 6px' }}>住户年度分成</h4>
      <table>
        <thead><tr><th>住户</th><th className="num">归属电量 kWh</th><th className="num">年度分成（元）</th></tr></thead>
        <tbody>
          {(s.shareholder_earnings || []).map((h, i) => (
            <tr key={i}><td>{h.name}</td><td className="num">{fmtNum(h.kwh)}</td><td className="num">{fmtMoney(h.amount)}</td></tr>
          ))}
        </tbody>
      </table>
      <h4 style={{ margin: '12px 0 6px' }}>年度维修记录</h4>
      <table>
        <thead><tr><th>月份</th><th>事项</th><th>责任方</th><th className="num">费用</th><th>状态</th></tr></thead>
        <tbody>
          {(s.work_orders || []).map((w, i) => (
            <tr key={i}>
              <td>{w.period_month}</td><td>{w.title}</td><td>{w.responsibility}</td>
              <td className="num">{fmtMoney(w.cost)}</td><td>{w.status}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <h4 style={{ margin: '12px 0 6px' }}>逆变器故障收益损失与赔付</h4>
      <table>
        <thead><tr><th>月份</th><th>设备</th><th>责任</th><th className="num">损失电量</th>
          <th className="num">供应商赔付</th><th className="num">物业赔偿</th>
          <th className="num">恢复率/覆盖率</th><th>状态</th></tr></thead>
        <tbody>
          {(s.inverter_faults || []).map((f, i) => (
            <tr key={i}>
              <td>{f.period_month}</td><td>{f.inverter}</td>
              <td>{f.cause === 'quality' ? '设备质量' : f.cause === 'channel' ? '通道占用' : '混合责任'}</td>
              <td className="num">{fmtNum(f.lost_kwh)} kWh</td>
              <td className="num">¥{fmtMoney(f.supplier_compensation)}</td>
              <td className="num">¥{fmtMoney(f.property_compensation)}</td>
              <td className="num small">{Number(f.recovery_ratio).toFixed(1)}% / {Number(f.cover_ratio).toFixed(1)}%</td>
              <td>{f.paid ? <span className="badge green">已补偿</span> : <span className="badge amber">待发放</span>}</td>
            </tr>
          ))}
          {(s.inverter_faults || []).length === 0 && (
            <tr><td colSpan={8} className="list-empty">本年度无已评估故障</td></tr>
          )}
        </tbody>
      </table>
      {(Number(s.fault_supplier_compensation) || Number(s.fault_property_compensation)) ? (
        <p className="small muted" style={{ marginTop: 6 }}>
          合计：供应商质量赔付 ¥{fmtMoney(s.fault_supplier_compensation)}（进入住户收益补偿）；
          物业通道延迟赔偿 ¥{fmtMoney(s.fault_property_compensation)}（楼栋公共账说明后代发住户）。
        </p>
      ) : null}
    </div>
  )
}
