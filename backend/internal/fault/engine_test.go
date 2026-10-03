package fault

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// 基准场景：inv2 占整楼 50%，故障前日均 70kWh，停机 7 天=168h，电价 0.4153 + 补贴 0.05
func baseInput(cause string, delay float64) Input {
	return Input{
		InverterID: 2, InverterName: "2#逆变器", InverterRatio: 50,
		PeriodMonth:   "2026-10",
		DowntimeHours: 168, ResponseHours: 6, SLAHours: 24,
		DelayHours: delay, Cause: cause,
		BaselineDailyKwh: 70, GridPrice: 0.4153, SubsidyRate: 0.05,
		Shareholders: []Share{
			{UserID: 1, Name: "张伟", Ratio: 30},
			{UserID: 2, Name: "李娜", Ratio: 25},
			{UserID: 3, Name: "王强", Ratio: 15},
		},
	}
}

func TestQualityFaultUnderWarranty(t *testing.T) {
	in := baseInput("quality", 0)
	in.WarrantyCovered = true
	res := Assess(in)

	if !res.SLAMet {
		t.Fatalf("响应 6h 应满足 24h SLA")
	}
	// 70/24*168 = 490 kWh
	if !approx(res.LostKwh, 490) {
		t.Fatalf("lostKwh=%v want 490", res.LostKwh)
	}
	// 490 * 0.4653 = 227.997 → 228.00
	if !approx(res.TotalLoss, 228.00) {
		t.Fatalf("totalLoss=%v want 228.00", res.TotalLoss)
	}
	if !approx(res.SupplierCompensation, 228.00) {
		t.Fatalf("supplier=%v want 228.00", res.SupplierCompensation)
	}
	if res.PropertyCompensation != 0 {
		t.Fatalf("物业赔付应为 0, got %v", res.PropertyCompensation)
	}
	if !res.ReviewNeeded {
		t.Fatal("质量赔付触发后应提醒同型号复查")
	}

	// 张伟持有整楼 30% 组件 → 147 kWh，赔付 68.40
	zhang := res.Items[0]
	if !approx(zhang.LostKwh, 147) || !approx(zhang.SupplierAmount, 68.40) {
		t.Fatalf("张伟 %+v", zhang)
	}
	if zhang.PropertyAmount != 0 {
		t.Fatalf("张伟物业赔付应为 0: %v", zhang.PropertyAmount)
	}
	// 未认购 30% 归公共部分：147 kWh / 68.40 元供应商赔入公共账户
	var pub *Item
	for i := range res.Items {
		if res.Items[i].Party == "public" {
			pub = &res.Items[i]
		}
	}
	if pub == nil || !approx(pub.LostKwh, 147) || !approx(pub.SupplierAmount, 68.40) {
		t.Fatalf("公共明细 %+v", pub)
	}
}

func TestChannelDelayGoesToProperty(t *testing.T) {
	in := baseInput("channel", 168)
	res := Assess(in)
	if !approx(res.ChannelLostKwh, 490) || res.QualityLostKwh != 0 {
		t.Fatalf("电量拆分错误 q=%v c=%v", res.QualityLostKwh, res.ChannelLostKwh)
	}
	if !approx(res.PropertyCompensation, 228.00) || res.SupplierCompensation != 0 {
		t.Fatalf("赔付拆分错误 sup=%v prop=%v", res.SupplierCompensation, res.PropertyCompensation)
	}
	// 李娜持有 25% → 122.5kWh → 57.00 元（经公共账代发住户）
	li := res.Items[1]
	if !approx(li.PropertyAmount, 57.00) || li.SupplierAmount != 0 {
		t.Fatalf("李娜 %+v", li)
	}
}

