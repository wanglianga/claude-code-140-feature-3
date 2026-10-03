// Package fault 实现逆变器停机故障的住户收益损失分摊引擎（纯计算，无数据库依赖，便于单元测试）。
//
// 业务口径：
//   - 逆变器按容量绑定整楼组件（capacity_ratio%）；某台逆变器停机时，其损失电量按各住户
//     绑定到该设备的组件比例（住户认购比例 / 逆变器占比）分摊到住户与公共部分；
//   - 设备质量导致的停机：保修期内由供应商按发电量损失赔付，赔付直接进入住户收益补偿
//     （未认购的公共组件部分赔付款进入公共账户）；
//   - 屋顶维护通道被占用导致维修延迟：延迟时段损失由物业赔偿，赔付款先进楼栋公共账并
//     出具说明，再按组件比例代发给受影响住户，公共部分留存公共账；
//   - 混合责任按质量时段 / 通道延迟时段的小时数拆分；
//   - 修复后对比修复前后日均发电曲线，结合实际赔付计算覆盖率，确认补偿是否覆盖真实损失。
package fault

import "math"

// 出力恢复率达到该阈值才认为设备性能恢复正常
const RecoveryThresholdPct = 95.0

// Share 已认购住户在整楼的组件份额（%）
type Share struct {
	UserID int64   `json:"user_id"`
	Name   string  `json:"name"`
	Ratio  float64 `json:"ratio"`
}

// Input 故障损失评估输入（时长单位：小时；电量：kWh；金额：元）
type Input struct {
	InverterID    int64   `json:"inverter_id"`
	InverterName  string  `json:"inverter_name"`
	InverterRatio float64 `json:"inverter_ratio"` // 该逆变器绑定组件占整楼比例 %
	PeriodMonth   string  `json:"period_month"`

	DowntimeHours float64 `json:"downtime_hours"` // 故障→修复总停机时长
	ResponseHours float64 `json:"response_hours"` // 运维到场响应时长
	SLAHours      float64 `json:"sla_hours"`      // 保修条款承诺的响应时长
	DelayHours    float64 `json:"delay_hours"`    // 维护通道占用导致的维修延迟时长

	// cause: quality 设备质量 / channel 通道占用延迟 / mixed 两者混合 / undetermined 待判定
	Cause string `json:"cause"`

	BaselineDailyKwh float64 `json:"baseline_daily_kwh"` // 故障前该逆变器日均发电量（基准曲线）
	GridPrice        float64 `json:"grid_price"`         // 上网电价 元/kWh
	SubsidyRate      float64 `json:"subsidy_rate"`       // 度电补贴 元/kWh

	// 保修条款适用结果：保修期内、设备质量责任时供应商承担赔付；
	// QualityCoveragePct 为供应商确认承担的质量损失比例（默认 100，
	// 例如保修条款约定只赔电费不赔补贴时可下调）。
	WarrantyCovered    bool    `json:"warranty_covered"`
	QualityCoveragePct float64 `json:"quality_coverage_pct"`

	Shareholders []Share `json:"shareholders"`
}

// Item 单个受影响住户（或公共部分）的损失与赔付明细
type Item struct {
	UserID         *int64  `json:"user_id"`
	Party          string  `json:"party"` // shareholder / public
	Name           string  `json:"name"`
	Ratio          float64 `json:"ratio"` // 绑定该逆变器的组件占比（折算到整楼 %）
	LostKwh        float64 `json:"lost_kwh"`
	LossAmount     float64 `json:"loss_amount"`     // 真实收益损失（电费+补贴）
	SupplierAmount float64 `json:"supplier_amount"` // 供应商赔付（质量时段）
	PropertyAmount float64 `json:"property_amount"` // 物业赔偿（通道延迟时段，经公共账代发）
}

