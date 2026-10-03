package outage

import (
	"math"
	"testing"
	"time"
)

func mkTime(s string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04", s, time.UTC)
	return t
}

func ptr(t time.Time) *time.Time { return &t }

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func bindings3() []Binding {
	return []Binding{
		{UserID: ptrID(1), Name: "张伟", Ratio: 15, PanelCount: 12}, // inv 内归一化 30%
		{UserID: ptrID(2), Name: "李娜", Ratio: 12.5, PanelCount: 10},
		{UserID: ptrID(3), Name: "王强", Ratio: 7.5, PanelCount: 6},
		{Name: "公共部分", Ratio: 15, PanelCount: 12}, // 30%
	}
}

func ptrID(x int64) *int64 { return &x }

// 纯质量故障：SLA 内到场，保修期内，全部损失由供应商赔到绑定住户/公共
func TestAssessQualityUnderWarranty(t *testing.T) {
	fault := mkTime("2026-09-12 08:00")
	res := Assess(Input{
		FaultAt: fault, RespondedAt: ptr(mkTime("2026-09-12 14:00")),
		ResolvedAt:       ptr(mkTime("2026-09-19 18:00")),
		ResponseSLAHours: 24, CauseType: CauseQuality, UnderWarranty: true,
		BaselineKwhPerDay: 51.1, GridPrice: 0.4153, SubsidyRate: 0.05,
		Bindings: bindings3(),
	})
	// 178 小时
	if !approx(res.TotalHours, 178) {
		t.Fatalf("hours=%v want 178", res.TotalHours)
	}
	if !res.WithinSLA || res.SLADelayHours != 0 {
		t.Fatalf("应在 SLA 内: %+v", res)
	}
	// 51.1 * 178/24 = 378.99
	if !approx(res.LostKwhTotal, 379) {
		t.Fatalf("lost=%v want 379", res.LostKwhTotal)
	}
	if !approx(res.LostKwhQuality, 379) || res.LostKwhChannel != 0 || res.LostKwhPending != 0 {
		t.Fatalf("责任段电量错误 q=%v c=%v p=%v", res.LostKwhQuality, res.LostKwhChannel, res.LostKwhPending)
	}
	if !approx(res.LossAmountTotal, 176.35) {
		t.Fatalf("loss=%v want 176.35", res.LossAmountTotal)
	}
	if !approx(res.SupplierDueTotal, 176.35) || res.PropertyDueTotal != 0 {
		t.Fatalf("due supplier=%v property=%v", res.SupplierDueTotal, res.PropertyDueTotal)
	}
	if len(res.Segments) != 1 || res.Segments[0].Kind != CauseQuality {
		t.Fatalf("segments=%+v", res.Segments)
	}

	// 归一化分摊 30/25/15/30
	byName := map[string]Allocation{}
	for _, a := range res.Allocations {
		byName[a.Name] = a
	}
	zw := byName["张伟"]
	if !approx(zw.Ratio, 30) || zw.PanelCount != 12 {
		t.Fatalf("张伟 ratio=%v panels=%v", zw.Ratio, zw.PanelCount)
	}
	if !approx(zw.SupplierDue, 52.91) { // 176.35*0.3
		t.Fatalf("张伟 supplier due=%v want 52.91", zw.SupplierDue)
	}
	if zw.LossChannel != 0 {
		t.Fatalf("质量故障不应有物业损失: %v", zw.LossChannel)
	}
	pub := byName["公共部分"]
	if !approx(pub.SupplierDue, 52.9) {
		t.Fatalf("公共 supplier due=%v", pub.SupplierDue)
	}
	// 各列合计守恒
	var sumDue float64
	for _, a := range res.Allocations {
		sumDue += a.SupplierDue
	}
	if !approx(sumDue, 176.35) {
		t.Fatalf("住户+公共 supplier due 合计=%v want 176.35", sumDue)
	}
}

// 保修期外：质量段不向供应商主张，应赔为 0
func TestAssessOutOfWarrantyNoSupplierDue(t *testing.T) {
	res := Assess(Input{
		FaultAt: mkTime("2026-10-01 08:00"), ResolvedAt: ptr(mkTime("2026-10-03 08:00")),
		ResponseSLAHours: 24, CauseType: CauseQuality, UnderWarranty: false,
		BaselineKwhPerDay: 40, GridPrice: 0.4,
		Bindings: []Binding{{UserID: ptrID(1), Ratio: 100}},
	})
	if res.SupplierDueTotal != 0 {
		t.Fatalf("保修期外供应商应赔应为 0, got %v", res.SupplierDueTotal)
	}
	if !approx(res.LossAmountTotal, 32) {
		t.Fatalf("损失仍应记录: %v", res.LossAmountTotal)
	}
}

