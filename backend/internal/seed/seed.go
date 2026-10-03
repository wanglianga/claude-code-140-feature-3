// Package seed 在首次启动时灌入演示数据：管理员、物业、6 户居民、两个示例楼栋。
package seed

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pvplatform/internal/auth"
	"pvplatform/internal/outage"
	"pvplatform/internal/settlement"
)

func Run(ctx context.Context, pool *pgxpool.Pool) error {
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	mkUser := func(name, phone, unit, role, pwd string) int64 {
		h, _ := auth.HashPassword(pwd)
		var id int64
		if err := tx.QueryRow(ctx,
			`INSERT INTO users(name,phone,unit_no,role,password_hash) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			name, phone, unit, role, h).Scan(&id); err != nil {
			panic(err)
		}
		return id
	}
	admin := mkUser("平台管理员", "13800000000", "", "admin", "admin123")
	property := mkUser("阳光物业-王经理", "13800000001", "物业办", "property", "property123")
	r1 := mkUser("张伟(顶楼)", "13900000001", "1-601", "resident", "resident123")
	r2 := mkUser("李娜", "13900000002", "1-602", "resident", "resident123")
	r3 := mkUser("王强", "13900000003", "1-501", "resident", "resident123")
	r4 := mkUser("赵敏", "13900000004", "1-502", "resident", "resident123")
	r5 := mkUser("陈杰", "13900000005", "1-401", "resident", "resident123")
	r6 := mkUser("刘洋(新业主)", "13900000006", "1-402", "resident", "resident123")

	// 楼栋 1：已运行，含两个月份的发电与结算
	var b1 int64
	err = tx.QueryRow(ctx, `
		INSERT INTO buildings(name,address,roof_area,orientation,load_assessment,shading,
			ownership_note,public_revenue_rule,grid_condition,construction_impact,
			total_capacity_kw,panel_total,unit_price,stage,subscribed_ratio)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'operating',$14) RETURNING id`,
		"幸福里 1 号楼", "幸福里小区 1 号", 320, "正南偏西 8°",
		"原设计活荷载 2.0kN/m²，加装支架后满足要求（2026-03 第三方评估）",
		"上午无遮挡；下午西侧女儿墙遮挡约 6%",
		"屋顶为全体业主共有，公共部分收益归公共账户；顶层住户另签漏水补偿备忘录",
		`{"maintenance_fund_ratio":20,"note":"公共净收益的20%划入维修基金"}`,
		"小区配电室余量 120kW，拟 0.4kV 全额自发自用、余电上网",
		"施工期 15 天，占用消防通道 2 处，已制定绕行方案",
		40, 80, 1500, 70).Scan(&b1)
	if err != nil {
		return err
	}
	members := []int64{property, r1, r2, r3, r4, r5, r6}
	for i, uid := range members {
		mt := "resident"
		if i == 0 {
			mt = "property"
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO building_members(building_id,user_id,member_type) VALUES ($1,$2,$3)`,
			b1, uid, mt); err != nil {
			return err
		}
	}
	stageSeq := []string{"draft", "voting", "subscribing", "construction", "grid_acceptance", "operating"}
	var prev *string
	for _, st := range stageSeq {
		if _, err := tx.Exec(ctx,
			`INSERT INTO stage_logs(building_id,from_stage,to_stage,reason,operator_id) VALUES ($1,$2,$3,$4,$5)`,
			b1, prev, st, "种子数据", admin); err != nil {
			return err
		}
		s := st
		prev = &s
	}

	// 认购：张伟 30%、李娜 25%、王强 15%；赵敏只参与公共收益分配
	sub := func(uid int64, ratio float64) {
		count := int(ratio/100*80 + 0.5)
		if _, err := tx.Exec(ctx, `
			INSERT INTO subscriptions(building_id,user_id,sub_type,panel_ratio,panel_count,amount,status,paid_at)
			VALUES ($1,$2,'shareholder',$3,$4,$5,'paid',now())`,
			b1, uid, ratio, count, count*1500); err != nil {
			panic(err)
		}
	}
	sub(r1, 30)
	sub(r2, 25)
	sub(r3, 15)
	if _, err := tx.Exec(ctx, `
		INSERT INTO subscriptions(building_id,user_id,sub_type,panel_ratio,status,paid_at)
		VALUES ($1,$2,'public_only',0,'paid',now())`, b1, r4); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO payments(building_id,user_id,amount,pay_type,memo)
		 VALUES ($1,$2,36000,'subscription','30% 组件认购 24 块'),
		        ($1,$3,30000,'subscription','25% 组件认购 20 块'),
		        ($1,$4,18000,'subscription','15% 组件认购 12 块')`,
		b1, r1, r2, r3); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger(building_id,user_id,direction,amount,category,ref_type,memo)
		 VALUES ($1,NULL,'in',84000,'subscription','subscription','三户认购组件款入公共账户')`,
		b1); err != nil {
		return err
	}

	// 两台逆变器（同型号同供应商，5 年保修，2026-04 装机）
	var inv1, inv2 int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO inverters(building_id,name,capacity_kw,capacity_ratio,model,supplier,warranty_until,installed_at)
		 VALUES ($1,'1#逆变器',20,50,'SN-50KTL','华阳光伏','2031-04-14','2026-04-15') RETURNING id`,
		b1).Scan(&inv1); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO inverters(building_id,name,capacity_kw,capacity_ratio,model,supplier,warranty_until,installed_at)
		 VALUES ($1,'2#逆变器',20,50,'SN-50KTL','华阳光伏','2031-04-14','2026-04-15') RETURNING id`,
		b1).Scan(&inv2); err != nil {
		return err
	}
	// 设备绑定组件：按 50/50 装机比例，三户（24/20/12 块）与公共（24 块）各自一半接到两台逆变器
	bind := func(inv int64, uid any, panels int) {
		ratio := float64(panels) / 80 * 100
		if _, err := tx.Exec(ctx, `
			INSERT INTO panel_bindings(building_id,inverter_id,user_id,panel_count,ratio,created_by)
			VALUES ($1,$2,$3,$4,$5,$6)`, b1, inv, uid, panels, ratio, property); err != nil {
			panic(err)
		}
	}
	for _, inv := range []int64{inv1, inv2} {
		bind(inv, r1, 12) // 张伟 15%
		bind(inv, r2, 10) // 李娜 12.5%
		bind(inv, r3, 6)  // 王强 7.5%
		bind(inv, 0, 12)  // 公共部分 15%
	}
	for _, m := range []struct {
		month  string
		k1, k2 float64
	}{{"2026-08", 2050, 2100}, {"2026-09", 1680, 920}} {
		for _, inv := range []struct {
			id  int64
			kwh float64
		}{{inv1, m.k1}, {inv2, m.k2}} {
			if _, err := tx.Exec(ctx, `
				INSERT INTO generation_records(building_id,inverter_id,period_month,kwh,created_by)
				VALUES ($1,$2,$3,$4,$5)`, b1, inv.id, m.month, inv.kwh, property); err != nil {
				return err
			}
		}
	}
	// 9 月 2# 逆变器异常已有解释
	if _, err := tx.Exec(ctx, `
		INSERT INTO generation_alerts(building_id,inverter_id,period_month,reason,explanation,status,created_by,explained_at)
		VALUES ($1,$2,'2026-09','2#逆变器发电量环比下降 56%','9月12日逆变模块过热保护停机，厂家更换散热风扇，19日恢复发电，损失电量约 760kWh','explained',$3,now())`,
		b1, inv2, property); err != nil {
		return err
	}
	// 一条漏水投诉工单（已解决，物业责任 600 元——演示维修基金部分支付后按比例分摊）
	if _, err := tx.Exec(ctx, `
		INSERT INTO work_orders(building_id,inverter_id,period_month,title,description,wo_type,responsibility,cost,status,reporter_id,assignee,resolved_at)
		VALUES ($1,NULL,'2026-09','601 主卧顶板渗水','暴雨后顶板线管周边渗水，排查为支架穿层防水卷材老化','leak','property',600,'resolved',$2,'防水班组-周工',now())`,
		b1, r1); err != nil {
		return err
	}
	// 2# 逆变器 9 月停机故障：设备质量（保修期内），已测算+曲线复核，赔付尚未入账（留待演示）
	if err := seedOutageEvent(ctx, tx, b1, property, inv1, inv2, r1, r2, r3); err != nil {
		return err
	}
	// 8 月结算：用结算引擎真实定稿，并同步台账/住户余额/收付款记录
	if err := seedFinalizeAugust(ctx, tx, b1, property, r1, r2, r3); err != nil {
		return err
	}

	// 公告 + 异议投票
	if _, err := tx.Exec(ctx, `
		INSERT INTO announcements(building_id,title,content,pinned,author_id)
		VALUES ($1,'2026 年 8 月发电收益公示','本月总发电 4150 度，收入 1723.50 元，补贴 207.50 元，明细见结算页。',TRUE,$2)`,
		b1, property); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO polls(building_id,title,content,status,result,agree,disagree,created_by)
		VALUES ($1,'是否同意动用公共收益更新楼道照明','顶楼照明损坏 4 处，预算 1800 元，建议从公共可支配收益列支。','closed','approved',5,1,$2)`,
		b1, r1); err != nil {
		return err
	}

	// 楼栋 2：处于表决阶段，顶楼住户有漏水担忧
	var b2 int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO buildings(name,address,roof_area,orientation,load_assessment,shading,
			ownership_note,public_revenue_rule,grid_condition,construction_impact,
			total_capacity_kw,panel_total,unit_price,stage)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'voting') RETURNING id`,
		"幸福里 2 号楼", "幸福里小区 2 号", 298, "正南",
		"待第三方承重复核（初评满足）",
		"南侧有香樟树一棵，夏季清晨遮挡约 10%",
		"屋顶业主共有；待表决通过后签署四方协议",
		`{"maintenance_fund_ratio":20}`,
		"并网容量需电网公司复核，预计排队 2 个月",
		"预计占用电梯运输 3 天",
		30, 60, 1500).Scan(&b2); err != nil {
		return err
	}
	for _, uid := range []int64{property, r1, r2, r3, r4, r5} {
		mt := "resident"
		if uid == property {
			mt = "property"
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO building_members(building_id,user_id,member_type) VALUES ($1,$2,$3)`,
			b2, uid, mt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO votes(building_id,title,content,status,result,agree,disagree,abstain,created_by)
		 VALUES ($1,'2 号楼屋顶光伏立项表决','是否同意在本楼屋顶建设 30kW 分布式光伏？方案含顶楼防水二次处理承诺。','open','pending',3,1,0,$2)`,
		b2, property); err != nil {
		return err
	}
	var voteID int64
	if err := tx.QueryRow(ctx, `SELECT id FROM votes WHERE building_id=$1 ORDER BY id DESC LIMIT 1`, b2).Scan(&voteID); err != nil {
		return err
	}
	for _, c := range []struct {
		uid    int64
		choice string
	}{{r2, "agree"}, {r3, "agree"}, {r4, "agree"}, {r1, "disagree"}} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO vote_choices(vote_id,user_id,choice) VALUES ($1,$2,$3)`, voteID, c.uid, c.choice); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO announcements(building_id,title,content,author_id)
		VALUES ($1,'顶楼防水说明会通知','本周六 10 点在物业办召开，安装商将说明支架穿层防水工艺与 5 年质保。',$2)`,
		b2, property); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Println("seed data inserted")
	return nil
}

