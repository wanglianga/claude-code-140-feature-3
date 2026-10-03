package api_test

import (
	"fmt"
	"testing"
)

// TestOutageApportionment 逆变器停机故障影响分摊全链路：
// 设备绑定 → 停机登记 → 质量/通道混合责任与 SLA 测算 → 供应商+物业赔付入账
// → 修复前后曲线复核（欠发预警）→ 同型号设备复查提醒 → 权限与锁定校验。
func TestOutageApportionment(t *testing.T) {
	c := setup(t)
	admin := c.login("13000000000", "admin123")
	registerResident(c, admin, "甲", "13200000001", "5-601")
	registerResident(c, admin, "乙", "13200000002", "5-501")
	tA, tB := c.login("13200000001", "pwd123"), c.login("13200000002", "pwd123")
	idA, idB := userID(c, admin, "13200000001"), userID(c, admin, "13200000002")

	st, out := c.do("POST", "/api/buildings", admin, map[string]any{
		"name": "停机测试楼", "total_capacity_kw": 30, "panel_total": 60, "unit_price": 1000,
	})
	if st != 201 {
		t.Fatalf("create building: %d %v", st, out)
	}
	bid := int64(out["id"].(float64))
	for _, id := range []int64{idA, idB} {
		if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/members", bid), admin,
			map[string]any{"user_id": id}); st != 201 {
			t.Fatalf("add member: %d %v", st, out)
		}
	}
	// 立项表决：两户都同意（2/2 >= 2/3）→ approved
	if st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
		map[string]string{"to_stage": "voting"}); st != 200 {
		t.Fatalf("to voting: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/votes", bid), admin,
		map[string]string{"title": "立项表决"})
	voteID := int64(out["id"].(float64))
	c.do("POST", fmt.Sprintf("/api/votes/%d/choose", voteID), tA, map[string]string{"choice": "agree"})
	c.do("POST", fmt.Sprintf("/api/votes/%d/choose", voteID), tB, map[string]string{"choice": "agree"})
	if st, out = c.do("POST", fmt.Sprintf("/api/votes/%d/close", voteID), admin, nil); st != 200 ||
		out["result"] != "approved" {
		t.Fatalf("表决应通过: %d %v", st, out)
	}
	for _, stage := range []string{"subscribing", "construction", "grid_acceptance", "operating"} {
		if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
			map[string]string{"to_stage": stage}); st != 200 {
			t.Fatalf("stage %s: %d %v", stage, st, out)
		}
	}

	// 两台同型号逆变器（同供应商，保修期内）
	mkInv := func(name string) int64 {
		st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/inverters", bid), admin, map[string]any{
			"name": name, "capacity_kw": 15, "model": "SN-50KTL", "supplier": "华阳光伏",
			"warranty_until": "2031-04-14", "installed_at": "2026-04-15",
		})
		if st != 201 {
			t.Fatalf("create inverter: %d %v", st, out)
		}
		return int64(out["id"].(float64))
	}
	inv1, inv2 := mkInv("1#"), mkInv("2#")

	// 2# 绑定：甲 30%（18 块）、乙 20%（12 块）→ 逆变器内归一化 60/40
	for _, x := range []struct {
		uid           int64
		panels, ratio float64
	}{{idA, 18, 30}, {idB, 12, 20}} {
		st, out = c.do("PUT", fmt.Sprintf("/api/buildings/%d/panel-bindings", bid), admin, map[string]any{
			"inverter_id": inv2, "user_id": x.uid, "panel_count": x.panels, "ratio": x.ratio,
		})
		if st != 200 {
			t.Fatalf("binding: %d %v", st, out)
		}
	}
	// 自动绑定在已有记录时应被拒绝
	if st, _ := c.do("POST", fmt.Sprintf("/api/buildings/%d/panel-bindings/auto", bid), admin, nil); st != 409 {
		t.Fatalf("已有绑定时自动生成应 409, got %d", st)
	}

	// 登记停机：故障 10-08 08:00，36h 后到场（超 SLA 12h），84h 后修复；
	// 通道 10-09 08:00 起被占用至修复 → 前 24h 质量，后 60h 通道
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/outages", bid), admin, map[string]any{
		"inverter_id": inv2, "title": "2#过热停机", "period_month": "2026-10",
		"fault_at": "2026-10-08 08:00", "responded_at": "2026-10-09 20:00",
		"resolved_at": "2026-10-11 20:00", "response_sla_hours": 24,
	})
	if st != 201 {
		t.Fatalf("create outage: %d %v", st, out)
	}
	oid := int64(out["id"].(float64))

	// 责任测算：质量（保修）+ 通道占用混合
	st, out = c.do("POST", fmt.Sprintf("/api/outages/%d/assess", oid), admin, map[string]any{
		"cause_type":           "mixed",
		"channel_blocked_at":   "2026-10-09 08:00",
		"channel_freed_at":     "2026-10-11 20:00",
		"baseline_kwh_per_day": 50, "grid_price": 0.4, "subsidy_per_kwh": 0.05,
	})
	if st != 200 {
		t.Fatalf("assess: %d %v", st, out)
	}
	asm := out["assessment"].(map[string]any)
	if asm["lost_kwh_quality"].(float64) != 50 || asm["lost_kwh_channel"].(float64) != 125 {
		t.Fatalf("分段电量 q=%v c=%v want 50/125", asm["lost_kwh_quality"], asm["lost_kwh_channel"])
	}
	if asm["supplier_due_total"].(float64) != 22.5 || asm["property_due_total"].(float64) != 56.25 {
		t.Fatalf("应赔 s=%v p=%v want 22.5/56.25", asm["supplier_due_total"], asm["property_due_total"])
	}
	if asm["within_sla"].(bool) || asm["sla_delay_hours"].(float64) != 12 {
		t.Fatalf("SLA 判定错误: %v", asm)
	}
	allocByUser := map[int64]map[string]any{}
	for _, it := range out["allocations"].([]any) {
		m := it.(map[string]any)
		allocByUser[int64(m["user_id"].(float64))] = m
	}
	aA, aB := allocByUser[idA], allocByUser[idB]
	// 甲 60%：供应商 13.50、物业 33.75
	if aA["supplier_due"].(float64) != 13.5 || aA["property_paid"].(float64) != 33.75 {
		t.Fatalf("甲分摊 supplier=%v property=%v want 13.5/33.75", aA["supplier_due"], aA["property_paid"])
	}
	// 乙 40%：供应商 9.00、物业 22.50
	if aB["supplier_due"].(float64) != 9 || aB["property_paid"].(float64) != 22.5 {
		t.Fatalf("乙分摊 supplier=%v property=%v want 9/22.5", aB["supplier_due"], aB["property_paid"])
	}
	// 住户无权测算
	if st, _ := c.do("POST", fmt.Sprintf("/api/outages/%d/assess", oid), tA, map[string]any{}); st != 403 {
		t.Fatalf("住户测算应 403, got %d", st)
	}

	// 赔付入账：甲余额 +47.25，乙 +31.50；无公共绑定，公共账余额不变
	if st, out := c.do("POST", fmt.Sprintf("/api/outages/%d/post-compensation", oid), admin, nil); st != 200 {
		t.Fatalf("post compensation: %d %v", st, out)
	}
	st, out = c.do("GET", "/api/me", tA, nil)
	if out["balance"].(string) != "47.25" {
		t.Fatalf("甲余额=%v want 47.25", out["balance"])
	}
	st, out = c.do("GET", "/api/me", tB, nil)
	if out["balance"].(string) != "31.50" {
		t.Fatalf("乙余额=%v want 31.50", out["balance"])
	}
	// 入账后锁定：禁止重算/重复入账
	if st, _ = c.do("POST", fmt.Sprintf("/api/outages/%d/assess", oid), admin, map[string]any{}); st != 409 {
		t.Fatalf("入账后重算应 409, got %d", st)
	}
	if st, _ = c.do("POST", fmt.Sprintf("/api/outages/%d/post-compensation", oid), admin, nil); st != 409 {
		t.Fatalf("重复入账应 409, got %d", st)
	}

	// 修复前后曲线复核：基线 50/天 ×7，修复后 40/天 ×7 → shortfall，extra 70kWh，
	// 并对同型号 1# 生成复查提醒
	for d := 1; d <= 7; d++ {
		c.do("PUT", fmt.Sprintf("/api/buildings/%d/generation-curve", bid), admin, map[string]any{
			"inverter_id": inv2, "day": fmt.Sprintf("2026-10-%02d", d), "kwh": 50, "phase": "normal",
		})
	}
	for d := 12; d <= 18; d++ {
		c.do("PUT", fmt.Sprintf("/api/buildings/%d/generation-curve", bid), admin, map[string]any{
			"inverter_id": inv2, "day": fmt.Sprintf("2026-10-%02d", d), "kwh": 40,
		})
	}
	st, out = c.do("POST", fmt.Sprintf("/api/outages/%d/review", oid), admin, nil)
	if st != 200 {
		t.Fatalf("review: %d %v", st, out)
	}
	rv := out["review"].(map[string]any)
	if rv["cover_status"].(string) != "shortfall" || rv["extra_loss_kwh"].(float64) != 70 {
		t.Fatalf("复核结论=%v extra=%v want shortfall/70", rv["cover_status"], rv["extra_loss_kwh"])
	}
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/rechecks", bid), admin, nil)
	if st != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("应生成 1 条同型号复查, got %d %v", len(out["items"].([]any)), out)
	}
	rc := out["items"].([]any)[0].(map[string]any)
	if int64(rc["inverter_id"].(float64)) != inv1 {
		t.Fatalf("复查对象应为同型号 1# 逆变器, got %v", rc["inverter_id"])
	}
	rid := int64(rc["id"].(float64))
	if st, out = c.do("POST", fmt.Sprintf("/api/rechecks/%d", rid), admin,
		map[string]string{"status": "done", "result": "复查散热模块正常"}); st != 200 {
		t.Fatalf("handle recheck: %d %v", st, out)
	}
	// 样本不足的复核应返回 pending（另建一个未修复事件验证）
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/outages", bid), admin, map[string]any{
		"inverter_id": inv1, "fault_at": "2026-10-20 08:00",
	})
	oid2 := int64(out["id"].(float64))
	c.do("POST", fmt.Sprintf("/api/outages/%d/timeline", oid2), admin, map[string]string{
		"resolved_at": "2026-10-21 08:00",
	})
	st, out = c.do("POST", fmt.Sprintf("/api/outages/%d/review", oid2), admin, nil)
	if st != 200 || out["review"].(map[string]any)["cover_status"].(string) != "pending" {
		t.Fatalf("无曲线样本应 pending: %d %v", st, out)
	}
}

