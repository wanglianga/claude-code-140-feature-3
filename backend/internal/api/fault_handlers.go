package api

import (
	"context"
	"math"
	"net/http"
	"time"

	"pvplatform/internal/auth"
	"pvplatform/internal/fault"
)

// parseFaultTime 解析可选的 RFC3339 时间；空串返回零值
func parseFaultTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// createFault 登记逆变器停机故障（任何成员均可上报，物业随后响应/维修）
func (s *Server) createFault(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		InverterID       int64   `json:"inverter_id"`
		PeriodMonth      string  `json:"period_month"`
		Title            string  `json:"title"`
		Description      string  `json:"description"`
		FaultAt          string  `json:"fault_at"`
		BaselineDailyKwh float64 `json:"baseline_daily_kwh"`
		GridPrice        float64 `json:"grid_price"`
		SubsidyPerKwh    float64 `json:"subsidy_per_kwh"`
		SLAHours         float64 `json:"sla_response_hours"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Title == "" || in.PeriodMonth == "" || in.InverterID == 0 {
		writeErr(w, 400, "inverter_id、period_month、title 必填")
		return
	}
	if in.BaselineDailyKwh < 0 || in.GridPrice < 0 || in.SubsidyPerKwh < 0 {
		writeErr(w, 400, "基准电量/电价不能为负")
		return
	}
	// 校验设备归属
	var invBid int64
	err := s.Pool.QueryRow(r.Context(), `SELECT building_id FROM inverters WHERE id=$1`, in.InverterID).Scan(&invBid)
	if isNoRows(err) {
		writeErr(w, 404, "逆变器不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if invBid != bid {
		writeErr(w, 400, "设备不属于该楼栋")
		return
	}
	faultAt := time.Now()
	if t, ok := parseFaultTime(in.FaultAt); ok {
		faultAt = t
	}
	sla := in.SLAHours
	if sla <= 0 {
		sla = 24
	}

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	var fid int64
	err = tx.QueryRow(r.Context(), `
		INSERT INTO inverter_faults(building_id,inverter_id,period_month,title,description,
			fault_at,baseline_daily_kwh,grid_price,subsidy_per_kwh,sla_response_hours,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		bid, in.InverterID, in.PeriodMonth, in.Title, in.Description,
		faultAt, in.BaselineDailyKwh, in.GridPrice, in.SubsidyPerKwh, sla, u.ID).Scan(&fid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE inverters SET status='faulty', fault_note=$2, fault_at=$3 WHERE id=$1`,
		in.InverterID, in.Title, faultAt); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": fid})
}

// respondFault 运维到场响应
func (s *Server) respondFault(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	fid := urlID(r, "fid")
	var in struct {
		RespondedAt string `json:"responded_at"`
	}
	_ = decodeJSON(r, &in)
	respondedAt := time.Now()
	if t, ok := parseFaultTime(in.RespondedAt); ok {
		respondedAt = t
	}
	ct, err := s.Pool.Exec(r.Context(),
		`UPDATE inverter_faults SET responded_at=$2, status='responded'
		 WHERE id=$1 AND status='open'`, fid, respondedAt)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, 404, "故障不存在或已响应/修复")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// resolveFault 修复完成：计算停机时长、运维响应时长并判定 SLA，同步恢复设备
func (s *Server) resolveFault(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	fid := urlID(r, "fid")
	var in struct {
		ResolvedAt string `json:"resolved_at"`
	}
	_ = decodeJSON(r, &in)

	var invID int64
	var faultAt, respondedAt *time.Time
	var sla float64
	err := s.Pool.QueryRow(r.Context(),
		`SELECT inverter_id, fault_at, responded_at, sla_response_hours FROM inverter_faults WHERE id=$1`,
		fid).Scan(&invID, &faultAt, &respondedAt, &sla)
	if isNoRows(err) {
		writeErr(w, 404, "故障记录不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	resolvedAt := time.Now()
	if t, ok := parseFaultTime(in.ResolvedAt); ok {
		resolvedAt = t
	}
	if !resolvedAt.After(*faultAt) {
		writeErr(w, 400, "修复时间不能早于故障发生时间")
		return
	}
	downtime := resolvedAt.Sub(*faultAt).Hours()
	response := 0.0
	if respondedAt != nil {
		response = respondedAt.Sub(*faultAt).Hours()
		if response < 0 {
			response = 0
		}
	}
	slaMet := sla <= 0 || response <= sla

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `
		UPDATE inverter_faults
		SET status='resolved', resolved_at=$2, downtime_hours=$3, response_hours=$4, sla_met=$5
		WHERE id=$1`, fid, resolvedAt, round2go(downtime), round2go(response), slaMet); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE inverters SET status='normal', fault_note='', fault_at=NULL WHERE id=$1`, invID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "downtime_hours": math.Round(downtime*100) / 100,
		"response_hours": math.Round(response*100) / 100, "sla_met": slaMet,
	})
}