// Assessment 故障损失分摊评估结果
type Assessment struct {
	Cause  string `json:"cause"`
	SLAMet bool   `json:"sla_met"`

	DowntimeHours float64 `json:"downtime_hours"`
	ResponseHours float64 `json:"response_hours"`
	SLAHours      float64 `json:"sla_hours"`
	QualityHours  float64 `json:"quality_hours"`
	DelayHours    float64 `json:"delay_hours"`

	BaselineDailyKwh float64 `json:"baseline_daily_kwh"`
	LostKwh          float64 `json:"lost_kwh"`
	QualityLostKwh   float64 `json:"quality_lost_kwh"`
	ChannelLostKwh   float64 `json:"channel_lost_kwh"`

	GridPrice            float64 `json:"grid_price"`
	SubsidyRate          float64 `json:"subsidy_rate"`
	TotalLoss            float64 `json:"total_loss"`
	QualityLoss          float64 `json:"quality_loss"`
	ChannelLoss          float64 `json:"channel_loss"`
	WarrantyCovered      bool    `json:"warranty_covered"`
	QualityCoveragePct   float64 `json:"quality_coverage_pct"`
	SupplierCompensation float64 `json:"supplier_compensation"`
	PropertyCompensation float64 `json:"property_compensation"`
	UncoveredLoss        float64 `json:"uncovered_loss"` // 暂无责任方/赔付覆盖的损失
	CoverRatio           float64 `json:"cover_ratio"`    // 赔付/真实损失 %
	ReviewNeeded         bool    `json:"review_needed"`  // 是否需要复查同型号设备

	Items []Item `json:"items"`
}

// CurveResult 修复前后发电曲线对比结果
type CurveResult struct {
	PreDailyKwh   float64 `json:"pre_daily_kwh"`
	PostDailyKwh  float64 `json:"post_daily_kwh"`
	RecoveryRatio float64 `json:"recovery_ratio"` // 修复后/修复前日均出力 %
	CoverRatio    float64 `json:"cover_ratio"`    // 已赔付/真实损失 %
	Status        string  `json:"status"`         // covered / insufficient
	Note          string  `json:"note"`
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }
func round4(x float64) float64 { return math.Round(x*10000) / 10000 }

