// Package outage 实现逆变器停机故障的影响测算与责任分摊（纯计算，无数据库依赖，便于单元测试）。
//
// 业务口径：
//   - 一台逆变器停机后，其绑定组件发电为零；损失电量 = 停机时长 × 正常时日发电基线。
//   - 损失按"设备绑定组件"在该逆变器内的份额分摊到住户与公共部分。
//   - 运维响应 SLA：自停机起 response_sla_hours 内到场属于正常响应窗口。
//   - 设备质量（保修期内）时段损失由供应商赔付：住户部分赔付进入住户收益补偿，
//     公共组件部分进入公共账户。
//   - 屋顶维护通道被物业占用、且占用导致超过 SLA 的维修延迟，该时段损失属物业责任，
//     由物业赔付并在楼栋公共账说明：住户部分经公共账赔付给住户，公共部分留存公共账。
//   - 责任未判定的时段挂账（pending），不生成赔付，待判定后重新测算。
package outage

import (
	"math"
	"sort"
	"time"
)

// Cause 责任类型
const (
	CausePending = "pending" // 待判定
	CauseQuality = "quality" // 设备质量（保修）
	CauseChannel = "channel" // 维护通道占用导致维修延迟
	CauseMixed   = "mixed"   // 质量 + 通道占用兼有
)

// Binding 设备绑定组件归属。Ratio 为占整楼组件比例（%），同一逆变器下的绑定之和≈该逆变器装机占比。
type Binding struct {
	UserID     *int64  `json:"user_id"`
	Name       string  `json:"name"`
	Ratio      float64 `json:"ratio"`
	PanelCount int     `json:"panel_count"`
}

// Segment 停机时间轴上的一个责任时段
type Segment struct {
	Kind  string    `json:"kind"` // quality / channel / pending
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Hours float64   `json:"hours"`
}

// Allocation 单户（或公共部分）的损失与补偿分摊行
type Allocation struct {
	UserID       *int64  `json:"user_id"`
	Name         string  `json:"name"`
	Ratio        float64 `json:"ratio"` // 在该逆变器绑定组件中的归一化占比 %
	PanelCount   int     `json:"panel_count"`
	LostKwh      float64 `json:"lost_kwh"`
	LossQuality  float64 `json:"loss_quality"`
	LossChannel  float64 `json:"loss_channel"`
	LossPending  float64 `json:"loss_pending"`
	LossTotal    float64 `json:"loss_total"`
	SupplierDue  float64 `json:"supplier_due"`  // 供应商应赔（质量段）
	SupplierPaid float64 `json:"supplier_paid"` // 供应商实际赔到户
	PropertyDue  float64 `json:"property_due"`  // 物业应赔（通道延迟段，经公共账）
	PropertyPaid float64 `json:"property_paid"`
}

// Input 停机影响测算输入
type Input struct {
	FaultAt          time.Time  `json:"fault_at"`
	RespondedAt      *time.Time `json:"responded_at"`
	ResolvedAt       *time.Time `json:"resolved_at"`
	AsOf             time.Time  `json:"as_of"` // 未修复时按此时刻截断
	ResponseSLAHours float64    `json:"response_sla_hours"`

	CauseType        string     `json:"cause_type"`
	ChannelBlockedAt *time.Time `json:"channel_blocked_at"`
	ChannelFreedAt   *time.Time `json:"channel_freed_at"`
	UnderWarranty    bool       `json:"under_warranty"`

	BaselineKwhPerDay float64 `json:"baseline_kwh_per_day"`
	GridPrice         float64 `json:"grid_price"`
	SubsidyRate       float64 `json:"subsidy_per_kwh"`

	// 供应商/物业实际已赔付到平台的总额（登记后按应赔比例分到各户）；0 表示默认按应赔足额
	SupplierPaidTotal float64 `json:"supplier_paid_total"`
	PropertyPaidTotal float64 `json:"property_paid_total"`

	Bindings []Binding `json:"bindings"`
}

