package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"pvplatform/internal/api"
	"pvplatform/internal/auth"
	"pvplatform/internal/db"
)

// adminURL 指向用于创建/删除测试库的维护连接
const adminURL = "postgres://pvapp@127.0.0.1:5433/pvplatform?sslmode=disable"
const testURL = "postgres://pvapp@127.0.0.1:5433/pvplatform_itest?sslmode=disable"

type client struct {
	t      *testing.T
	srv    *httptest.Server
	tokens map[string]string
}

func (c *client) do(method, path, token string, body any) (int, map[string]any) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func (c *client) login(phone, pwd string) string {
	st, out := c.do("POST", "/api/auth/login", "", map[string]string{"phone": phone, "password": pwd})
	if st != 200 {
		c.t.Fatalf("login %s: %d %v", phone, st, out)
	}
	return out["token"].(string)
}

func setup(t *testing.T) *client {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Skipf("测试数据库不可用，跳过集成测试: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("测试数据库不可用，跳过集成测试: %v", err)
	}
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS pvplatform_itest WITH (FORCE)`); err != nil {
		t.Skipf("无法重建测试库: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE pvplatform_itest`); err != nil {
		t.Skipf("无法创建测试库: %v", err)
	}

	pool, err := pgxpool.New(ctx, testURL)
	if err != nil {
		t.Skipf("连接测试库失败: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 直接建一个管理员
	hash, _ := auth.HashPassword("admin123")
	if _, err := pool.Exec(ctx,
		`INSERT INTO users(name,phone,role,password_hash) VALUES ('管理员','13000000000','admin',$1)`, hash); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(api.NewServer(pool, "test-secret").Router())
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv, tokens: map[string]string{}}
}

func registerResident(c *client, adminToken, name, phone, unit string) {
	st, out := c.do("POST", "/api/auth/register", adminToken,
		map[string]string{"name": name, "phone": phone, "unit_no": unit, "password": "pwd123", "role": "resident"})
	if st != 201 {
		c.t.Fatalf("register %s: %d %v", phone, st, out)
	}
}

func userID(c *client, adminToken, phone string) int64 {
	st, out := c.do("GET", "/api/users?q="+phone, adminToken, nil)
	if st != 200 {
		c.t.Fatalf("users: %d", st)
	}
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if m["phone"] == phone {
			return int64(m["id"].(float64))
		}
	}
	c.t.Fatalf("user %s not found", phone)
	return 0
}