// seedOutageEvent 构造 2# 逆变器 9 月停机故障的完整演示数据：
// 日曲线（故障前/停机/修复后，合计与分表月读数 920kWh 一致）、按引擎测算的损失分摊、
// 曲线复核（恢复至基线，赔付覆盖真实损失）以及对同型号 1# 逆变器的复查提醒。赔付未入账，留作演示。
func seedOutageEvent(ctx context.Context, tx pgx.Tx, bid, propertyID, inv1, inv2, r1, r2, r3 int64) error {
	const gridPrice, subsidyRate = 0.4153, 0.05
	faultAt := time.Date(2026, 9, 12, 8, 0, 0, 0, time.Local)
	respondedAt := time.Date(2026, 9, 12, 14, 0, 0, 0, time.Local)
	resolvedAt := time.Date(2026, 9, 19, 20, 0, 0, 0, time.Local)

	// 曲线：9/1-9/11 正常（1 天 40 + 10 天 41 = 450）；9/12 停机前发 13.3；
	// 9/13-9/18 为零；9/19 修复后发 6.7；9/20-9/30 恢复（1 天 40 + 10 天 41 = 450）。合计 920。
	put := func(day string, inv int64, kwh float64, phase string) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO generation_curve_points(building_id,inverter_id,day,kwh,phase)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (building_id,inverter_id,day) DO UPDATE SET kwh=EXCLUDED.kwh, phase=EXCLUDED.phase`,
			bid, inv, day, kwh, phase); err != nil {
			panic(err)
		}
	}
	for d := 1; d <= 11; d++ {
		kwh := 41.0
		if d == 1 {
			kwh = 40
		}
		put(fmt.Sprintf("2026-09-%02d", d), inv2, kwh, "normal")
	}
	put("2026-09-12", inv2, 13.3, "outage")
	for d := 13; d <= 18; d++ {
		put(fmt.Sprintf("2026-09-%02d", d), inv2, 0, "outage")
	}
	put("2026-09-19", inv2, 6.7, "recovery")
	for d := 20; d <= 30; d++ {
		kwh := 41.0
		if d == 20 {
			kwh = 40
		}
		put(fmt.Sprintf("2026-09-%02d", d), inv2, kwh, "recovery")
	}

	bindings := []outage.Binding{
		{UserID: &r1, Name: "张伟", Ratio: 15, PanelCount: 12},
		{UserID: &r2, Name: "李娜", Ratio: 12.5, PanelCount: 10},
		{UserID: &r3, Name: "王强", Ratio: 7.5, PanelCount: 6},
		{Name: "公共部分", Ratio: 15, PanelCount: 12},
	}
	baseline := 450.0 / 11
	asm := outage.Assess(outage.Input{
		FaultAt: faultAt, RespondedAt: &respondedAt, ResolvedAt: &resolvedAt,
		AsOf: resolvedAt, ResponseSLAHours: 24,
		CauseType: outage.CauseQuality, UnderWarranty: true,
		BaselineKwhPerDay: baseline, GridPrice: gridPrice, SubsidyRate: subsidyRate,
		Bindings: bindings,
	})

	// 复核：基线与修复后各 11 个全天点，平均均为 450/11
	var basePts, recPts []outage.CurvePoint
	for d := 1; d <= 11; d++ {
		kwh := 41.0
		if d == 1 {
			kwh = 40
		}
		basePts = append(basePts, outage.CurvePoint{Day: time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC), Kwh: kwh})
	}
	for i, d := 0, 20; d <= 30; i, d = i+1, d+1 {
		kwh := 41.0
		if d == 20 {
			kwh = 40
		}
		recPts = append(recPts, outage.CurvePoint{Day: time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC), Kwh: kwh})
	}
	rv := outage.CompareCurve(outage.ReviewInput{
		BaselinePoints: basePts, RecoveryPoints: recPts,
		FallbackBaseline: baseline, EventLossAmount: asm.LossAmountTotal,
		SupplierPaid: asm.SupplierPaidTotal, PropertyPaid: asm.PropertyPaidTotal,
	}, gridPrice+subsidyRate)

	var eid int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO inverter_outage_events(building_id,inverter_id,title,period_month,
			fault_at,responded_at,resolved_at,response_sla_hours,cause_type,under_warranty,warranty_note,
			baseline_kwh_per_day,lost_kwh_total,lost_kwh_quality,lost_kwh_channel,lost_kwh_pending,
			grid_price,subsidy_per_kwh,loss_amount_total,supplier_paid_amount,property_paid_amount,
			recovery_baseline_avg,recovery_after_avg,recovery_rate,actual_extra_loss_kwh,
			cover_status,review_note,status,created_by,reviewed_by,reviewed_at)
		VALUES ($1,$2,'2#逆变器过热保护停机（散热风扇故障）','2026-09',
			$3,$4,$5,24,'quality',TRUE,'供应商 华阳光伏；保修至 2031-04-14（故障在保修期内）',
			$6,$7,$8,$9,$10,$11,$12,$13,$14,0,
			$15,$16,$17,0,$18,$19,'recovered',$20,$20,now()) RETURNING id`,
		bid, inv2, faultAt, respondedAt, resolvedAt,
		baseline, asm.LostKwhTotal, asm.LostKwhQuality, asm.LostKwhChannel, asm.LostKwhPending,
		gridPrice, subsidyRate, asm.LossAmountTotal, asm.SupplierPaidTotal,
		rv.BaselineAvg, rv.RecoveryAvg, rv.RecoveryRate, rv.CoverStatus, rv.Note, propertyID).Scan(&eid); err != nil {
		return err
	}
	for _, a := range asm.Allocations {
		uid := int64(0)
		if a.UserID != nil {
			uid = *a.UserID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO outage_allocations(event_id,user_id,ratio,panel_count,lost_kwh,
				loss_quality,loss_channel,loss_pending,loss_total,supplier_due,supplier_paid)
			VALUES ($1,$2,$3,$4,$5,$6,0,0,$7,$8,$9)`,
			eid, uid, a.Ratio, a.PanelCount, a.LostKwh, a.LossQuality, a.LossTotal,
			a.SupplierDue, a.SupplierPaid); err != nil {
			return err
		}
	}
	// 同型号 1# 逆变器复查提醒（质量故障批次复查）
	if _, err := tx.Exec(ctx, `
		INSERT INTO outage_rechecks(building_id,event_id,inverter_id,reason)
		VALUES ($1,$2,$3,'同型号（SN-50KTL）2#逆变器散热风扇质量故障并触发保修赔付，请安排批次复查散热模块')`,
		bid, eid, inv1); err != nil {
		return err
	}
	// 设备已恢复正常
	if _, err := tx.Exec(ctx,
		`UPDATE inverters SET status='normal', fault_note='', fault_at=NULL WHERE id=$1`, inv2); err != nil {
		return err
	}
	return nil
}

func seedFinalizeAugust(ctx context.Context, tx pgx.Tx, bid, propertyID, r1, r2, r3 int64) error {
	res := settlement.Compute(settlement.Input{
		BuildingID:  bid,
		PeriodMonth: "2026-08",
		TotalKwh:    4150,
		GridPrice:   0.4153,
		SubsidyRate: 0.05,
		OmFee:       200,
		Tax:         100,
		FundRatio:   20,
		Shareholders: []settlement.Share{
			{UserID: r1, Name: "张伟", Ratio: 30},
			{UserID: r2, Name: "李娜", Ratio: 25},
			{UserID: r3, Name: "王强", Ratio: 15},
		},
	})
	var pid int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO settlement_periods(building_id,period_month,status,total_kwh,grid_price,subsidy_per_kwh,
			gross_revenue,total_subsidy,om_fee,tax,work_order_cost,maintenance_fund_ratio,
			maintenance_fund_used,maintenance_fund_addition,compensation,public_ratio,
			distributable,created_by,finalized_at)
		VALUES ($1,$2,'finalized',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,now()) RETURNING id`,
		bid, res.PeriodMonth, res.TotalKwh, 0.4153, 0.05, res.GrossRevenue, res.TotalSubsidy,
		res.OmFee, res.Tax, res.WorkOrderCost, res.FundRatio, res.FundUsed, res.FundAddition,
		res.Compensation, res.PublicRatio, res.Distributable, propertyID).Scan(&pid); err != nil {
		return err
	}
	for _, it := range res.Items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO settlement_items(period_id,user_id,item_type,ratio,kwh,revenue,subsidy,om_share,tax_share,wo_share,amount,memo)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			pid, it.UserID, it.ItemType, it.Ratio, it.Kwh, it.Revenue, it.Subsidy,
			it.OmShare, it.TaxShare, it.WoShare, it.Amount, it.Memo); err != nil {
			return err
		}
	}
	led := func(uid any, dir, cat string, amount float64, memo string) error {
		if amount == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO ledger(building_id,user_id,direction,amount,category,period_month,ref_type,ref_id,memo)
			VALUES ($1,$2,$3,$4,$5,$6,'settlement',$7,$8)`,
			bid, uid, dir, amount, cat, res.PeriodMonth, pid, memo)
		return err
	}
	pay := func(ptype string, amount float64, memo string) error {
		if amount == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO payments(building_id,user_id,amount,pay_type,memo) VALUES ($1,NULL,$2,$3,$4)`,
			bid, amount, ptype, memo)
		return err
	}
	if err := pay("grid", res.GrossRevenue, "2026-08 电网购电收入"); err != nil {
		return err
	}
	if err := pay("subsidy", res.TotalSubsidy, "2026-08 发电补贴"); err != nil {
		return err
	}
	if err := led(nil, "in", "revenue", res.GrossRevenue, "2026-08 上网电费收入"); err != nil {
		return err
	}
	if err := led(nil, "in", "subsidy", res.TotalSubsidy, "2026-08 度电补贴"); err != nil {
		return err
	}
	if err := led(nil, "out", "om", res.OmFee, "2026-08 运维费用（全额）"); err != nil {
		return err
	}
	if err := led(nil, "out", "tax", res.Tax, "2026-08 税费（全额）"); err != nil {
		return err
	}
	if res.WorkOrderCost > 0 {
		if err := led(nil, "out", "repair", res.WorkOrderCost, "2026-08 维修费用（全额）"); err != nil {
			return err
		}
	}
	for _, it := range res.Items {
		switch it.ItemType {
		case "shareholder":
			if it.UserID == nil {
				continue
			}
			uid := *it.UserID
			if err := led(uid, "in", "revenue", it.Revenue, "2026-08 住户发电收益"); err != nil {
				return err
			}
			if err := led(uid, "in", "subsidy", it.Subsidy, "2026-08 住户补贴分成"); err != nil {
				return err
			}
			if err := led(uid, "out", "om", it.OmShare, "2026-08 运维费分摊"); err != nil {
				return err
			}
			if err := led(uid, "out", "tax", it.TaxShare, "2026-08 税费分摊"); err != nil {
				return err
			}
			if err := led(uid, "out", "repair", it.WoShare, "2026-08 维修费分摊（基金不足部分）"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET balance=balance+$2 WHERE id=$1`, uid, it.Amount); err != nil {
				return err
			}
		case "maintenance_fund":
			if res.FundAddition > 0 {
				if err := led(nil, "in", "maintenance_fund", res.FundAddition, "2026-08 公共收益计提维修基金（专款标记）"); err != nil {
					return err
				}
			}
			if res.FundUsed > 0 {
				if err := led(nil, "out", "maintenance_fund", res.FundUsed, "2026-08 动用维修基金（专款标记）"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