// Assessment 测算结果
type Assessment struct {
	TotalHours    float64   `json:"total_hours"`
	ResponseHours float64   `json:"response_hours"` // 实际响应耗时（无到场时间取 0）
	WithinSLA     bool      `json:"within_sla"`
	SLADelayHours float64   `json:"sla_delay_hours"` // 超出 SLA 的响应延迟
	Segments      []Segment `json:"segments"`

	LostKwhTotal   float64 `json:"lost_kwh_total"`
	LostKwhQuality float64 `json:"lost_kwh_quality"`
	LostKwhChannel float64 `json:"lost_kwh_channel"`
	LostKwhPending float64 `json:"lost_kwh_pending"`

	RatePerKwh        float64 `json:"rate_per_kwh"` // 电价+补贴
	LossAmountTotal   float64 `json:"loss_amount_total"`
	LossAmountQuality float64 `json:"loss_amount_quality"`
	LossAmountChannel float64 `json:"loss_amount_channel"`
	LossAmountPending float64 `json:"loss_amount_pending"`

	SupplierDueTotal  float64 `json:"supplier_due_total"`
	SupplierPaidTotal float64 `json:"supplier_paid_total"`
	SupplierPayable   float64 `json:"supplier_payable"` // 仍欠付
	PropertyDueTotal  float64 `json:"property_due_total"`
	PropertyPaidTotal float64 `json:"property_paid_total"`
	PropertyPayable   float64 `json:"property_payable"`

	Allocations []Allocation `json:"allocations"`
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

func hoursBetween(a, b time.Time) float64 {
	if b.Before(a) {
		return 0
	}
	return b.Sub(a).Hours()
}

// intersect 求 [a1,a2] ∩ [b1,b2]，无交集返回 nil
func intersect(a1, a2, b1, b2 time.Time) (time.Time, time.Time, bool) {
	s := a1
	if b1.After(s) {
		s = b1
	}
	e := a2
	if b2.Before(e) {
		e = b2
	}
	if !e.After(s) {
		return time.Time{}, time.Time{}, false
	}
	return s, e, true
}

// Assess 执行停机损失测算与责任分摊。
func Assess(in Input) Assessment {
	res := Assessment{RatePerKwh: in.GridPrice + in.SubsidyRate}

	end := in.AsOf
	if in.ResolvedAt != nil {
		end = *in.ResolvedAt
	}
	if end.Before(in.FaultAt) {
		end = in.FaultAt
	}
	res.TotalHours = round2(hoursBetween(in.FaultAt, end))
	if in.RespondedAt != nil {
		res.ResponseHours = round2(hoursBetween(in.FaultAt, *in.RespondedAt))
	}
	sla := in.ResponseSLAHours
	if sla <= 0 {
		sla = 24
	}
	res.WithinSLA = in.RespondedAt == nil || res.ResponseHours <= sla
	if !res.WithinSLA {
		res.SLADelayHours = round2(res.ResponseHours - sla)
	}

	// ---- 时间轴切分 ----
	// 物业责任（通道占用）时段：停机窗口 ∩ 通道占用区间，且只计超过 SLA 响应时限的部分。
	var chStart, chEnd time.Time
	hasChannel := in.CauseType == CauseChannel || in.CauseType == CauseMixed
	if hasChannel && in.ChannelBlockedAt != nil {
		freed := end
		if in.ChannelFreedAt != nil && in.ChannelFreedAt.Before(freed) {
			freed = *in.ChannelFreedAt
		}
		slaDeadline := in.FaultAt.Add(time.Duration(sla * float64(time.Hour)))
		if s, e, ok := intersect(in.FaultAt, end, *in.ChannelBlockedAt, freed); ok {
			if s.Before(slaDeadline) {
				s = slaDeadline
			}
			if e.After(s) {
				chStart, chEnd = s, e
			}
		}
	}

	if in.CauseType == CausePending {
		res.Segments = []Segment{{Kind: CausePending, Start: in.FaultAt, End: end, Hours: res.TotalHours}}
	} else if chStart.IsZero() {
		res.Segments = []Segment{{Kind: CauseQuality, Start: in.FaultAt, End: end, Hours: res.TotalHours}}
	} else {
		// 通道时段可能把质量时段切成前后两段
		if in.FaultAt.Before(chStart) {
			s, e := in.FaultAt, chStart
			res.Segments = append(res.Segments, Segment{Kind: CauseQuality, Start: s, End: e, Hours: round2(hoursBetween(s, e))})
		}
		res.Segments = append(res.Segments, Segment{Kind: CauseChannel, Start: chStart, End: chEnd, Hours: round2(hoursBetween(chStart, chEnd))})
		if end.After(chEnd) {
			s, e := chEnd, end
			res.Segments = append(res.Segments, Segment{Kind: CauseQuality, Start: s, End: e, Hours: round2(hoursBetween(s, e))})
		}
	}

	var hQuality, hChannel, hPending float64
	for _, sg := range res.Segments {
		switch sg.Kind {
		case CauseQuality:
			hQuality += sg.Hours
		case CauseChannel:
			hChannel += sg.Hours
		case CausePending:
			hPending += sg.Hours
		}
	}
	// 按小时比例把基线日电量摊到各责任时段
	kwh := func(h float64) float64 {
		if res.TotalHours <= 0 {
			return 0
		}
		return round2(in.BaselineKwhPerDay * h / 24)
	}
	totalKwh := round2(in.BaselineKwhPerDay * res.TotalHours / 24)
	res.LostKwhQuality, res.LostKwhChannel, res.LostKwhPending = kwh(hQuality), kwh(hChannel), kwh(hPending)
	// 分段四舍五入误差归到最大时段，保证分段之和=总量
	diff := round2(totalKwh - (res.LostKwhQuality + res.LostKwhChannel + res.LostKwhPending))
	if diff != 0 {
		switch {
		case hQuality >= hChannel && hQuality >= hPending:
			res.LostKwhQuality = round2(res.LostKwhQuality + diff)
		case hChannel >= hPending:
			res.LostKwhChannel = round2(res.LostKwhChannel + diff)
		default:
			res.LostKwhPending = round2(res.LostKwhPending + diff)
		}
	}
	res.LostKwhTotal = round2(res.LostKwhQuality + res.LostKwhChannel + res.LostKwhPending)

	money := func(k float64) float64 { return round2(k * res.RatePerKwh) }
	res.LossAmountQuality = money(res.LostKwhQuality)
	res.LossAmountChannel = money(res.LostKwhChannel)
	res.LossAmountPending = money(res.LostKwhPending)
	res.LossAmountTotal = money(res.LostKwhTotal)

	// 保修期外：质量段不自动向供应商主张（转既有工单/月结机制处理），应赔为 0
	if in.UnderWarranty {
		res.SupplierDueTotal = res.LossAmountQuality
	}
	res.PropertyDueTotal = res.LossAmountChannel

	// 实际赔付款：缺省按应赔足额；传入值以应赔为上限
	supPaid := in.SupplierPaidTotal
	if supPaid <= 0 {
		supPaid = res.SupplierDueTotal
	}
	supPaid = math.Min(supPaid, res.SupplierDueTotal)
	propPaid := in.PropertyPaidTotal
	if propPaid <= 0 {
		propPaid = res.PropertyDueTotal
	}
	propPaid = math.Min(propPaid, res.PropertyDueTotal)
	res.SupplierPaidTotal = round2(supPaid)
	res.PropertyPaidTotal = round2(propPaid)
	res.SupplierPayable = round2(res.SupplierDueTotal - supPaid)
	res.PropertyPayable = round2(res.PropertyDueTotal - propPaid)

	// ---- 按绑定组件分摊到户 ----
	type norm struct {
		b     Binding
		ratio float64
	}
	var rows []norm
	var sumRatio float64
	for _, b := range in.Bindings {
		sumRatio += b.Ratio
	}
	if sumRatio <= 0 {
		// 无绑定数据时按公共部分整体挂账
		sumRatio = 1
		rows = []norm{{b: Binding{Name: "公共部分（未绑定组件）"}, ratio: 1}}
	} else {
		for _, b := range in.Bindings {
			rows = append(rows, norm{b: b, ratio: b.Ratio / sumRatio * 100})
		}
	}
	// 公共部分排在最后
	sort.SliceStable(rows, func(i, j int) bool {
		return (rows[i].b.UserID != nil) && (rows[j].b.UserID == nil)
	})

	ratioKwh := func(total float64, r float64) float64 { return round2(total * r / 100) }
	for _, nr := range rows {
		a := Allocation{
			UserID: nr.b.UserID, Name: nr.b.Name, Ratio: round2(nr.ratio), PanelCount: nr.b.PanelCount,
		}
		a.LostKwh = ratioKwh(res.LostKwhTotal, nr.ratio)
		a.LossQuality = ratioKwh(res.LossAmountQuality, nr.ratio)
		a.LossChannel = ratioKwh(res.LossAmountChannel, nr.ratio)
		a.LossPending = ratioKwh(res.LossAmountPending, nr.ratio)
		a.LossTotal = round2(a.LossQuality + a.LossChannel + a.LossPending)
		a.SupplierDue = a.LossQuality
		if res.SupplierDueTotal > 0 {
			a.SupplierPaid = round2(res.SupplierPaidTotal * nr.ratio / 100)
		}
		a.PropertyDue = a.LossChannel
		if res.PropertyDueTotal > 0 {
			a.PropertyPaid = round2(res.PropertyPaidTotal * nr.ratio / 100)
		}
		res.Allocations = append(res.Allocations, a)
	}
	balanceAllocationRounding(&res)
	return res
}

// balanceAllocationRounding 修正各分摊列四舍五入后的合计误差（归到第一行，通常为最大住户）。
func balanceAllocationRounding(res *Assessment) {
	if len(res.Allocations) == 0 {
		return
	}
	sum := func(get func(Allocation) float64) float64 {
		var s float64
		for _, a := range res.Allocations {
			s += get(a)
		}
		return round2(s)
	}
	fix := func(target float64, get func(Allocation) float64, set func(*Allocation, float64)) {
		d := round2(target - sum(get))
		if d != 0 {
			set(&res.Allocations[0], round2(get(res.Allocations[0])+d))
		}
	}
	fix(res.LostKwhTotal, func(a Allocation) float64 { return a.LostKwh }, func(a *Allocation, v float64) { a.LostKwh = v })
	fix(res.LossAmountQuality, func(a Allocation) float64 { return a.LossQuality }, func(a *Allocation, v float64) { a.LossQuality = v })
	fix(res.LossAmountChannel, func(a Allocation) float64 { return a.LossChannel }, func(a *Allocation, v float64) { a.LossChannel = v })
	fix(res.LossAmountPending, func(a Allocation) float64 { return a.LossPending }, func(a *Allocation, v float64) { a.LossPending = v })
	for i := range res.Allocations {
		res.Allocations[i].LossTotal = round2(res.Allocations[i].LossQuality + res.Allocations[i].LossChannel + res.Allocations[i].LossPending)
		res.Allocations[i].SupplierDue = res.Allocations[i].LossQuality
	}
	fix(res.SupplierPaidTotal, func(a Allocation) float64 { return a.SupplierPaid }, func(a *Allocation, v float64) { a.SupplierPaid = v })
	fix(res.PropertyPaidTotal, func(a Allocation) float64 { return a.PropertyPaid }, func(a *Allocation, v float64) { a.PropertyPaid = v })
}

// ---------- 修复前后发电曲线复核 ----------

// CurvePoint 日发电曲线点
type CurvePoint struct {
	Day time.Time `json:"day"`
	Kwh float64   `json:"kwh"`
}

// ReviewInput 曲线复核输入
type ReviewInput struct {
	BaselinePoints   []CurvePoint `json:"baseline_points"`   // 故障前正常日
	RecoveryPoints   []CurvePoint `json:"recovery_points"`   // 修复后观测日
	FallbackBaseline float64      `json:"fallback_baseline"` // 曲线不足时使用的基线日电量
	EventLossAmount  float64      `json:"event_loss_amount"` // 事件测算损失金额
	SupplierPaid     float64      `json:"supplier_paid"`
	PropertyPaid     float64      `json:"property_paid"`
	// 修复后仍欠发按多少天折算（默认取 recovery 点数）
	MinRecoveryPoints int `json:"min_recovery_points"`
}

// Review 曲线复核结论
type Review struct {
	BaselineAvg    float64 `json:"baseline_avg"`
	RecoveryAvg    float64 `json:"recovery_avg"`
	RecoveryRate   float64 `json:"recovery_rate"` // %
	RecoveryDays   int     `json:"recovery_days"`
	ExtraLossKwh   float64 `json:"extra_loss_kwh"` // 修复后持续欠发电量
	RealLossAmount float64 `json:"real_loss_amount"`
	PaidTotal      float64 `json:"paid_total"`
	CoverStatus    string  `json:"cover_status"` // pending / covered / shortfall / surplus
	Note           string  `json:"note"`
}

// CompareCurve 对比修复前后曲线，确认补偿是否覆盖真实损失。
func CompareCurve(in ReviewInput, ratePerKwh float64) Review {
	var rv Review
	avg := func(pts []CurvePoint) (float64, int) {
		if len(pts) == 0 {
			return 0, 0
		}
		var s float64
		for _, p := range pts {
			s += p.Kwh
		}
		return s / float64(len(pts)), len(pts)
	}
	baseAvg, baseN := avg(in.BaselinePoints)
	if baseN == 0 {
		baseAvg = in.FallbackBaseline
	}
	recAvg, recN := avg(in.RecoveryPoints)
	rv.BaselineAvg, rv.RecoveryAvg, rv.RecoveryDays = round2(baseAvg), round2(recAvg), recN
	minN := in.MinRecoveryPoints
	if minN <= 0 {
		minN = 3
	}
	if recN < minN || baseAvg <= 0 {
		rv.CoverStatus = "pending"
		rv.Note = "修复前后曲线样本不足，暂不能确认补偿是否覆盖真实损失"
		return rv
	}
	rv.RecoveryRate = round2(recAvg / baseAvg * 100)
	rv.ExtraLossKwh = round2(math.Max(0, baseAvg-recAvg) * float64(recN))
	rv.RealLossAmount = round2(in.EventLossAmount + rv.ExtraLossKwh*ratePerKwh)
	rv.PaidTotal = round2(in.SupplierPaid + in.PropertyPaid)
	switch {
	case rv.ExtraLossKwh > 0:
		rv.CoverStatus = "shortfall"
		rv.Note = "修复后日均发电仍低于故障前基线，存在持续欠发；既有补偿未完全覆盖真实损失，需向供应商/物业追加主张并安排复查"
	case rv.PaidTotal+0.005 >= rv.RealLossAmount:
		if rv.PaidTotal > rv.RealLossAmount+0.01 {
			rv.CoverStatus = "surplus"
			rv.Note = "赔付金额高于实际损失，结余按楼栋规则退回或结转"
		} else {
			rv.CoverStatus = "covered"
			rv.Note = "修复后发电恢复至基线水平，赔付覆盖真实损失"
		}
	default:
		rv.CoverStatus = "shortfall"
		rv.Note = "赔付金额低于实际损失，补偿未覆盖，需追加赔付"
	}
	return rv
}