// 混合责任：SLA 24h；通道在第 24h 后被占用，直到修复。前 24h 质量，之后通道（物业）
func TestAssessMixedChannelDelay(t *testing.T) {
	fault := mkTime("2026-10-08 08:00")
	res := Assess(Input{
		FaultAt:          fault,
		RespondedAt:      ptr(mkTime("2026-10-09 20:00")), // 36h，超 SLA 12h
		ResolvedAt:       ptr(mkTime("2026-10-11 20:00")), // 84h
		ResponseSLAHours: 24,
		CauseType:        CauseMixed, UnderWarranty: true,
		ChannelBlockedAt:  ptr(mkTime("2026-10-09 08:00")),
		ChannelFreedAt:    ptr(mkTime("2026-10-11 20:00")),
		BaselineKwhPerDay: 50, GridPrice: 0.4, SubsidyRate: 0.05,
		Bindings: []Binding{
			{UserID: ptrID(9), Name: "丙", Ratio: 20}, // 40%
			{UserID: ptrID(2), Name: "乙", Ratio: 15}, // 30%
			{Name: "公共部分", Ratio: 15},                // 30%
		},
	})
	if res.WithinSLA {
		t.Fatalf("响应 36h > SLA 24h，应判超时")
	}
	if !approx(res.SLADelayHours, 12) {
		t.Fatalf("delay=%v want 12", res.SLADelayHours)
	}
	// 质量 24h -> 50kWh；通道 60h -> 125kWh
	if !approx(res.LostKwhQuality, 50) || !approx(res.LostKwhChannel, 125) {
		t.Fatalf("q=%v c=%v want 50/125", res.LostKwhQuality, res.LostKwhChannel)
	}
	if !approx(res.LostKwhTotal, 175) {
		t.Fatalf("total=%v want 175", res.LostKwhTotal)
	}
	if !approx(res.SupplierDueTotal, 22.5) || !approx(res.PropertyDueTotal, 56.25) {
		t.Fatalf("due supplier=%v property=%v want 22.5/56.25", res.SupplierDueTotal, res.PropertyDueTotal)
	}
	byName := map[string]Allocation{}
	for _, a := range res.Allocations {
		byName[a.Name] = a
	}
	yi := byName["乙"]
	if !approx(yi.SupplierDue, 6.75) { // 22.5*0.3
		t.Fatalf("乙 supplier=%v want 6.75", yi.SupplierDue)
	}
	if !approx(yi.PropertyDue, 16.88) { // 56.25*0.3=16.875
		t.Fatalf("乙 property=%v want 16.88", yi.PropertyDue)
	}
	// 分段之和守恒（修正四舍五入后）
	var sumCh float64
	for _, a := range res.Allocations {
		sumCh += a.LossChannel
	}
	if !approx(sumCh, 56.25) {
		t.Fatalf("物业段金额合计=%v want 56.25", sumCh)
	}
	// 三段顺序：quality -> channel
	if len(res.Segments) != 2 || res.Segments[0].Kind != "quality" || res.Segments[1].Kind != "channel" {
		t.Fatalf("segments=%+v", res.Segments)
	}
}

// 通道占用但发生在 SLA 内：不应产生物业责任（正常响应窗口内）
func TestChannelInsideSLANotCounted(t *testing.T) {
	fault := mkTime("2026-10-08 08:00")
	res := Assess(Input{
		FaultAt: fault, RespondedAt: ptr(mkTime("2026-10-08 12:00")),
		ResolvedAt:       ptr(mkTime("2026-10-09 08:00")), // 24h 全部在 SLA 窗口内
		ResponseSLAHours: 24, CauseType: CauseChannel, UnderWarranty: true,
		ChannelBlockedAt:  ptr(mkTime("2026-10-08 09:00")),
		ChannelFreedAt:    ptr(mkTime("2026-10-09 08:00")),
		BaselineKwhPerDay: 48, GridPrice: 0.4,
		Bindings: []Binding{{UserID: ptrID(1), Ratio: 100}},
	})
	if res.PropertyDueTotal != 0 {
		t.Fatalf("SLA 内通道占用不应计物业责任, property due=%v", res.PropertyDueTotal)
	}
}

