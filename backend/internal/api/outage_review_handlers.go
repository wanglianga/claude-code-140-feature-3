package api

import (
	"fmt"
	"net/http"
	"time"

	"pvplatform/internal/auth"
	"pvplatform/internal/outage"
)

// reviewOutage 对比修复前后的发电曲线，确认补偿是否覆盖真实损失；必要时生成同型号设备复查提醒。
func (s *Server) reviewOutage(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	oid := urlID(r, "oid")
	var in struct {
		BaselineDays int    `json:"baseline_days"` // 取故障前最近 N 天正常曲线，默认 7
		RecoveryDays int    `json:"recovery_days"` // 取修复后 N 天，默认 7
		Note         string `json:"note"`          // 追加复核备注
	}
	_ = decodeJSON(r, &in)
	if in.BaselineDays <= 0 {
		in.BaselineDays = 7
	}
	if in.RecoveryDays <= 0 {
		in.RecoveryDays = 7
	}

	var bid, iid int64
	var faultAt time.Time
	var resolvedAt *time.Time
	var baselineInput, gridPrice, subsidy float64
	var lossTotal, supPaid, propPaid float64
	var cause, model string
	var posted bool
	if err := s.Pool.QueryRow(r.Context(), `
		SELECT e.building_id,e.inverter_id,e.fault_at,e.resolved_at,
		       COALESCE(e.baseline_kwh_per_day,0), e.grid_price,e.subsidy_per_kwh,
		       COALESCE(e.loss_amount_total,0), COALESCE(e.supplier_paid_amount,0),
		       COALESCE(e.property_paid_amount,0), e.cause_type, COALESCE(i.model,''), e.posted_to_ledger
		FROM inverter_outage_events e JOIN inverters i ON i.id=e.inverter_id
		WHERE e.id=$1`, oid).Scan(&bid, &iid, &faultAt, &resolvedAt, &baselineInput,
		&gridPrice, &subsidy, &lossTotal, &supPaid, &propPaid, &cause, &model, &posted); err != nil {
		writeErr(w, 404, "停机事件不存在")
		return
	}
	if resolvedAt == nil {
		writeErr(w, 409, "尚未登记修复完成时间，无法做修复后曲线复核")
		return
	}

	loadPoints := func(query string, args ...any) []outage.CurvePoint {
		rows, err := s.Pool.Query(r.Context(), query, args...)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var pts []outage.CurvePoint
		for rows.Next() {
			var day time.Time
			var kwh float64
			if err := rows.Scan(&day, &kwh); err != nil {
				continue
			}
			pts = append(pts, outage.CurvePoint{Day: day, Kwh: kwh})
		}
		return pts
	}
	// 故障前正常日（phase=normal），取最近 BaselineDays 个点
	basePts := loadPoints(`
		SELECT day, kwh FROM generation_curve_points
		WHERE building_id=$1 AND inverter_id=$2 AND phase='normal' AND day < $3::date
		ORDER BY day DESC LIMIT $4`, bid, iid, faultAt, in.BaselineDays)
	for i, j := 0, len(basePts)-1; i < j; i, j = i+1, j-1 {
		basePts[i], basePts[j] = basePts[j], basePts[i]
	}
	// 修复后曲线：从修复完成后的第一个完整发电日起（修复时刻 +12h 所在日，
	// 避免修复当天只发了几个小时的欠发拉低均值），优先 phase=recovery，否则取修复后全部点
	recPts := loadPoints(`
		SELECT day, kwh FROM generation_curve_points
		WHERE building_id=$1 AND inverter_id=$2
		  AND day >= ($3::timestamptz + interval '12 hours')::date
		  AND (phase='recovery' OR NOT EXISTS (
		      SELECT 1 FROM generation_curve_points g2
		      WHERE g2.building_id=$1 AND g2.inverter_id=$2
		        AND g2.day >= ($3::timestamptz + interval '12 hours')::date AND phase='recovery'))
		ORDER BY day ASC LIMIT $4`, bid, iid, *resolvedAt, in.RecoveryDays)

	rv := outage.CompareCurve(outage.ReviewInput{
		BaselinePoints:    basePts,
		RecoveryPoints:    recPts,
		FallbackBaseline:  baselineInput,
		EventLossAmount:   lossTotal,
		SupplierPaid:      supPaid,
		PropertyPaid:      propPaid,
		MinRecoveryPoints: 3,
	}, gridPrice+subsidy)
	if in.Note != "" {
		if rv.Note != "" {
			rv.Note = rv.Note + "；" + in.Note
		} else {
			rv.Note = in.Note
		}
	}

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `
		UPDATE inverter_outage_events SET
			recovery_baseline_avg=$2, recovery_after_avg=$3, recovery_rate=$4,
			actual_extra_loss_kwh=$5, cover_status=$6, review_note=$7,
			reviewed_by=$8, reviewed_at=now(),
			status=CASE WHEN status='open' THEN 'recovered' ELSE status END
		WHERE id=$1`,
		oid, rv.BaselineAvg, rv.RecoveryAvg, rv.RecoveryRate, rv.ExtraLossKwh,
		rv.CoverStatus, rv.Note, u.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 把修复后完整观测日标记为 recovery（修复当天保留原状）
	if _, err := tx.Exec(r.Context(), `
		UPDATE generation_curve_points SET phase='recovery'
		WHERE building_id=$1 AND inverter_id=$2
		  AND day >= (COALESCE((SELECT max(resolved_at) FROM inverter_outage_events WHERE id=$3), now()) + interval '12 hours')::date
		  AND phase='normal'`,
		bid, iid, oid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	createdRechecks := []int64{}
	// 设备质量（保修赔付）或复核发现欠发 → 提醒运维复查同型号设备
	if rv.CoverStatus != "pending" && (cause == "quality" || cause == "mixed" || rv.CoverStatus == "shortfall") {
		rows, qerr := tx.Query(r.Context(), `
			SELECT id, name, COALESCE(supplier,'') FROM inverters
			WHERE building_id=$1 AND id<>$2 AND model=$3 AND model<>''
			AND NOT EXISTS (SELECT 1 FROM outage_rechecks rc WHERE rc.event_id=$4 AND rc.inverter_id=inverters.id)`,
			bid, iid, model, oid)
		if qerr == nil {
			type peer struct {
				id             int64
				name, supplier string
			}
			var peers []peer
			for rows.Next() {
				var p peer
				_ = rows.Scan(&p.id, &p.name, &p.supplier)
				peers = append(peers, p)
			}
			rows.Close()
			reason := ""
			switch {
			case rv.CoverStatus == "shortfall":
				reason = fmt.Sprintf("同型号（%s）设备发生停机且修复后仍有欠发（恢复率 %.1f%%），请复查散热/模块等共性缺陷",
					model, rv.RecoveryRate)
			case cause == "quality" || cause == "mixed":
				reason = fmt.Sprintf("同型号（%s）设备被认定质量故障并触发保修赔付，请安排批次复查", model)
			}
			for _, p := range peers {
				var rcid int64
				if err := tx.QueryRow(r.Context(), `
					INSERT INTO outage_rechecks(building_id,event_id,inverter_id,reason)
					VALUES ($1,$2,$3,$4) RETURNING id`, bid, oid, p.id, reason).Scan(&rcid); err == nil {
					createdRechecks = append(createdRechecks, rcid)
				}
			}
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "review": rv,
		"baseline_points": len(basePts), "recovery_points": len(recPts),
		"recheck_created": createdRechecks,
	})
}

// listRechecks 同型号设备复查提醒
func (s *Server) listRechecks(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	status := r.URL.Query().Get("status")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT rc.id, rc.event_id, rc.inverter_id, i.name, COALESCE(i.model,''),
		       COALESCE(rc.reason,''), rc.status, COALESCE(rc.result,''), rc.created_at, rc.handled_at
		FROM outage_rechecks rc JOIN inverters i ON i.id=rc.inverter_id
		WHERE rc.building_id=$1 AND ($2='' OR rc.status=$2)
		ORDER BY rc.status DESC, rc.id DESC`, bid, status)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id, eid, iid int64
		var invName, model, reason, st, result string
		var at, handledAt any
		if err := rows.Scan(&id, &eid, &iid, &invName, &model, &reason, &st, &result, &at, &handledAt); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		items = append(items, map[string]any{
			"id": id, "event_id": eid, "inverter_id": iid, "inverter_name": invName,
			"model": model, "reason": reason, "status": st, "result": result,
			"created_at": at, "handled_at": handledAt,
		})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// handleRecheck 运维处理复查提醒（填写复查结果，标记完成/忽略）
func (s *Server) handleRecheck(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	rid := urlID(r, "rid")
	var in struct {
		Status string `json:"status"` // done / dismissed
		Result string `json:"result"`
	}
	if err := decodeJSON(r, &in); err != nil || (in.Status != "done" && in.Status != "dismissed") {
		writeErr(w, 400, "status 必须是 done 或 dismissed")
		return
	}
	ct, err := s.Pool.Exec(r.Context(), `
		UPDATE outage_rechecks SET status=$2, result=COALESCE(NULLIF($3,''),result),
			handled_by=$4, handled_at=now()
		WHERE id=$1 AND status='open'`, rid, in.Status, in.Result, u.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, 404, "复查提醒不存在或已处理")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