func round2go(x float64) float64 { return math.Round(x*100) / 100 }

// assessFault 责任判定与保修评估：计算受影响住户/公共损失明细与赔付归集
func (s *Server) assessFault(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	fid := urlID(r, "fid")
	var in struct {
		Cause              string   `json:"cause"`
		DelayHours         *float64 `json:"delay_hours"`
		WarrantyCovered    *bool    `json:"warranty_covered"`
		QualityCoveragePct float64  `json:"quality_coverage_pct"`
		WarrantyNote       string   `json:"warranty_note"`
		BaselineDailyKwh   *float64 `json:"baseline_daily_kwh"`
		GridPrice          *float64 `json:"grid_price"`
		SubsidyPerKwh      *float64 `json:"subsidy_per_kwh"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if in.Cause != "quality" && in.Cause != "channel" && in.Cause != "mixed" {
		writeErr(w, 400, "责任类型须为 quality（设备质量）/channel（维护通道占用）/mixed（混合）")
		return
	}

	// 读取故障与设备档案
	f, err := s.loadFault(r, fid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "故障记录不存在")
		return
	}
	if f["status"].(string) != "resolved" {
		writeErr(w, 409, "请先在维修完成后登记修复，再进行责任与保修评估")
		return
	}
	if f["paid"].(bool) {
		writeErr(w, 409, "该故障补偿已发放入账，责任与赔付不可再变更")
		return
	}
	bid := f["building_id"].(int64)
	invID := f["inverter_id"].(int64)
	downtime := num(f, "downtime_hours")
	if downtime <= 0 {
		writeErr(w, 409, "停机时长为 0，无法评估损失")
		return
	}

	delay := 0.0
	if in.DelayHours != nil {
		delay = *in.DelayHours
	}
	if delay < 0 || delay > downtime {
		writeErr(w, 400, "通道占用延迟时长须介于 0 与停机总时长之间")
		return
	}
	if in.Cause == "quality" {
		delay = 0
	}
	if in.Cause == "channel" {
		delay = downtime
	}

	// 设备档案（型号/供应商/保修条款）
	var model, supplier string
	var warrantyMonths int
	var installDate *time.Time
	if err := s.Pool.QueryRow(r.Context(),
		`SELECT COALESCE(model,''), COALESCE(supplier,''), COALESCE(warranty_months,0), install_date
		 FROM inverters WHERE id=$1`, invID).Scan(&model, &supplier, &warrantyMonths, &installDate); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	// 保修适用：显式传入优先；否则依据设备安装日期与保修月数自动认定
	warrantyCovered := false
	if in.WarrantyCovered != nil {
		warrantyCovered = *in.WarrantyCovered
	} else if (in.Cause == "quality" || in.Cause == "mixed") && installDate != nil && warrantyMonths > 0 {
		faultAt := f["fault_at"].(time.Time)
		warrantyUntil := installDate.AddDate(0, warrantyMonths, 0)
		warrantyCovered = faultAt.Before(warrantyUntil) || faultAt.Equal(warrantyUntil)
	}
	warrantyNote := in.WarrantyNote
	if warrantyNote == "" {
		switch {
		case warrantyCovered:
			warrantyNote = "故障发生在整机保修期内，质量时段损失由供应商按保修条款赔付"
		case in.Cause == "quality" || in.Cause == "mixed":
			warrantyNote = "不在保修范围（超保修期或档案缺失），质量损失暂记未覆盖，需另行追偿"
		default:
			warrantyNote = "维护通道占用导致的维修延迟，与设备保修无关，由物业承担"
		}
	}

	baseline := num(f, "baseline_daily_kwh")
	if in.BaselineDailyKwh != nil {
		baseline = *in.BaselineDailyKwh
	}
	gridPrice := num(f, "grid_price")
	if in.GridPrice != nil {
		gridPrice = *in.GridPrice
	}
	subsidy := num(f, "subsidy_per_kwh")
	if in.SubsidyPerKwh != nil {
		subsidy = *in.SubsidyPerKwh
	}
	if baseline <= 0 {
		writeErr(w, 400, "缺少故障前基准日均发电量，无法折算损失电量")
		return
	}

	// 当前认购份额（评估时快照固化，补偿按损失发生时的份额持有人发放）
	rows, err := s.Pool.Query(r.Context(), `
		SELECT s.user_id, u.name, s.panel_ratio
		FROM subscriptions s JOIN users u ON u.id=s.user_id
		WHERE s.building_id=$1 AND s.sub_type='shareholder' AND s.status IN ('paid','transferred')
		ORDER BY s.id`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var shares []fault.Share
	for rows.Next() {
		var sh fault.Share
		if err := rows.Scan(&sh.UserID, &sh.Name, &sh.Ratio); err != nil {
			rows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		shares = append(shares, sh)
	}
	rows.Close()

	res := fault.Assess(fault.Input{
		InverterID:         invID,
		InverterRatio:      num(f, "inverter_ratio"),
		PeriodMonth:        f["period_month"].(string),
		DowntimeHours:      downtime,
		ResponseHours:      num(f, "response_hours"),
		SLAHours:           num(f, "sla_response_hours"),
		DelayHours:         delay,
		Cause:              in.Cause,
		BaselineDailyKwh:   baseline,
		GridPrice:          gridPrice,
		SubsidyRate:        subsidy,
		WarrantyCovered:    warrantyCovered,
		QualityCoveragePct: in.QualityCoveragePct,
		Shareholders:       shares,
	})

	// 同型号设备复查清单（质量赔付触发时）
	var sameModelIDs []int64
	if res.ReviewNeeded && model != "" {
		mr, err := s.Pool.Query(r.Context(),
			`SELECT id FROM inverters WHERE building_id=$1 AND model=$2 AND id<>$3 ORDER BY id`,
			bid, model, invID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		for mr.Next() {
			var id int64
			if err := mr.Scan(&id); err != nil {
				mr.Close()
				writeErr(w, 500, err.Error())
				return
			}
			sameModelIDs = append(sameModelIDs, id)
		}
		mr.Close()
	}
	if sameModelIDs == nil {
		sameModelIDs = []int64{}
	}

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `
		UPDATE inverter_faults SET
			cause=$2, delay_hours=$3, quality_hours=$4,
			lost_kwh=$5, quality_lost_kwh=$6, channel_lost_kwh=$7, total_loss=$8,
			baseline_daily_kwh=$9, grid_price=$10, subsidy_per_kwh=$11,
			warranty_covered=$12, warranty_note=$13, quality_coverage_pct=$14,
			supplier_compensation=$15, property_compensation=$16,
			review_needed=$17, same_model_ids=$18, assessed=TRUE, assessed_at=now(), assessed_by=$19,
			compare_status='', compared_at=NULL, paid=FALSE, paid_at=NULL,
			review_done=FALSE, review_note='', reviewed_at=NULL
		WHERE id=$1`,
		fid, in.Cause, res.DelayHours, res.QualityHours,
		res.LostKwh, res.QualityLostKwh, res.ChannelLostKwh, res.TotalLoss,
		baseline, gridPrice, subsidy,
		warrantyCovered, warrantyNote, res.QualityCoveragePct,
		res.SupplierCompensation, res.PropertyCompensation,
		res.ReviewNeeded, sameModelIDs, u.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM fault_compensation_items WHERE fault_id=$1`, fid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, it := range res.Items {
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO fault_compensation_items(fault_id,user_id,party,ratio,lost_kwh,supplier_amount,property_amount)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			fid, it.UserID, it.Party, it.Ratio, it.LostKwh, it.SupplierAmount, it.PropertyAmount); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out, _ := s.faultDetail(r.Context(), fid)
	writeJSON(w, 200, out)
}

// compareFaultCurve 修复前后发电曲线对比：出力恢复率 + 赔付覆盖率
func (s *Server) compareFaultCurve(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	fid := urlID(r, "fid")
	var in struct {
		PostDailyKwh float64 `json:"post_daily_kwh"`
		Note         string  `json:"note"`
	}
	if err := decodeJSON(r, &in); err != nil || in.PostDailyKwh <= 0 {
		writeErr(w, 400, "post_daily_kwh（修复后观测期日均发电量）必填且为正")
		return
	}
	f, err := s.loadFault(r, fid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "故障记录不存在")
		return
	}
	if !f["assessed"].(bool) {
		writeErr(w, 409, "请先完成责任与保修评估，再对比修复前后曲线")
		return
	}
	cr := fault.CompareCurve(num(f, "baseline_daily_kwh"), in.PostDailyKwh,
		num(f, "total_loss"), num(f, "supplier_compensation")+num(f, "property_compensation"))
	note := in.Note
	if note == "" {
		note = cr.Note
	}
	if _, err := s.Pool.Exec(r.Context(), `
		UPDATE inverter_faults SET compare_status=$2, pre_daily_kwh=$3, post_daily_kwh=$4,
			recovery_ratio=$5, cover_ratio=$6, compare_note=$7, compared_at=now()
		WHERE id=$1`,
		fid, cr.Status, cr.PreDailyKwh, cr.PostDailyKwh, cr.RecoveryRatio, cr.CoverRatio, note); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out, _ := s.faultDetail(r.Context(), fid)
	writeJSON(w, 200, out)
}