// 待判定：全部损失挂账 pending，无任何赔付
func TestAssessPending(t *testing.T) {
	res := Assess(Input{
		FaultAt: mkTime("2026-10-01 00:00"), AsOf: mkTime("2026-10-02 00:00"),
		ResponseSLAHours: 24, CauseType: CausePending,
		BaselineKwhPerDay: 40, GridPrice: 0.4,
		Bindings: []Binding{{UserID: ptrID(1), Ratio: 100}},
	})
	if !approx(res.LostKwhPending, 40) {
		t.Fatalf("pending kwh=%v want 40", res.LostKwhPending)
	}
	if res.SupplierDueTotal != 0 || res.PropertyDueTotal != 0 {
		t.Fatalf("待判定不应有赔付: s=%v p=%v", res.SupplierDueTotal, res.PropertyDueTotal)
	}
}

// 供应商只实际赔了应赔的一部分：payable 挂差，按比例分到户
func TestAssessPartialSupplierPayment(t *testing.T) {
	res := Assess(Input{
		FaultAt: mkTime("2026-10-01 00:00"), ResolvedAt: ptr(mkTime("2026-10-02 00:00")),
		ResponseSLAHours: 24, CauseType: CauseQuality, UnderWarranty: true,
		BaselineKwhPerDay: 100, GridPrice: 0.4,
		SupplierPaidTotal: 30, // 应赔 40，实到 30
		Bindings:          []Binding{{UserID: ptrID(1), Ratio: 50}, {Name: "公共部分", Ratio: 50}},
	})
	if !approx(res.SupplierDueTotal, 40) {
		t.Fatalf("due=%v want 40", res.SupplierDueTotal)
	}
	if !approx(res.SupplierPaidTotal, 30) || !approx(res.SupplierPayable, 10) {
		t.Fatalf("paid=%v payable=%v want 30/10", res.SupplierPaidTotal, res.SupplierPayable)
	}
	if !approx(res.Allocations[0].SupplierPaid, 15) {
		t.Fatalf("首户实赔=%v want 15", res.Allocations[0].SupplierPaid)
	}
}

// ---------- 曲线复核 ----------

func day(s string) time.Time { t, _ := time.ParseInLocation("2006-01-02", s, time.UTC); return t }

func cp(d string, kwh float64) CurvePoint { return CurvePoint{Day: day(d), Kwh: kwh} }

func TestReviewCovered(t *testing.T) {
	base, rec := []CurvePoint{}, []CurvePoint{}
	for i := 1; i <= 7; i++ {
		base = append(base, cp("2026-09-0"+itoa(i), 50))
	}
	for i := 20; i <= 26; i++ {
		rec = append(rec, cp("2026-09-"+itoa(i), 50))
	}
	rv := CompareCurve(ReviewInput{
		BaselinePoints: base, RecoveryPoints: rec,
		EventLossAmount: 100, SupplierPaid: 100,
	}, 0.45)
	if rv.CoverStatus != "covered" {
		t.Fatalf("status=%v note=%s", rv.CoverStatus, rv.Note)
	}
	if !approx(rv.RecoveryRate, 100) || rv.ExtraLossKwh != 0 {
		t.Fatalf("rate=%v extra=%v", rv.RecoveryRate, rv.ExtraLossKwh)
	}
}

func TestReviewShortfallAfterRepair(t *testing.T) {
	base, rec := []CurvePoint{}, []CurvePoint{}
	for i := 1; i <= 7; i++ {
		base = append(base, cp("2026-09-0"+itoa(i), 50))
	}
	for i := 20; i <= 26; i++ {
		rec = append(rec, cp("2026-09-"+itoa(i), 40)) // 修复后仍欠发 20%
	}
	rv := CompareCurve(ReviewInput{
		BaselinePoints: base, RecoveryPoints: rec,
		EventLossAmount: 90, SupplierPaid: 90,
	}, 0.45)
	if rv.CoverStatus != "shortfall" {
		t.Fatalf("应 shortfall, got %v (%s)", rv.CoverStatus, rv.Note)
	}
	if !approx(rv.ExtraLossKwh, 70) { // (50-40)*7
		t.Fatalf("extra=%v want 70", rv.ExtraLossKwh)
	}
}

func TestReviewInsufficientSamples(t *testing.T) {
	rv := CompareCurve(ReviewInput{
		BaselinePoints:   []CurvePoint{cp("2026-09-01", 50)},
		RecoveryPoints:   []CurvePoint{cp("2026-09-20", 50), cp("2026-09-21", 49)},
		FallbackBaseline: 50,
	}, 0.45)
	if rv.CoverStatus != "pending" {
		t.Fatalf("样本不足应 pending, got %v", rv.CoverStatus)
	}
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