// TestOutageBindingTransfer 设备绑定随房屋转让过户、随退购回归公共。
func TestOutageBindingTransfer(t *testing.T) {
	c := setup(t)
	admin := c.login("13000000000", "admin123")
	registerResident(c, admin, "甲", "13200000003", "6-601")
	registerResident(c, admin, "丙", "13200000004", "6-401")
	tA := c.login("13200000003", "pwd123")
	tC := c.login("13200000004", "pwd123")
	idA, idC := userID(c, admin, "13200000003"), userID(c, admin, "13200000004")

	st, out := c.do("POST", "/api/buildings", admin, map[string]any{
		"name": "绑定转让楼", "total_capacity_kw": 10, "panel_total": 20, "unit_price": 1000,
	})
	bid := int64(out["id"].(float64))
	for _, id := range []int64{idA, idC} {
		c.do("POST", fmt.Sprintf("/api/buildings/%d/members", bid), admin, map[string]any{"user_id": id})
	}
	// 表决通过并进入认购阶段
	if st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
		map[string]string{"to_stage": "voting"}); st != 200 {
		t.Fatalf("to voting: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/votes", bid), admin, map[string]string{"title": "立项"})
	voteID := int64(out["id"].(float64))
	c.do("POST", fmt.Sprintf("/api/votes/%d/choose", voteID), tA, map[string]string{"choice": "agree"})
	c.do("POST", fmt.Sprintf("/api/votes/%d/choose", voteID), tC, map[string]string{"choice": "agree"})
	if st, out = c.do("POST", fmt.Sprintf("/api/votes/%d/close", voteID), admin, nil); st != 200 ||
		out["result"] != "approved" {
		t.Fatalf("表决应通过: %d %v", st, out)
	}
	if st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
		map[string]string{"to_stage": "subscribing"}); st != 200 {
		t.Fatalf("to subscribing: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/inverters", bid), admin,
		map[string]any{"name": "1#", "capacity_kw": 10, "capacity_ratio": 100})
	inv := int64(out["id"].(float64))

	// 甲认购 50%（10 块）并缴费；手动绑定到逆变器
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/subscribe", bid), tA, map[string]any{
		"type": "shareholder", "ratio": 50,
	})
	sid := int64(out["id"].(float64))
	if st, out = c.do("POST", fmt.Sprintf("/api/subscriptions/%d/pay", sid), tA, nil); st != 200 {
		t.Fatalf("pay: %d %v", st, out)
	}
	if st, out = c.do("PUT", fmt.Sprintf("/api/buildings/%d/panel-bindings", bid), admin, map[string]any{
		"inverter_id": inv, "user_id": idA, "panel_count": 10, "ratio": 50,
	}); st != 200 {
		t.Fatalf("binding: %d %v", st, out)
	}

	// 权益随房转让给丙
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/transfers", bid), tA, map[string]any{
		"subscription_id": sid, "to_user_id": idC, "rights_transfer": true,
	})
	if st != 201 {
		t.Fatalf("transfer: %d %v", st, out)
	}
	tid := int64(out["id"].(float64))
	if st, out = c.do("POST", fmt.Sprintf("/api/transfers/%d/confirm", tid), admin, nil); st != 200 {
		t.Fatalf("confirm: %d %v", st, out)
	}
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/panel-bindings", bid), admin, nil)
	if st != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("转让后应仍只有 1 条绑定: %v", out["items"])
	}
	b := out["items"].([]any)[0].(map[string]any)
	if int64(b["user_id"].(float64)) != idC || b["panel_count"].(float64) != 10 {
		t.Fatalf("绑定应过户给丙: %v", b)
	}
}
