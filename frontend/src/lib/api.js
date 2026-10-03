// 极简 API 封装：JWT 存 localStorage，错误统一抛出 Error(message)
const TOKEN_KEY = 'pv_token'

export function getToken() {
  return localStorage.getItem(TOKEN_KEY) || ''
}
export function setToken(t) {
  localStorage.setItem(TOKEN_KEY, t)
}
export function clearToken() {
  localStorage.removeItem(TOKEN_KEY)
}

export async function api(method, path, body) {
  const headers = { 'Content-Type': 'application/json' }
  const token = getToken()
  if (token) headers.Authorization = 'Bearer ' + token
  const resp = await fetch('/api' + path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  const text = await resp.text()
  let data = {}
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = { raw: text }
    }
  }
  if (!resp.ok) {
    throw new Error(data.error || `请求失败 (${resp.status})`)
  }
  return data
}

export const fmtMoney = (v) =>
  Number(v || 0).toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })
export const fmtNum = (v, d = 0) =>
  Number(v || 0).toLocaleString('zh-CN', { minimumFractionDigits: d, maximumFractionDigits: d })

export const STAGES = [
  { key: 'draft', label: '立项建档' },
  { key: 'voting', label: '楼栋表决' },
  { key: 'subscribing', label: '认购缴费' },
  { key: 'construction', label: '施工排期' },
  { key: 'grid_acceptance', label: '并网验收' },
  { key: 'operating', label: '发电运营' },
]
export const stageLabel = (k) => STAGES.find((s) => s.key === k)?.label || k

export const ROLE_LABEL = { admin: '管理员', property: '物业', resident: '住户' }
export const WO_RESP = {
  undetermined: '待判定',
  property: '物业',
  installer: '安装商',
  owner: '业主',
  insurance: '保险',
}
export const CAUSE_LABELS = {
  pending: '待判定',
  quality: '设备质量（保修）',
  channel: '维护通道占用',
  mixed: '质量+通道延迟',
}
export const COVER_LABELS = {
  pending: '待复核',
  covered: '赔付覆盖损失',
  shortfall: '补偿不足',
  surplus: '赔付有结余',
}