func TestMixedResponsibilitySplit(t *testing.T) {
	// 总停机 168h，其中通道占用延迟 72h（到场后无法进场），质量 96h
	in := baseInput("mixed", 72)
	in.WarrantyCovered = true
	res := Assess(in)
	if !approx(res.QualityHours, 96) || !approx(res.DelayHours, 72) {
		t.Fatalf("时长拆分 q=%v d=%v", res.QualityHours, res.DelayHours)
	}
	hourly := 70.0 / 24
	if !approx(res.QualityLostKwh, round2(hourly*96)) || !approx(res.ChannelLostKwh, round2(hourly*72)) {
		t.Fatalf("电量 q=%v c=%v", res.QualityLostKwh, res.ChannelLostKwh)
	}
	// 供应商赔付质量时段全额、物业赔延迟时段全额 → 覆盖率 100
	if !approx(res.CoverRatio, 100) {
		t.Fatalf("coverRatio=%v want 100", res.CoverRatio)
	}
	if res.UncoveredLoss > 0.01 {
		t.Fatalf("uncovered=%v 应仅有分币尾差", res.UncoveredLoss)
	}
	// 王强持有 15%：质量与延迟损失均按 0.15 分摊
	wq := res.Items[2]
	wantSup := round2(res.QualityLoss * 0.15)
	wantProp := round2(res.ChannelLoss * 0.15)
	if !approx(wq.SupplierAmount, wantSup) || !approx(wq.PropertyAmount, wantProp) {
		t.Fatalf("王强 sup=%v/%v prop=%v/%v", wq.SupplierAmount, wantSup, wq.PropertyAmount, wantProp)
	}
	// 各方赔付之和应等于供应商/物业赔付总额（住户+公共）
	var supSum, propSum float64
	for _, it := range res.Items {
		supSum += it.SupplierAmount
		propSum += it.PropertyAmount
	}
	if !approx(supSum, res.SupplierCompensation) || !approx(propSum, res.PropertyCompensation) {
		t.Fatalf("明细合计 sup=%v/%v prop=%v/%v", supSum, res.SupplierCompensation, propSum, res.PropertyCompensation)
	}
}

func TestQualityOutsideWarrantyUncovered(t *testing.T) {
	in := baseInput("quality", 0)
	in.WarrantyCovered = false
	res := Assess(in)
	if res.SupplierCompensation != 0 || !approx(res.UncoveredLoss, 228.00) {
		t.Fatalf("超保修质量损失应暂无赔付 sup=%v uncov=%v", res.SupplierCompensation, res.UncoveredLoss)
	}
	if res.CoverRatio != 0 || res.ReviewNeeded {
		t.Fatal("无供应商赔付时不应触发同型号复查")
	}
}

func TestPartialWarrantyCoverage(t *testing.T) {
	// 保修条款约定供应商仅承担 80% 质量损失（如不含补贴）
	in := baseInput("quality", 0)
	in.WarrantyCovered = true
	in.QualityCoveragePct = 80
	res := Assess(in)
	if !approx(res.SupplierCompensation, 182.40) {
		t.Fatalf("supplier=%v want 182.40", res.SupplierCompensation)
	}
	if !approx(res.UncoveredLoss, 45.60) || !approx(res.CoverRatio, 80) {
		t.Fatalf("uncov=%v cover=%v", res.UncoveredLoss, res.CoverRatio)
	}
}

func TestPublicShareWhenSubscriptionUnderDeviceRatio(t *testing.T) {
	// 逆变器占 60%，住户合计仅认购 40% → 住户损失 40%、未认购公共部分 60%
	in := Input{
		InverterRatio: 60, DowntimeHours: 24, Cause: "quality",
		BaselineDailyKwh: 60, GridPrice: 0.4, SubsidyRate: 0,
		WarrantyCovered: true,
		Shareholders:    []Share{{UserID: 9, Name: "甲", Ratio: 40}},
	}
	res := Assess(in)
	// 总损失 60kWh / 24元；甲 40% → 24kWh/9.60元；公共 60% → 36kWh/14.40元
	var pub *Item
	for i := range res.Items {
		if res.Items[i].Party == "public" {
			pub = &res.Items[i]
		}
	}
	if pub == nil {
		t.Fatal("应生成公共部分明细")
	}
	if !approx(pub.LostKwh, 36) || !approx(pub.SupplierAmount, 14.40) {
		t.Fatalf("公共 %+v", pub)
	}
	if !approx(res.Items[0].LostKwh, 24) || !approx(res.Items[0].SupplierAmount, 9.60) {
		t.Fatalf("甲 %+v", res.Items[0])
	}
}

func TestSLABreach(t *testing.T) {
	in := baseInput("quality", 0)
	in.ResponseHours = 30
	res := Assess(in)
	if res.SLAMet {
		t.Fatal("响应 30h 超过 24h SLA，应标记未满足")
	}
}

func TestCompareCurve(t *testing.T) {
	// 出力恢复且赔付足额
	r := CompareCurve(70, 68, 228, 228)
	if !approx(r.RecoveryRatio, 97.14) || r.Status != "covered" {
		t.Fatalf("covered case %+v", r)
	}
	// 出力未恢复
	r = CompareCurve(70, 60, 228, 228)
	if r.Status != "insufficient" {
		t.Fatalf("恢复率不足应 insufficient: %+v", r)
	}
	// 出力恢复但赔付不足
	r = CompareCurve(70, 70, 228, 180)
	if r.Status != "insufficient" || !approx(r.CoverRatio, 78.95) {
		t.Fatalf("赔付不足应 insufficient: %+v", r)
	}
}