// Assess 依据设备绑定组件、停机时长、运维响应与保修条款计算受影响各方损失与赔付。
func Assess(in Input) Assessment {
	covPct := in.QualityCoveragePct
	if covPct <= 0 {
		covPct = 100
	}
	if covPct > 100 {
		covPct = 100
	}
	downtime := math.Max(0, in.DowntimeHours)
	delay := math.Max(0, math.Min(in.DelayHours, downtime))
	quality := downtime - delay
	if in.Cause == "channel" {
		// 纯通道责任：全部停机视为延迟，不产生质量时段
		delay, quality = downtime, 0
	}
	if in.Cause == "quality" {
		delay, quality = 0, downtime
	}

	res := Assessment{
		Cause:              in.Cause,
		DowntimeHours:      round2(downtime),
		ResponseHours:      round2(math.Max(0, in.ResponseHours)),
		SLAHours:           round2(math.Max(0, in.SLAHours)),
		QualityHours:       round2(quality),
		DelayHours:         round2(delay),
		BaselineDailyKwh:   round2(in.BaselineDailyKwh),
		GridPrice:          in.GridPrice,
		SubsidyRate:        in.SubsidyRate,
		WarrantyCovered:    in.WarrantyCovered,
		QualityCoveragePct: covPct,
	}
	res.SLAMet = in.SLAHours <= 0 || res.ResponseHours <= res.SLAHours

	// 损失电量按基准曲线（日均/24）折算各时段停机损失
	hourly := in.BaselineDailyKwh / 24
	res.LostKwh = round2(hourly * downtime)
	res.QualityLostKwh = round2(hourly * quality)
	res.ChannelLostKwh = round2(hourly * delay)

	rate := in.GridPrice + in.SubsidyRate
	res.TotalLoss = round2(res.LostKwh * rate)
	res.QualityLoss = round2(res.QualityLostKwh * rate)
	res.ChannelLoss = round2(res.ChannelLostKwh * rate)

	// 质量时段：保修期内由供应商按确认比例赔付；通道延迟：物业全额赔偿
	if in.WarrantyCovered && (in.Cause == "quality" || in.Cause == "mixed") {
		res.SupplierCompensation = round2(res.QualityLoss * covPct / 100)
	}
	if in.Cause == "channel" || in.Cause == "mixed" {
		res.PropertyCompensation = res.ChannelLoss
	}
	if res.TotalLoss > 0 {
		res.CoverRatio = round2((res.SupplierCompensation + res.PropertyCompensation) / res.TotalLoss * 100)
	}
	res.UncoveredLoss = round2(math.Max(0, res.TotalLoss-res.SupplierCompensation-res.PropertyCompensation))

	// 设备质量责任（含混合）且触发保修赔付时，提醒运维复查同型号设备
	res.ReviewNeeded = res.SupplierCompensation > 0

	// 组件按容量均匀绑定到各台逆变器：住户认购了整楼 panel_ratio% 的组件，
	// 即持有每台设备 panel_ratio% 的组件；设备损失按整楼比例分给住户，
	// 未认购部分归公共（分项系数之和恒为 100%，与设备容量占比无关）。
	var sumRatio float64
	for _, s := range in.Shareholders {
		sumRatio += s.Ratio
	}
	publicRatio := math.Max(0, 100-sumRatio)

	qualityAmt := res.QualityLoss
	channelAmt := res.ChannelLoss
	partyLoss := func(ratio float64) (float64, float64, float64) {
		f := ratio / 100
		lostKwh := round2(res.LostKwh * f)
		supplierAmt, propertyAmt := 0.0, 0.0
		if in.WarrantyCovered && (in.Cause == "quality" || in.Cause == "mixed") {
			supplierAmt = round2(qualityAmt * f * covPct / 100)
		}
		if in.Cause == "channel" || in.Cause == "mixed" {
			propertyAmt = round2(channelAmt * f)
		}
		return lostKwh, supplierAmt, propertyAmt
	}
	for _, s := range in.Shareholders {
		lostKwh, supplierAmt, propertyAmt := partyLoss(s.Ratio)
		uid := s.UserID
		res.Items = append(res.Items, Item{
			UserID:         &uid,
			Party:          "shareholder",
			Name:           s.Name,
			Ratio:          round4(s.Ratio),
			LostKwh:        lostKwh,
			LossAmount:     round2(lostKwh * rate),
			SupplierAmount: supplierAmt,
			PropertyAmount: propertyAmt,
		})
	}
	// 公共部分 = 整楼未认购组件对应的损失
	if publicRatio > 0.000001 {
		pubLost, pubSupplier, pubProperty := partyLoss(publicRatio)
		res.Items = append(res.Items, Item{
			UserID:         nil,
			Party:          "public",
			Name:           "公共部分（未认购组件）",
			Ratio:          round4(publicRatio),
			LostKwh:        pubLost,
			LossAmount:     round2(pubLost * rate),
			SupplierAmount: pubSupplier,
			PropertyAmount: pubProperty,
		})
	}

	// 尾差平衡：各行分币四舍五入后，把与总额的差额补到金额最大的一行，
	// 保证住户/公共明细之和恒等于损失与赔付总额（入账可逐分对账）。
	balance := func(target float64, get func(*Item) float64, set func(*Item, float64)) {
		var sum float64
		big := -1
		for i := range res.Items {
			sum += get(&res.Items[i])
			if big < 0 || get(&res.Items[i]) > get(&res.Items[big]) {
				big = i
			}
		}
		diff := round2(target - sum)
		if diff != 0 && big >= 0 {
			set(&res.Items[big], round2(get(&res.Items[big])+diff))
		}
	}
	balance(res.LostKwh, func(it *Item) float64 { return it.LostKwh }, func(it *Item, v float64) { it.LostKwh = v })
	balance(res.TotalLoss, func(it *Item) float64 { return it.LossAmount }, func(it *Item, v float64) { it.LossAmount = v })
	balance(res.SupplierCompensation, func(it *Item) float64 { return it.SupplierAmount }, func(it *Item, v float64) { it.SupplierAmount = v })
	balance(res.PropertyCompensation, func(it *Item) float64 { return it.PropertyAmount }, func(it *Item, v float64) { it.PropertyAmount = v })
	return res
}

// CompareCurve 对比修复前后发电曲线，并结合已落实赔付判断补偿是否覆盖真实损失。
//
//	preDaily  修复前基准日均发电量
//	postDaily 修复后观测期实测日均发电量
//	totalLoss 评估认定的真实收益损失
//	paidComp  实际到账赔付（供应商+物业）
func CompareCurve(preDaily, postDaily, totalLoss, paidComp float64) CurveResult {
	r := CurveResult{PreDailyKwh: round2(preDaily), PostDailyKwh: round2(postDaily)}
	if preDaily > 0 {
		r.RecoveryRatio = round2(postDaily / preDaily * 100)
	}
	if totalLoss > 0 {
		r.CoverRatio = round2(paidComp / totalLoss * 100)
	}
	switch {
	case r.RecoveryRatio < RecoveryThresholdPct:
		r.Status = "insufficient"
		r.Note = "修复后出力未恢复至基准曲线 95%，需运维再次复查设备；补偿暂不能确认为足额覆盖"
	case r.CoverRatio < 100:
		r.Status = "insufficient"
		r.Note = "出力已恢复，但赔付未完全覆盖真实损失，需继续向责任方追偿差额"
	default:
		r.Status = "covered"
		r.Note = "出力已恢复且赔付覆盖真实损失，补偿闭环完成"
	}
	return r
}