// payFaultCompensation 发放补偿：
//   - 设备质量供应商赔付：住户部分直接计入住户收益余额，未认购公共组件部分入公共账户；
//   - 维护通道物业赔偿：先全额入楼栋公共账并出具说明，再按组件比例代发给住户，公共部分留存。
func (s *Server) payFaultCompensation(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	fid := urlID(r, "fid")
	f, err := s.loadFault(r, fid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "故障记录不存在")
		return
	}
	if !f["assessed"].(bool) {
		writeErr(w, 409, "尚未完成责任与保修评估，不能发放补偿")
		return
	}
	if f["paid"].(bool) {
		writeErr(w, 409, "该故障补偿已发放，不能重复入账")
		return
	}
	if f["compare_status"].(string) == "" {
		writeErr(w, 409, "请先对比修复前后发电曲线，确认补偿覆盖真实损失后再发放")
		return
	}
	if f["compare_status"].(string) == "insufficient" {
		writeErr(w, 409, "曲线对比结论为补偿不足，请先向责任方追偿或重新评估后再发放")
		return
	}

	bid := f["building_id"].(int64)
	month := f["period_month"].(string)
	invName := f["inverter_name"].(string)
	supplierTotal := num(f, "supplier_compensation")
	propertyTotal := num(f, "property_compensation")

	rows, err := s.Pool.Query(r.Context(), `
		SELECT user_id, party, lost_kwh, supplier_amount, property_amount
		FROM fault_compensation_items WHERE fault_id=$1 ORDER BY id`, fid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	type item struct {
		uid      *int64
		party    string
		lost     float64
		supplier float64
		property float64
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.uid, &it.party, &it.lost, &it.supplier, &it.property); err != nil {
			rows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		items = append(items, it)
	}
	rows.Close()

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	led := func(uid any, dir, cat string, amount float64, memo string) {
		if amount == 0 {
			return
		}
		if _, e := tx.Exec(r.Context(), `
			INSERT INTO ledger(building_id,user_id,direction,amount,category,period_month,ref_type,ref_id,memo)
			VALUES ($1,$2,$3,$4,$5,$6,'fault',$7,$8)`,
			bid, uid, dir, amount, cat, month, fid, memo); e != nil {
			err = e
		}
	}
	// 楼级收付款记录（供应商+物业赔付款到账）
	if total := round2go(supplierTotal + propertyTotal); total > 0 {
		if _, e := tx.Exec(r.Context(), `
			INSERT INTO payments(building_id,user_id,amount,pay_type,memo)
			VALUES ($1,NULL,$2,'compensation',$3)`,
			bid, total, month+" "+invName+" 停机故障赔付（供应商质量赔付/物业延迟赔偿）"); e != nil {
			writeErr(w, 500, e.Error())
			return
		}
	}

	// 物业赔偿先全额入公共账（楼栋公共账说明），再按明细代发住户
	led(nil, "in", "fault_comp", propertyTotal,
		month+" 物业责任赔偿：屋顶维护通道占用致维修延迟（入楼栋公共账）")
	// 供应商对未认购公共组件的赔付入公共账
	for _, it := range items {
		if it.party == "public" && it.supplier > 0 {
			led(nil, "in", "fault_comp", it.supplier,
				month+" 供应商质量赔付-未认购公共组件损失（"+invName+"）")
		}
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	for _, it := range items {
		if it.party != "shareholder" || it.uid == nil {
			continue
		}
		uid := *it.uid
		// 供应商赔付直接进入住户收益补偿（不经公共账）
		led(uid, "in", "fault_comp", it.supplier,
			month+" 供应商设备质量赔付（"+invName+"停机收益补偿）")
		// 物业赔偿由公共账代发住户
		if it.property > 0 {
			led(nil, "out", "fault_comp", it.property,
				month+" 公共账代发住户：维护通道延迟维修赔偿（"+invName+"）")
			led(uid, "in", "fault_comp", it.property,
				month+" 物业维修延迟赔偿（"+invName+"停机收益补偿）")
		}
		if amt := round2go(it.supplier + it.property); amt > 0 {
			if _, e := tx.Exec(r.Context(),
				`UPDATE users SET balance=balance+$2 WHERE id=$1`, uid, amt); e != nil {
				writeErr(w, 500, e.Error())
				return
			}
		}
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE inverter_faults SET paid=TRUE, paid_at=now() WHERE id=$1`, fid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out, _ := s.faultDetail(r.Context(), fid)
	writeJSON(w, 200, out)
}

// reviewSameModel 运维复查同型号设备并记录结论
func (s *Server) reviewSameModel(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	fid := urlID(r, "fid")
	var in struct {
		Note string `json:"note"`
		Done *bool  `json:"done"`
	}
	_ = decodeJSON(r, &in)
	var needed bool
	if err := s.Pool.QueryRow(r.Context(), `SELECT review_needed FROM inverter_faults WHERE id=$1`, fid).Scan(&needed); err != nil {
		writeErr(w, 404, "故障记录不存在")
		return
	}
	if !needed {
		writeErr(w, 409, "该故障无需复查同型号设备（非保修期内质量责任）")
		return
	}
	done := true
	if in.Done != nil {
		done = *in.Done
	}
	note := in.Note
	if note == "" {
		note = "运维已完成同型号设备巡检，未发现同类缺陷"
	}
	if _, err := s.Pool.Exec(r.Context(),
		`UPDATE inverter_faults SET review_done=$2, review_note=$3, reviewed_at=now() WHERE id=$1`,
		fid, done, note); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 查询 ----------

func num(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case float64:
			return x
		case float32:
			return float64(x)
		case int64:
			return float64(x)
		case int:
			return float64(x)
		}
	}
	return 0
}

// loadFault 读取单条故障（含设备信息），返回 nil 表示不存在
func (s *Server) loadFault(r *http.Request, fid int64) (map[string]any, error) {
	row := s.Pool.QueryRow(r.Context(), faultSelect+` WHERE f.id=$1`, fid)
	return scanFault(row)
}

const faultSelect = `
	SELECT f.id, f.building_id, f.inverter_id, COALESCE(i.name,''), COALESCE(i.model,''),
		COALESCE(i.supplier,''), COALESCE(i.warranty_months,0), i.install_date,
		COALESCE(i.capacity_ratio,0),
		f.period_month, f.title, f.description, f.status,
		f.fault_at, f.responded_at, f.resolved_at,
		f.downtime_hours, f.response_hours, f.sla_response_hours, f.sla_met,
		f.cause, f.delay_hours, f.quality_hours,
		f.baseline_daily_kwh, f.grid_price, f.subsidy_per_kwh,
		f.lost_kwh, f.quality_lost_kwh, f.channel_lost_kwh, f.total_loss,
		f.warranty_covered, f.warranty_note, COALESCE(f.quality_coverage_pct,100),
		f.supplier_compensation, f.property_compensation,
		f.assessed, f.assessed_at,
		f.compare_status, f.pre_daily_kwh, f.post_daily_kwh, f.recovery_ratio, f.cover_ratio,
		COALESCE(f.compare_note,''), f.compared_at,
		f.paid, f.paid_at, f.review_needed, f.same_model_ids, f.review_done,
		COALESCE(f.review_note,''), f.reviewed_at, f.created_at
	FROM inverter_faults f JOIN inverters i ON i.id=f.inverter_id`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanFault(row rowScanner) (map[string]any, error) {
	var id, bid, iid int64
	var invName, model, supplier, month, title, desc, status, cause, compareStatus string
	var warrantyNote, compareNote, reviewNote string
	var warrantyMonths int
	var installDate *time.Time
	var faultAt time.Time
	var respondedAt, resolvedAt, assessedAt, comparedAt, paidAt, reviewedAt, createdAt *time.Time
	var downtime, response, slaH, invRatio, delay, qualityH, baseline, gridP, subRate float64
	var lostKwh, qLost, cLost, totalLoss, covPct, supComp, propComp float64
	var preDaily, postDaily, recovery, cover float64
	var slaMet, warrantyCovered, assessed, paid, reviewNeeded, reviewDone bool
	var sameModelIDs []int64
	err := row.Scan(&id, &bid, &iid, &invName, &model, &supplier, &warrantyMonths, &installDate,
		&invRatio, &month, &title, &desc, &status,
		&faultAt, &respondedAt, &resolvedAt,
		&downtime, &response, &slaH, &slaMet,
		&cause, &delay, &qualityH,
		&baseline, &gridP, &subRate,
		&lostKwh, &qLost, &cLost, &totalLoss,
		&warrantyCovered, &warrantyNote, &covPct,
		&supComp, &propComp,
		&assessed, &assessedAt,
		&compareStatus, &preDaily, &postDaily, &recovery, &cover,
		&compareNote, &comparedAt,
		&paid, &paidAt, &reviewNeeded, &sameModelIDs, &reviewDone,
		&reviewNote, &reviewedAt, &createdAt)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return map[string]any{
		"id": id, "building_id": bid, "inverter_id": iid,
		"inverter_name": invName, "model": model, "supplier": supplier,
		"warranty_months": warrantyMonths, "install_date": installDate,
		"inverter_ratio": invRatio,
		"period_month":   month, "title": title, "description": desc, "status": status,
		"fault_at": faultAt, "responded_at": respondedAt, "resolved_at": resolvedAt,
		"downtime_hours": downtime, "response_hours": response,
		"sla_response_hours": slaH, "sla_met": slaMet,
		"cause": cause, "delay_hours": delay, "quality_hours": qualityH,
		"baseline_daily_kwh": baseline, "grid_price": gridP, "subsidy_per_kwh": subRate,
		"lost_kwh": lostKwh, "quality_lost_kwh": qLost, "channel_lost_kwh": cLost,
		"total_loss":       totalLoss,
		"warranty_covered": warrantyCovered, "warranty_note": warrantyNote,
		"quality_coverage_pct":  covPct,
		"supplier_compensation": supComp, "property_compensation": propComp,
		"assessed": assessed, "assessed_at": assessedAt,
		"compare_status": compareStatus, "pre_daily_kwh": preDaily, "post_daily_kwh": postDaily,
		"recovery_ratio": recovery, "cover_ratio": cover,
		"compare_note": compareNote, "compared_at": comparedAt,
		"paid": paid, "paid_at": paidAt,
		"review_needed": reviewNeeded, "same_model_ids": sameModelIDs,
		"review_done": reviewDone, "review_note": reviewNote, "reviewed_at": reviewedAt,
		"created_at": createdAt,
	}, nil
}

func (s *Server) listFaults(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), faultSelect+` WHERE f.building_id=$1 ORDER BY f.id DESC`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		m, err := scanFault(rows)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		// 同型号设备名称
		if ids, ok := m["same_model_ids"].([]int64); ok && len(ids) > 0 {
			names := []string{}
			rr, err := s.Pool.Query(r.Context(),
				`SELECT name FROM inverters WHERE id = ANY($1) ORDER BY id`, ids)
			if err == nil {
				for rr.Next() {
					var n string
					if rr.Scan(&n) == nil {
						names = append(names, n)
					}
				}
				rr.Close()
			}
			m["same_model_names"] = names
		} else {
			m["same_model_names"] = []string{}
		}
		out = append(out, m)
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) getFault(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	fid := urlID(r, "fid")
	out, err := s.faultDetail(r.Context(), fid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if out == nil {
		writeErr(w, 404, "故障记录不存在")
		return
	}
	writeJSON(w, 200, out)
}

// faultDetail 故障台账 + 受影响住户/公共赔付明细
func (s *Server) faultDetail(ctx context.Context, fid int64) (map[string]any, error) {
	row := s.Pool.QueryRow(ctx, faultSelect+` WHERE f.id=$1`, fid)
	m, err := scanFault(row)
	if err != nil || m == nil {
		return m, err
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT c.id, COALESCE(c.user_id,0), COALESCE(u.name,'公共部分（未认购组件）'),
			c.party, c.ratio, c.lost_kwh,
			ROUND(c.lost_kwh * (f.grid_price + f.subsidy_per_kwh), 2),
			c.supplier_amount, c.property_amount
		FROM fault_compensation_items c
		JOIN inverter_faults f ON f.id=c.fault_id
		LEFT JOIN users u ON u.id=c.user_id
		WHERE c.fault_id=$1
		ORDER BY CASE c.party WHEN 'shareholder' THEN 1 ELSE 2 END, c.id`, fid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id, uid int64
		var name, party string
		var ratio, lost, lossAmt, sup, prop float64
		if err := rows.Scan(&id, &uid, &name, &party, &ratio, &lost, &lossAmt, &sup, &prop); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{
			"id": id, "user_id": uid, "name": name, "party": party,
			"ratio": ratio, "lost_kwh": lost, "loss_amount": lossAmt,
			"supplier_amount": sup, "property_amount": prop,
			"total_compensation": round2go(sup + prop),
		})
	}
	m["items"] = items
	if ids, ok := m["same_model_ids"].([]int64); ok && len(ids) > 0 {
		names := []string{}
		rr, qErr := s.Pool.Query(ctx,
			`SELECT name FROM inverters WHERE id = ANY($1) ORDER BY id`, ids)
		if qErr == nil {
			for rr.Next() {
				var n string
				if rr.Scan(&n) == nil {
					names = append(names, n)
				}
			}
			rr.Close()
		}
		m["same_model_names"] = names
	} else {
		m["same_model_names"] = []string{}
	}
	return m, nil
}
