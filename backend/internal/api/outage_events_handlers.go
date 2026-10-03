package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pvplatform/internal/auth"
	"pvplatform/internal/outage"
)

// ---------- 停机事件 ----------

func (s *Server) createOutage(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in map[string]any
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	iid := int64(numArg(in, "inverter_id"))
	if iid == 0 {
		writeErr(w, 400, "inverter_id 必填")
		return
	}
	var invBuilding int64
	var invName string
	if err := s.Pool.QueryRow(r.Context(),
		`SELECT building_id, name FROM inverters WHERE id=$1`, iid).Scan(&invBuilding, &invName); err != nil || invBuilding != bid {
		writeErr(w, 404, "逆变器不存在或不属于该楼栋")
		return
	}
	faultAt, ferr := timeArg(in, "fault_at")
	if ferr != nil || faultAt == nil {
		t := time.Now()
		faultAt = &t
	}
	periodMonth := strArg(in, "period_month")
	if periodMonth == "" {
		periodMonth = faultAt.Format("2006-01")
	}
	title := strArg(in, "title")
	if title == "" {
		title = invName + "停机故障"
	}
	sla := numArg(in, "response_sla_hours")
	if sla <= 0 {
		sla = 24
	}
	var workOrderID *int64
	if wid := int64(numArg(in, "work_order_id")); wid != 0 {
		workOrderID = &wid
	}
	respondedAt, err := timeArg(in, "responded_at")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	resolvedAt, err := timeArg(in, "resolved_at")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	var id int64
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO inverter_outage_events(building_id,inverter_id,work_order_id,title,period_month,
			fault_at,responded_at,resolved_at,response_sla_hours,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		bid, iid, workOrderID, title, periodMonth, faultAt, respondedAt, resolvedAt, sla, u.ID).Scan(&id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE inverters SET status='faulty',
			fault_note=CASE WHEN fault_note='' THEN $2 ELSE fault_note END, fault_at=$3
		 WHERE id=$1 AND status='normal'`,
		iid, title, faultAt); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id})
}

func (s *Server) listOutages(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT e.id, e.inverter_id, i.name, COALESCE(i.model,''), COALESCE(i.supplier,''),
		       e.title, e.period_month, e.fault_at, e.responded_at, e.resolved_at,
		       e.cause_type, e.under_warranty, e.status,
		       e.lost_kwh_total, e.loss_amount_total, e.supplier_paid_amount, e.property_paid_amount,
		       e.posted_to_ledger, e.cover_status, e.recovery_rate
		FROM inverter_outage_events e JOIN inverters i ON i.id=e.inverter_id
		WHERE e.building_id=$1
		ORDER BY e.fault_at DESC, e.id DESC`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id, iid int64
		var invName, model, supplier, title, month, cause, status, cover string
		var underWarranty, posted bool
		var faultAt, respondedAt, resolvedAt any
		var lostKwh, lossAmt, supPaid, propPaid, recoveryRate float64
		if err := rows.Scan(&id, &iid, &invName, &model, &supplier, &title, &month,
			&faultAt, &respondedAt, &resolvedAt, &cause, &underWarranty, &status,
			&lostKwh, &lossAmt, &supPaid, &propPaid, &posted, &cover, &recoveryRate); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		items = append(items, map[string]any{
			"id": id, "inverter_id": iid, "inverter_name": invName, "model": model, "supplier": supplier,
			"title": title, "period_month": month, "fault_at": faultAt,
			"responded_at": respondedAt, "resolved_at": resolvedAt,
			"cause_type": cause, "under_warranty": underWarranty, "status": status,
			"lost_kwh_total": lostKwh, "loss_amount_total": lossAmt,
			"supplier_paid_amount": supPaid, "property_paid_amount": propPaid,
			"posted_to_ledger": posted, "cover_status": cover, "recovery_rate": recoveryRate,
		})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// getOutage 事件详情：事件 + 分摊行 + 复查提醒
func (s *Server) getOutage(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	detail, code, errMsg := s.loadOutageDetail(r, urlID(r, "oid"))
	if detail == nil {
		writeErr(w, code, errMsg)
		return
	}
	writeJSON(w, 200, detail)
}

func (s *Server) loadOutageDetail(r *http.Request, oid int64) (map[string]any, int, string) {
	var e struct {
		ID, BuildingID, InverterID     int64
		WorkOrderID                    *int64
		Title, PeriodMonth             string
		FaultAt                        time.Time
		RespondedAt, ResolvedAt        *time.Time
		SLAHours                       float64
		CauseType                      string
		ChannelBlocked, ChannelFree    *time.Time
		UnderWarranty                  bool
		WarrantyNote                   string
		BaselinePerDay                 float64
		LostTotal, LostQ, LostC, LostP float64
		GridPrice, SubsidyRate         float64
		LossTotal                      float64
		SupplierPaid, PropertyPaid     float64
		Posted                         bool
		RecBase, RecAfter, RecRate     float64
		ExtraLoss                      float64
		CoverStatus, ReviewNote        string
		Status                         string
		CreatedBy, ReviewedBy          *int64
		CreatedAt                      time.Time
		ReviewedAt                     *time.Time
	}
	err := s.Pool.QueryRow(r.Context(), `
		SELECT id,building_id,inverter_id,work_order_id,title,period_month,
			fault_at,responded_at,resolved_at,response_sla_hours,cause_type,
			channel_blocked_at,channel_freed_at,under_warranty,warranty_note,
			baseline_kwh_per_day,lost_kwh_total,lost_kwh_quality,lost_kwh_channel,lost_kwh_pending,
			grid_price,subsidy_per_kwh,loss_amount_total,supplier_paid_amount,property_paid_amount,
			posted_to_ledger,recovery_baseline_avg,recovery_after_avg,recovery_rate,actual_extra_loss_kwh,
			cover_status,review_note,status,created_by,reviewed_by,created_at,reviewed_at
		FROM inverter_outage_events WHERE id=$1`, oid).Scan(
		&e.ID, &e.BuildingID, &e.InverterID, &e.WorkOrderID, &e.Title, &e.PeriodMonth,
		&e.FaultAt, &e.RespondedAt, &e.ResolvedAt, &e.SLAHours, &e.CauseType,
		&e.ChannelBlocked, &e.ChannelFree, &e.UnderWarranty, &e.WarrantyNote,
		&e.BaselinePerDay, &e.LostTotal, &e.LostQ, &e.LostC, &e.LostP,
		&e.GridPrice, &e.SubsidyRate, &e.LossTotal, &e.SupplierPaid, &e.PropertyPaid,
		&e.Posted, &e.RecBase, &e.RecAfter, &e.RecRate, &e.ExtraLoss,
		&e.CoverStatus, &e.ReviewNote, &e.Status, &e.CreatedBy, &e.ReviewedBy, &e.CreatedAt, &e.ReviewedAt)
	if isNoRows(err) {
		return nil, 404, "停机事件不存在"
	}
	if err != nil {
		return nil, 500, err.Error()
	}
	var invName, model, supplier string
	var warrantyUntil, installedAt *string
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT name, COALESCE(model,''), COALESCE(supplier,''), warranty_until::text, installed_at::text
		 FROM inverters WHERE id=$1`, e.InverterID).Scan(&invName, &model, &supplier, &warrantyUntil, &installedAt)

	allocRows, err := s.Pool.Query(r.Context(), `
		SELECT id, user_id, ratio, panel_count, lost_kwh,
		       loss_quality, loss_channel, loss_pending, loss_total,
		       supplier_due, supplier_paid, property_paid, property_note, settled
		FROM outage_allocations WHERE event_id=$1
		ORDER BY (user_id=0), id`, oid)
	if err != nil {
		return nil, 500, err.Error()
	}
	allocs := []any{}
	for allocRows.Next() {
		var id, uid int64
		var ratio, kwh, lq, lc, lp, lt, sDue, sPaid, pPaid float64
		var panels int
		var note string
		var settled bool
		if err := allocRows.Scan(&id, &uid, &ratio, &panels, &kwh,
			&lq, &lc, &lp, &lt, &sDue, &sPaid, &pPaid, &note, &settled); err != nil {
			allocRows.Close()
			return nil, 500, err.Error()
		}
		name := "公共部分"
		if uid != 0 {
			_ = s.Pool.QueryRow(r.Context(), `SELECT name FROM users WHERE id=$1`, uid).Scan(&name)
		}
		allocs = append(allocs, map[string]any{
			"id": id, "user_id": uid, "name": name, "ratio": ratio, "panel_count": panels,
			"lost_kwh": kwh, "loss_quality": lq, "loss_channel": lc, "loss_pending": lp,
			"loss_total": lt, "supplier_due": sDue, "supplier_paid": sPaid,
			"property_paid": pPaid, "property_note": note, "settled": settled,
		})
	}
	allocRows.Close()

	recheckRows, err := s.Pool.Query(r.Context(), `
		SELECT rc.id, rc.inverter_id, i.name, COALESCE(i.model,''), COALESCE(rc.reason,''),
		       rc.status, COALESCE(rc.result,''), rc.created_at, rc.handled_at
		FROM outage_rechecks rc JOIN inverters i ON i.id=rc.inverter_id
		WHERE rc.event_id=$1 ORDER BY rc.id`, oid)
	if err != nil {
		return nil, 500, err.Error()
	}
	rechecks := []any{}
	for recheckRows.Next() {
		var id, iid int64
		var invN, model, reason, status, result string
		var at, handledAt any
		if err := recheckRows.Scan(&id, &iid, &invN, &model, &reason, &status, &result, &at, &handledAt); err != nil {
			recheckRows.Close()
			return nil, 500, err.Error()
		}
		rechecks = append(rechecks, map[string]any{
			"id": id, "inverter_id": iid, "inverter_name": invN, "model": model,
			"reason": reason, "status": status, "result": result,
			"created_at": at, "handled_at": handledAt,
		})
	}
	recheckRows.Close()

	detail := map[string]any{
		"id": e.ID, "building_id": e.BuildingID, "inverter_id": e.InverterID,
		"inverter_name": invName, "model": model, "supplier": supplier,
		"warranty_until": warrantyUntil, "installed_at": installedAt,
		"work_order_id": e.WorkOrderID, "title": e.Title, "period_month": e.PeriodMonth,
		"fault_at": e.FaultAt, "responded_at": e.RespondedAt, "resolved_at": e.ResolvedAt,
		"response_sla_hours": e.SLAHours, "cause_type": e.CauseType,
		"channel_blocked_at": e.ChannelBlocked, "channel_freed_at": e.ChannelFree,
		"under_warranty": e.UnderWarranty, "warranty_note": e.WarrantyNote,
		"baseline_kwh_per_day": e.BaselinePerDay,
		"lost_kwh_total":       e.LostTotal, "lost_kwh_quality": e.LostQ,
		"lost_kwh_channel": e.LostC, "lost_kwh_pending": e.LostP,
		"grid_price": e.GridPrice, "subsidy_per_kwh": e.SubsidyRate,
		"loss_amount_total":    e.LossTotal,
		"supplier_paid_amount": e.SupplierPaid, "property_paid_amount": e.PropertyPaid,
		"posted_to_ledger":      e.Posted,
		"recovery_baseline_avg": e.RecBase, "recovery_after_avg": e.RecAfter,
		"recovery_rate": e.RecRate, "actual_extra_loss_kwh": e.ExtraLoss,
		"cover_status": e.CoverStatus, "review_note": e.ReviewNote,
		"status": e.Status, "created_at": e.CreatedAt,
		"reviewed_at": e.ReviewedAt,
		"allocations": allocs, "rechecks": rechecks,
	}
	return detail, 200, ""
}

// updateOutageTimeline 更新到场/修复时间
func (s *Server) updateOutageTimeline(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	oid := urlID(r, "oid")
	var in map[string]any
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	var bid, iid int64
	var posted bool
	if err := s.Pool.QueryRow(r.Context(),
		`SELECT building_id, inverter_id, posted_to_ledger FROM inverter_outage_events WHERE id=$1`, oid).
		Scan(&bid, &iid, &posted); err != nil {
		writeErr(w, 404, "停机事件不存在")
		return
	}
	if posted {
		writeErr(w, 409, "补偿已入账，时间轴不可再变更")
		return
	}
	set := []string{}
	args := []any{oid}
	addTime := func(col, key string) error {
		if _, ok := in[key]; !ok {
			return nil
		}
		t, err := timeArg(in, key)
		if err != nil {
			return err
		}
		if t == nil {
			set = append(set, col+"=NULL")
		} else {
			set = append(set, fmt.Sprintf("%s=$%d", col, len(args)+1))
			args = append(args, *t)
		}
		return nil
	}
	for _, c := range []struct{ col, key string }{
		{"responded_at", "responded_at"}, {"resolved_at", "resolved_at"},
	} {
		if err := addTime(c.col, c.key); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if sla := numArg(in, "response_sla_hours"); sla > 0 {
		set = append(set, fmt.Sprintf("response_sla_hours=$%d", len(args)+1))
		args = append(args, sla)
	}
	if len(set) == 0 {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(),
		fmt.Sprintf("UPDATE inverter_outage_events SET %s WHERE id=$1", strings.Join(set, ",")), args...); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 修复时间登记后设备恢复正常（曲线复核在下一步进行）
	if _, ok := in["resolved_at"]; ok {
		if t, _ := timeArg(in, "resolved_at"); t != nil {
			if _, err := tx.Exec(r.Context(),
				`UPDATE inverters SET status='normal', fault_note='', fault_at=NULL WHERE id=$1 AND status<>'normal'`, iid); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// assessOutage 按绑定组件、停机时长、响应 SLA、保修条款测算损失与责任分摊
func (s *Server) assessOutage(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	oid := urlID(r, "oid")
	var in map[string]any
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}

	var bid, iid int64
	var faultAt time.Time
	var respondedAt, resolvedAt *time.Time
	var sla float64
	var posted bool
	if err := s.Pool.QueryRow(r.Context(), `
		SELECT building_id,inverter_id,fault_at,responded_at,resolved_at,response_sla_hours,posted_to_ledger
		FROM inverter_outage_events WHERE id=$1`, oid).
		Scan(&bid, &iid, &faultAt, &respondedAt, &resolvedAt, &sla, &posted); err != nil {
		writeErr(w, 404, "停机事件不存在")
		return
	}
	if posted {
		writeErr(w, 409, "补偿已入账，不可重新测算")
		return
	}

	cause := strArg(in, "cause_type")
	if cause == "" {
		cause = "pending"
	}
	if cause != "pending" && cause != "quality" && cause != "channel" && cause != "mixed" {
		writeErr(w, 400, "cause_type 只能是 pending/quality/channel/mixed")
		return
	}
	chBlocked, err := timeArg(in, "channel_blocked_at")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	chFreed, err := timeArg(in, "channel_freed_at")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	baseline := numArg(in, "baseline_kwh_per_day")
	gridPrice := numArg(in, "grid_price")
	subsidy := numArg(in, "subsidy_per_kwh")
	supplierPaidIn := numArg(in, "supplier_paid_amount")
	propertyPaidIn := numArg(in, "property_paid_amount")

	// 保修：默认按设备档案保修截止日自动判定，可显式覆盖
	var supplier, model string
	var warrantyUntil *time.Time
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT COALESCE(supplier,''), COALESCE(model,''), warranty_until FROM inverters WHERE id=$1`,
		iid).Scan(&supplier, &model, &warrantyUntil)
	underWarranty := warrantyUntil != nil && !warrantyUntil.Before(time.Date(faultAt.Year(), faultAt.Month(), faultAt.Day(), 0, 0, 0, 0, time.UTC))
	if v, ok := in["under_warranty"]; ok {
		if b, ok := v.(bool); ok {
			underWarranty = b
		}
	}
	warrantyNote := strArg(in, "warranty_note")
	if warrantyNote == "" {
		if warrantyUntil != nil {
			warrantyNote = fmt.Sprintf("供应商 %s；保修至 %s（%s）",
				orDash(supplier), warrantyUntil.Format("2006-01-02"),
				map[bool]string{true: "故障在保修期内", false: "故障已超出保修期"}[underWarranty])
		} else if supplier != "" {
			warrantyNote = "供应商 " + supplier + "；未登记保修截止日"
		}
	}

	// 绑定组件（user_id=0 为公共部分）
	rows, err := s.Pool.Query(r.Context(), `
		SELECT CASE WHEN b.user_id=0 THEN NULL ELSE b.user_id END,
		       COALESCE(u.name,'公共部分'), b.panel_count, b.ratio
		FROM panel_bindings b LEFT JOIN users u ON u.id=b.user_id
		WHERE b.inverter_id=$1
		ORDER BY (b.user_id=0), b.id`, iid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var bindings []outage.Binding
	for rows.Next() {
		var uid *int64
		var name string
		var panels int
		var ratio float64
		if err := rows.Scan(&uid, &name, &panels, &ratio); err != nil {
			rows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		bindings = append(bindings, outage.Binding{UserID: uid, Name: name, Ratio: ratio, PanelCount: panels})
	}
	rows.Close()

	ain := outage.Input{
		FaultAt: faultAt, RespondedAt: respondedAt, ResolvedAt: resolvedAt,
		AsOf: time.Now(), ResponseSLAHours: sla,
		CauseType: cause, ChannelBlockedAt: chBlocked, ChannelFreedAt: chFreed,
		UnderWarranty:     underWarranty,
		BaselineKwhPerDay: baseline, GridPrice: gridPrice, SubsidyRate: subsidy,
		SupplierPaidTotal: supplierPaidIn, PropertyPaidTotal: propertyPaidIn,
		Bindings: bindings,
	}
	res := outage.Assess(ain)

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `
		UPDATE inverter_outage_events SET
			cause_type=$2, channel_blocked_at=$3, channel_freed_at=$4,
			under_warranty=$5, warranty_note=$6, baseline_kwh_per_day=$7,
			lost_kwh_total=$8, lost_kwh_quality=$9, lost_kwh_channel=$10, lost_kwh_pending=$11,
			grid_price=$12, subsidy_per_kwh=$13, loss_amount_total=$14,
			supplier_paid_amount=$15, property_paid_amount=$16
		WHERE id=$1`,
		oid, cause, chBlocked, chFreed, underWarranty, warrantyNote, baseline,
		res.LostKwhTotal, res.LostKwhQuality, res.LostKwhChannel, res.LostKwhPending,
		gridPrice, subsidy, res.LossAmountTotal,
		res.SupplierPaidTotal, res.PropertyPaidTotal); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM outage_allocations WHERE event_id=$1`, oid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, a := range res.Allocations {
		isPublic := a.UserID == nil
		note := ""
		if isPublic {
			if a.PropertyPaid > 0 {
				note = "物业责任（维护通道占用延迟）赔付留存公共账，已在公共账说明"
			}
		} else if a.PropertyPaid > 0 {
			note = "物业责任部分经楼栋公共账赔付至住户收益补偿"
		}
		uid := int64(0)
		if a.UserID != nil {
			uid = *a.UserID
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO outage_allocations(event_id,user_id,ratio,panel_count,lost_kwh,
				loss_quality,loss_channel,loss_pending,loss_total,supplier_due,supplier_paid,property_paid,property_note)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			oid, uid, a.Ratio, a.PanelCount, a.LostKwh,
			a.LossQuality, a.LossChannel, a.LossPending, a.LossTotal,
			a.SupplierDue, a.SupplierPaid, a.PropertyPaid, note); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	detail, code, msg := s.loadOutageDetail(r, oid)
	if detail == nil {
		writeErr(w, code, msg)
		return
	}
	detail["assessment"] = res
	writeJSON(w, 200, detail)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// postOutageCompensation 把供应商/物业赔付入账：住户部分进入住户收益补偿，公共部分与说明进入公共账。
func (s *Server) postOutageCompensation(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	oid := urlID(r, "oid")

	var bid int64
	var month, invName string
	var cause string
	var supPaidTotal, propPaidTotal float64
	var posted bool
	if err := s.Pool.QueryRow(r.Context(), `
		SELECT e.building_id,e.period_month,i.name,e.cause_type,
		       e.supplier_paid_amount,e.property_paid_amount,e.posted_to_ledger
		FROM inverter_outage_events e JOIN inverters i ON i.id=e.inverter_id
		WHERE e.id=$1`, oid).Scan(&bid, &month, &invName, &cause, &supPaidTotal, &propPaidTotal, &posted); err != nil {
		writeErr(w, 404, "停机事件不存在")
		return
	}
	if posted {
		writeErr(w, 409, "该事件补偿已入账，不能重复登记")
		return
	}
	if supPaidTotal+propPaidTotal <= 0 {
		writeErr(w, 409, "尚无可入账的赔付金额，请先完成责任测算并登记赔付")
		return
	}

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	// 赔付现金先记收付款记录（payments 是事实台账，不参与公共账户余额计算）；
	// 公共账户余额由下面按分摊行写入的 ledger 决定：公共份额留存公共账，住户份额经公共账转出至住户。
	if supPaidTotal > 0 {
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO payments(building_id,user_id,amount,pay_type,memo)
			VALUES ($1,NULL,$2,'compensation',$3)`,
			bid, supPaidTotal, month+" "+invName+"停机 供应商设备质量赔付（保修期内）"); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if propPaidTotal > 0 {
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO payments(building_id,user_id,amount,pay_type,memo)
			VALUES ($1,NULL,$2,'compensation',$3)`,
			bid, propPaidTotal, month+" "+invName+"停机 物业（维护通道占用延迟）赔付款"); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}

	rows, err := tx.Query(r.Context(),
		`SELECT id,user_id,supplier_paid,property_paid FROM outage_allocations WHERE event_id=$1`, oid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	type alloc struct {
		id                         int64
		uid                        int64
		supplierPaid, propertyPaid float64
	}
	var allocs []alloc
	for rows.Next() {
		var a alloc
		if err := rows.Scan(&a.id, &a.uid, &a.supplierPaid, &a.propertyPaid); err != nil {
			rows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		allocs = append(allocs, a)
	}
	rows.Close()

	for _, a := range allocs {
		if a.supplierPaid > 0 {
			if a.uid == 0 {
				if _, err := tx.Exec(r.Context(), `
					INSERT INTO ledger(building_id,user_id,direction,amount,category,period_month,ref_type,ref_id,memo)
					VALUES ($1,NULL,'in',$2,'compensation',$3,'outage',$4,$5)`,
					bid, a.supplierPaid, month, oid,
					month+" 公共组件部分 供应商质量赔付（留存公共账户）"); err != nil {
					writeErr(w, 500, err.Error())
					return
				}
			} else {
				if err := postToResident(r, tx, bid, oid, month, invName, a.uid, a.supplierPaid,
					"供应商设备质量赔付 → 住户收益补偿"); err != nil {
					writeErr(w, 500, err.Error())
					return
				}
			}
		}
		if a.propertyPaid > 0 {
			if a.uid == 0 {
				if _, err := tx.Exec(r.Context(), `
					INSERT INTO ledger(building_id,user_id,direction,amount,category,period_month,ref_type,ref_id,memo)
					VALUES ($1,NULL,'in',$2,'compensation',$3,'outage',$4,$5)`,
					bid, a.propertyPaid, month, oid,
					month+" 公共组件部分 物业责任赔付（维护通道占用延迟，留存公共账说明）"); err != nil {
					writeErr(w, 500, err.Error())
					return
				}
			} else {
				if err := postToResident(r, tx, bid, oid, month, invName, a.uid, a.propertyPaid,
					"物业责任赔付（屋顶维护通道占用致维修延迟，经公共账）→ 住户收益补偿"); err != nil {
					writeErr(w, 500, err.Error())
					return
				}
			}
		}
		if _, err := tx.Exec(r.Context(),
			`UPDATE outage_allocations SET settled=TRUE WHERE id=$1`, a.id); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}

	if _, err := tx.Exec(r.Context(),
		`UPDATE inverter_outage_events SET posted_to_ledger=TRUE WHERE id=$1`, oid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// postToResident 赔付经公共账转出并计入住户余额（公共账 in 已在总额处登记，此处记 out + 住户 in）。
func postToResident(r *http.Request, tx pgx.Tx, bid, oid int64, month, invName string, uid int64, amount float64, memo string) error {
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO ledger(building_id,user_id,direction,amount,category,period_month,ref_type,ref_id,memo)
		VALUES ($1,NULL,'out',$2,'compensation',$3,'outage',$4,$5)`,
		bid, amount, month, oid, month+" "+invName+"停机赔付经公共账转出："+memo); err != nil {
		return err
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO ledger(building_id,user_id,direction,amount,category,period_month,ref_type,ref_id,memo)
		VALUES ($1,$2,'in',$3,'compensation',$4,'outage',$5,$6)`,
		bid, uid, amount, month, oid, month+" "+invName+" "+memo); err != nil {
		return err
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE users SET balance=balance+$2 WHERE id=$1`, uid, amount); err != nil {
		return err
	}
	return nil
}