func TestFullLifecycle(t *testing.T) {
	c := setup(t)
	admin := c.login("13000000000", "admin123")

	// 三户居民
	registerResident(c, admin, "甲", "13100000001", "3-601")
	registerResident(c, admin, "乙", "13100000002", "3-501")
	registerResident(c, admin, "丙(新业主)", "13100000003", "3-401")
	tA, tB := c.login("13100000001", "pwd123"), c.login("13100000002", "pwd123")
	idA, idB, idC := userID(c, admin, "13100000001"), userID(c, admin, "13100000002"), userID(c, admin, "13100000003")

	// 建档
	st, out := c.do("POST", "/api/buildings", admin, map[string]any{
		"name": "测试楼", "total_capacity_kw": 30, "panel_total": 60,
		"unit_price": 1500, "maintenance_fund_ratio": 20,
	})
	if st != 201 {
		t.Fatalf("create building: %d %v", st, out)
	}
	bid := int64(out["id"].(float64))
	for _, id := range []int64{idA, idB, idC} {
		if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/members", bid), admin,
			map[string]any{"user_id": id}); st != 201 {
			t.Fatalf("add member %d: %d %v", id, st, out)
		}
	}

	// 进入表决 → 全员同意 → 关表通过 → 进入认购
	if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
		map[string]string{"to_stage": "voting"}); st != 200 {
		t.Fatalf("to voting: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/votes", bid), admin,
		map[string]string{"title": "立项表决"})
	if st != 201 {
		t.Fatalf("create vote: %d", st)
	}
	voteID := int64(out["id"].(float64))
	// 第一次仅甲 1 票同意（1/3 < 2/3）→ rejected
	if st, out := c.do("POST", fmt.Sprintf("/api/votes/%d/choose", voteID), tA,
		map[string]string{"choice": "agree"}); st != 200 {
		t.Fatalf("vote: %d %v", st, out)
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/votes/%d/choose", voteID), tB,
		map[string]string{"choice": "disagree"}); st != 200 {
		t.Fatalf("vote: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/votes/%d/close", voteID), admin, nil)
	if st != 200 || out["result"] != "rejected" {
		t.Fatalf("第一次表决应为 rejected, got %v", out)
	}
	// 沟通后重新表决，3 户全部同意 → approved
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/votes", bid), admin,
		map[string]string{"title": "立项表决(二次)"})
	voteID = int64(out["id"].(float64))
	tC := c.login("13100000003", "pwd123")
	for _, tk := range []string{tA, tB, tC} {
		c.do("POST", fmt.Sprintf("/api/votes/%d/choose", voteID), tk, map[string]string{"choice": "agree"})
	}
	st, out = c.do("POST", fmt.Sprintf("/api/votes/%d/close", voteID), admin, nil)
	if out["result"] != "approved" {
		t.Fatalf("二次表决应 approved, got %v", out)
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
		map[string]string{"to_stage": "subscribing"}); st != 200 {
		t.Fatalf("to subscribing: %d %v", st, out)
	}

	// 认购 甲 40%、乙 30%，缴费
	for _, x := range []struct {
		tok   string
		ratio float64
	}{{tA, 40}, {tB, 30}} {
		st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/subscribe", bid), x.tok,
			map[string]any{"type": "shareholder", "ratio": x.ratio})
		if st != 201 {
			t.Fatalf("subscribe: %d %v", st, out)
		}
		sid := int64(out["id"].(float64))
		if st, out := c.do("POST", fmt.Sprintf("/api/subscriptions/%d/pay", sid), x.tok, nil); st != 200 {
			t.Fatalf("pay: %d %v", st, out)
		}
	}
	// 超额认购应被拒
	st, _ = c.do("POST", fmt.Sprintf("/api/buildings/%d/subscribe", bid), tC,
		map[string]any{"type": "shareholder", "ratio": 40})
	if st != 409 {
		t.Fatalf("超额认购应 409, got %d", st)
	}

	// 推进至运行（施工→并网验收→运行）
	for _, stage := range []string{"construction", "grid_acceptance", "operating"} {
		if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
			map[string]string{"to_stage": stage}); st != 200 {
			t.Fatalf("to %s: %d %v", stage, st, out)
		}
	}

	// 设备与发电量
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/inverters", bid), admin,
		map[string]any{"name": "1#", "capacity_kw": 15})
	inv1 := int64(out["id"].(float64))
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/inverters", bid), admin,
		map[string]any{"name": "2#", "capacity_kw": 15})
	inv2 := int64(out["id"].(float64))
	for _, x := range []struct {
		inv int64
		kwh float64
	}{{inv1, 1000}, {inv2, 800}} {
		if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/generation", bid), admin, map[string]any{
			"period_month": "2026-09", "inverter_id": x.inv, "kwh": x.kwh,
		}); st != 200 {
			t.Fatalf("generation: %d %v", st, out)
		}
	}
	// 漏水工单：物业责任 500 元，已解决
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/work-orders", bid), tA, map[string]any{
		"period_month": "2026-09", "title": "顶楼渗水", "wo_type": "leak",
		"description": "穿层处渗水", "cost": 500,
	})
	if st != 201 {
		t.Fatalf("work order: %d %v", st, out)
	}
	woID := int64(out["id"].(float64))
	if st, out := c.do("POST", fmt.Sprintf("/api/work-orders/%d", woID), admin, map[string]any{
		"status": "resolved", "responsibility": "property", "cost": 500,
	}); st != 200 {
		t.Fatalf("wo update: %d %v", st, out)
	}

	// 生成 9 月结算（草稿）→ 校验收支 → 定稿
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/settlements", bid), admin, map[string]any{
		"period_month": "2026-09", "grid_price": 0.4, "subsidy_per_kwh": 0.05,
		"om_fee": 100, "tax": 50,
	})
	if st != 200 {
		t.Fatalf("settlement create: %d %v", st, out)
	}
	spID := int64(out["id"].(float64))
	settle := out["settlement"].(map[string]any)
	if settle["work_order_cost"].(float64) != 500 {
		t.Fatalf("wo cost = %v", settle["work_order_cost"])
	}
	if settle["fund_used"].(float64) != 0 || settle["repair_deduct"].(float64) != 500 {
		t.Fatalf("基金为 0 时维修应全额分摊: %v", settle)
	}
	st, out = c.do("GET", fmt.Sprintf("/api/settlements/%d", spID), admin, nil)
	if st != 200 {
		t.Fatalf("get settlement: %d", st)
	}
	var holderA float64
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if m["item_type"] == "shareholder" && int64(m["user_id"].(float64)) == idA {
			holderA = m["amount"].(float64)
		}
	}
	// 甲 40%: 288 + 36 - 40 - 20 - 200 = 64
	if holderA != 64 {
		t.Fatalf("甲结算金额=%v want 64; items=%v", holderA, out["items"])
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/settlements/%d/finalize", spID), admin, nil); st != 200 {
		t.Fatalf("finalize: %d %v", st, out)
	}
	// 定稿后禁止重算/再改工单责任
	if st, _ = c.do("POST", fmt.Sprintf("/api/buildings/%d/settlements", bid), admin, map[string]any{
		"period_month": "2026-09", "grid_price": 0.5,
	}); st != 409 {
		t.Fatalf("已定稿重算应 409, got %d", st)
	}
	if st, _ = c.do("POST", fmt.Sprintf("/api/work-orders/%d", woID), admin,
		map[string]any{"responsibility": "owner"}); st != 409 {
		t.Fatalf("已定稿周期改责任应 409, got %d", st)
	}
	// 甲余额 = 64
	st, out = c.do("GET", "/api/me", tA, nil)
	if out["balance"].(string) != "64.00" {
		t.Fatalf("甲余额=%v want 64.00", out["balance"])
	}
	// 维修基金 = 公共净收益 48 × 20% = 9.60
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/ledger", bid), admin, nil)
	if out["maintenance_fund_balance"].(float64) != 9.6 {
		t.Fatalf("维修基金=%v want 9.6", out["maintenance_fund_balance"])
	}

	// ===== 逆变器故障影响分摊 =====
	// 两台设备登记同型号档案（保修期 60 个月，2026-04 安装）
	for _, inv := range []int64{inv1, inv2} {
		if st, out := c.do("PATCH", fmt.Sprintf("/api/inverters/%d", inv), admin, map[string]any{
			"model": "SUN15K", "supplier": "晶能电气",
			"warranty_months": 60, "install_date": "2026-04-01",
		}); st != 200 {
			t.Fatalf("设备档案更新: %d %v", st, out)
		}
	}
	// inv2 质量故障：10/8 8:00 停机，4 小时到场（24h SLA 内），10/9 8:00 修复（停机 24h）
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/faults", bid), tA, map[string]any{
		"inverter_id": inv2, "period_month": "2026-10",
		"title": "逆变模块过热停机", "baseline_daily_kwh": 60,
		"grid_price": 0.4, "subsidy_per_kwh": 0.05,
		"fault_at": "2026-10-08T08:00:00+08:00",
	})
	if st != 201 {
		t.Fatalf("登记故障: %d %v", st, out)
	}
	faultID := int64(out["id"].(float64))
	// 未修复先评估应 409
	if st, _ = c.do("POST", fmt.Sprintf("/api/faults/%d/assess", faultID), admin,
		map[string]any{"cause": "quality"}); st != 409 {
		t.Fatalf("未修复评估应 409, got %d", st)
	}
	if st, _ = c.do("POST", fmt.Sprintf("/api/faults/%d/respond", faultID), admin,
		map[string]any{"responded_at": "2026-10-08T12:00:00+08:00"}); st != 200 {
		t.Fatalf("运维响应: %d", st)
	}
	if st, out = c.do("POST", fmt.Sprintf("/api/faults/%d/resolve", faultID), admin,
		map[string]any{"resolved_at": "2026-10-09T08:00:00+08:00"}); st != 200 {
		t.Fatalf("修复: %d %v", st, out)
	}
	if out["sla_met"] != true || out["downtime_hours"].(float64) != 24 || out["response_hours"].(float64) != 4 {
		t.Fatalf("停机/响应时长或 SLA 错误: %v", out)
	}
	// 住户无权评估责任（403）
	if st, _ = c.do("POST", fmt.Sprintf("/api/faults/%d/assess", faultID), tA,
		map[string]any{"cause": "quality"}); st != 403 {
		t.Fatalf("住户评估责任应 403, got %d", st)
	}
	// 责任与保修评估（保修按设备档案自动认定）
	st, out = c.do("POST", fmt.Sprintf("/api/faults/%d/assess", faultID), admin,
		map[string]any{"cause": "quality"})
	if st != 200 {
		t.Fatalf("质量责任评估: %d %v", st, out)
	}
	if out["warranty_covered"] != true || out["lost_kwh"].(float64) != 60 {
		t.Fatalf("保修认定/损失电量错误: %v", out)
	}
	if out["supplier_compensation"].(float64) != 27 || out["property_compensation"].(float64) != 0 {
		t.Fatalf("供应商应赔 27 元（60×0.45）: %v", out)
	}
	if out["review_needed"] != true {
		t.Fatalf("质量赔付应触发同型号复查提醒")
	}
	// 住户赔付明细：甲 40%=10.80、乙 30%=8.10，公共 30%=8.10（合计 27.00）
	itemAmt := map[int64]float64{}
	var pubSup float64
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if m["party"] == "public" {
			pubSup = m["supplier_amount"].(float64)
		} else {
			itemAmt[int64(m["user_id"].(float64))] = m["supplier_amount"].(float64)
		}
	}
	if itemAmt[idA] != 10.8 || itemAmt[idB] != 8.1 || pubSup != 8.1 {
		t.Fatalf("住户/公共赔付明细错误: %v pub=%v", itemAmt, pubSup)
	}
	// 未对比曲线先发放 → 409；出力未恢复（恢复率<95%）→ 仍 409
	if st, _ = c.do("POST", fmt.Sprintf("/api/faults/%d/pay", faultID), admin, nil); st != 409 {
		t.Fatalf("未对比曲线发放应 409, got %d", st)
	}
	if st, out = c.do("POST", fmt.Sprintf("/api/faults/%d/compare-curve", faultID), admin,
		map[string]any{"post_daily_kwh": 50}); st != 200 || out["compare_status"] != "insufficient" {
		t.Fatalf("恢复率 83%% 应 insufficient: %d %v", st, out)
	}
	if st, _ = c.do("POST", fmt.Sprintf("/api/faults/%d/pay", faultID), admin, nil); st != 409 {
		t.Fatalf("补偿不足时发放应 409, got %d", st)
	}
	// 复测恢复后对比通过并发放
	if st, out = c.do("POST", fmt.Sprintf("/api/faults/%d/compare-curve", faultID), admin,
		map[string]any{"post_daily_kwh": 59}); st != 200 || out["compare_status"] != "covered" {
		t.Fatalf("恢复率 98.33%% 应 covered: %d %v", st, out)
	}
	if st, _ = c.do("POST", fmt.Sprintf("/api/faults/%d/pay", faultID), admin, nil); st != 200 {
		t.Fatalf("发放供应商赔付: %d", st)
	}
	// 甲余额 64 + 10.80 = 74.80
	st, out = c.do("GET", "/api/me", tA, nil)
	if out["balance"].(string) != "74.80" {
		t.Fatalf("甲余额含质量赔付=%v want 74.80", out["balance"])
	}
	if st, _ = c.do("POST", fmt.Sprintf("/api/faults/%d/pay", faultID), admin, nil); st != 409 {
		t.Fatalf("重复发放应 409, got %d", st)
	}
	// 同型号复查：通道类故障无需复查（409）；质量故障复查成功
	if st, _ = c.do("POST", fmt.Sprintf("/api/faults/%d/review", faultID), admin,
		map[string]any{"note": "巡检同型号设备正常"}); st != 200 {
		t.Fatalf("同型号复查登记: %d", st)
	}

	// inv1 通道占用责任故障：物业赔偿入公共账后代发住户
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/faults", bid), tA, map[string]any{
		"inverter_id": inv1, "period_month": "2026-10",
		"title": "屋顶通道被杂物占用无法进场", "baseline_daily_kwh": 60,
		"grid_price": 0.4, "subsidy_per_kwh": 0.05,
		"fault_at": "2026-10-15T08:00:00+08:00",
	})
	chFault := int64(out["id"].(float64))
	c.do("POST", fmt.Sprintf("/api/faults/%d/respond", chFault), admin, nil)
	c.do("POST", fmt.Sprintf("/api/faults/%d/resolve", chFault), admin,
		map[string]any{"resolved_at": "2026-10-16T08:00:00+08:00"})
	st, out = c.do("POST", fmt.Sprintf("/api/faults/%d/assess", chFault), admin,
		map[string]any{"cause": "channel"})
	if st != 200 || out["supplier_compensation"].(float64) != 0 || out["property_compensation"].(float64) != 27 {
		t.Fatalf("通道责任物业应赔 27 元: %d %v", st, out)
	}
	if out["review_needed"] != false {
		t.Fatalf("通道故障 review_needed 应为 false, got %v", out["review_needed"])
	}
	c.do("POST", fmt.Sprintf("/api/faults/%d/compare-curve", chFault), admin,
		map[string]any{"post_daily_kwh": 60})
	if st, _ = c.do("POST", fmt.Sprintf("/api/faults/%d/review", chFault), admin, nil); st != 409 {
		t.Fatalf("通道故障无需复查, /review 应 409, got %d", st)
	}
	if st, _ = c.do("POST", fmt.Sprintf("/api/faults/%d/pay", chFault), admin, nil); st != 200 {
		t.Fatalf("发放物业赔偿: %d", st)
	}
	// 乙余额 = 9月分成48 + 质量赔付8.10 + 通道赔付8.10 = 64.20
	st, out = c.do("GET", "/api/me", tB, nil)
	if out["balance"].(string) != "64.20" {
		t.Fatalf("乙余额=%v want 64.20", out["balance"])
	}

	// 房屋转让：甲把认购权益随房转给丙
	var subA int64
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/subscriptions", bid), admin, nil)
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if int64(m["user_id"].(float64)) == idA {
			subA = int64(m["id"].(float64))
		}
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/transfers", bid), tA, map[string]any{
		"subscription_id": subA, "to_user_id": idC, "rights_transfer": true,
	})
	if st != 201 {
		t.Fatalf("transfer create: %d %v", st, out)
	}
	trID := int64(out["id"].(float64))
	if st, out := c.do("POST", fmt.Sprintf("/api/transfers/%d/confirm", trID), admin, nil); st != 200 {
		t.Fatalf("transfer confirm: %d %v", st, out)
	}

	// 10 月发电再结算：40% 份额应归丙，甲不再出现在住户明细
	for _, x := range []struct {
		inv int64
		kwh float64
	}{{inv1, 600}, {inv2, 600}} {
		c.do("POST", fmt.Sprintf("/api/buildings/%d/generation", bid), admin, map[string]any{
			"period_month": "2026-10", "inverter_id": x.inv, "kwh": x.kwh,
		})
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/settlements", bid), admin, map[string]any{
		"period_month": "2026-10", "grid_price": 0.4,
	})
	if st != 200 {
		t.Fatalf("oct settlement: %d %v", st, out)
	}
	octID := int64(out["id"].(float64))
	st, out = c.do("GET", fmt.Sprintf("/api/settlements/%d", octID), admin, nil)
	holders := map[int64]float64{}
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if m["item_type"] == "shareholder" {
			holders[int64(m["user_id"].(float64))] = m["amount"].(float64)
		}
	}
	if _, ok := holders[idA]; ok {
		t.Fatalf("甲已转让，不应再有 10 月分成: %v", holders)
	}
	// 丙 40%: 1200*0.4*0.4 = 192（无补贴无运维无维修）
	if holders[idC] != 192 {
		t.Fatalf("丙 10 月分成=%v want 192", holders[idC])
	}
	c.do("POST", fmt.Sprintf("/api/settlements/%d/finalize", octID), admin, nil)

	// 公告、异议投票、异常解释
	if st, _ := c.do("POST", fmt.Sprintf("/api/buildings/%d/announcements", bid), admin,
		map[string]string{"title": "月度公示", "content": "见结算", "pinned": "x"}); st != 201 {
		// pinned 传字符串会被 bool 解析失败 → 改成 bool 再试
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/announcements", bid), admin,
		map[string]any{"title": "月度公示", "content": "见结算", "pinned": true}); st != 201 {
		t.Fatalf("announcement: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/polls", bid), tB,
		map[string]string{"title": "异议：收益偏低", "content": "要求复核"})
	pollID := int64(out["id"].(float64))
	c.do("POST", fmt.Sprintf("/api/polls/%d/vote", pollID), tA, map[string]bool{"support": true})
	c.do("POST", fmt.Sprintf("/api/polls/%d/vote", pollID), tB, map[string]bool{"support": true})
	st, out = c.do("POST", fmt.Sprintf("/api/polls/%d/close", pollID), admin, nil)
	if out["result"] != "approved" {
		t.Fatalf("异议投票应成立: %v", out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/alerts", bid), tA, map[string]any{
		"period_month": "2026-10", "inverter_id": inv2, "reason": "2# 停机",
	})
	alertID := int64(out["id"].(float64))
	if st, out := c.do("POST", fmt.Sprintf("/api/alerts/%d/explain", alertID), admin,
		map[string]string{"explanation": "电网检修限发 2 天"}); st != 200 {
		t.Fatalf("explain: %d %v", st, out)
	}

	// 年度报告
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/annual-reports", bid), admin,
		map[string]int{"year": 2026})
	if st != 200 {
		t.Fatalf("annual report: %d %v", st, out)
	}
	rid := int64(out["id"].(float64))
	summary := out["summary"].(map[string]any)
	if summary["finalized_months"].(float64) != 2 {
		t.Fatalf("年报月份数=%v", summary["finalized_months"])
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/annual-reports/%d/publish", rid), admin, nil); st != 200 {
		t.Fatalf("publish: %d %v", st, out)
	}

	// 仪表盘
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/dashboard", bid), admin, nil)
	if st != 200 || out["total_kwh"].(float64) != 3000 {
		t.Fatalf("dashboard: %d %v", st, out)
	}

	// 阶段回退必须有原因
	if st, _ = c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
		map[string]string{"to_stage": "construction"}); st != 409 {
		t.Fatalf("无原因回退应 409, got %d", st)
	}
	if st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
		map[string]string{"to_stage": "construction", "reason": "设备供应延迟，组件到货推迟"}); st != 200 {
		t.Fatalf("有原因回退应 200: %d %v", st, out)
	}
}
