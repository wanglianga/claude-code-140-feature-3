package api

import (
	"net/http"

	"pvplatform/internal/auth"
)

// ---------- 房屋转让 / 退购 ----------

func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		SubscriptionID  int64  `json:"subscription_id"`
		ToUserID        int64  `json:"to_user_id"`
		ToName          string `json:"to_name"`
		ToUnit          string `json:"to_unit"`
		RightsTransfer  *bool  `json:"rights_transfer"`
		RefundRequested bool   `json:"refund_requested"`
		Memo            string `json:"memo"`
	}
	if err := decodeJSON(r, &in); err != nil || in.SubscriptionID == 0 {
		writeErr(w, 400, "subscription_id 必填")
		return
	}
	rights := true
	if in.RightsTransfer != nil {
		rights = *in.RightsTransfer
	}
	if !rights && !in.RefundRequested {
		writeErr(w, 400, "权益不随房转移时，需要明确选择退购（refund_requested=true）")
		return
	}
	var subBid, fromID int64
	var status string
	err := s.Pool.QueryRow(r.Context(),
		`SELECT building_id, user_id, status FROM subscriptions WHERE id=$1`, in.SubscriptionID).
		Scan(&subBid, &fromID, &status)
	if isNoRows(err) {
		writeErr(w, 404, "认购记录不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if subBid != bid {
		writeErr(w, 400, "认购记录不属于该楼栋")
		return
	}
	if u.ID != fromID && u.Role != "admin" && u.Role != "property" {
		writeErr(w, 403, "只能为本人认购发起房屋转让")
		return
	}
	if status != "paid" {
		writeErr(w, 409, "仅已缴费的认购可以办理转让/退购，当前状态："+status)
		return
	}
	// 历史收益是否结清：检查是否存在已定稿但金额尚未体现……简化口径：余额已结算即视为结清，
	// 这里把发起时点是否有未结草稿期作为提示字段（物业确认时再次把关）。
	var pendingDraft int
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM settlement_periods WHERE building_id=$1 AND status='draft'`, bid).Scan(&pendingDraft)

	var id int64
	err = s.Pool.QueryRow(r.Context(), `
		INSERT INTO transfers(building_id,subscription_id,from_user_id,to_user_id,to_name,to_unit,
			rights_transfer,refund_requested,history_settled,memo)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		bid, in.SubscriptionID, fromID, nullableID(in.ToUserID), in.ToName, in.ToUnit,
		rights, in.RefundRequested, pendingDraft == 0, in.Memo).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "history_settled": pendingDraft == 0})
}

func (s *Server) listTransfers(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT t.id, t.subscription_id, f.name, COALESCE(f.unit_no,''),
			COALESCE(t.to_user_id,0), COALESCE(tt.name,t.to_name), COALESCE(tt.unit_no,t.to_unit),
			t.rights_transfer, t.refund_requested, t.history_settled, t.status,
			COALESCE(t.memo,''), t.created_at, t.confirmed_at
		FROM transfers t
		JOIN users f ON f.id=t.from_user_id
		LEFT JOIN users tt ON tt.id=t.to_user_id
		WHERE t.building_id=$1 ORDER BY t.id DESC`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, sid, fromID, toID int64
		var fromName, fromUnit, toName, toUnit, status, memo string
		var rights, refund, settled bool
		var createdAt, confirmedAt any
		if err := rows.Scan(&id, &sid, &fromName, &fromUnit, &toID, &toName, &toUnit,
			&rights, &refund, &settled, &status, &memo, &createdAt, &confirmedAt); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id": id, "subscription_id": sid,
			"from_user_id": fromID, "from_name": fromName, "from_unit": fromUnit,
			"to_user_id": toID, "to_name": toName, "to_unit": toUnit,
			"rights_transfer": rights, "refund_requested": refund,
			"history_settled": settled, "status": status, "memo": memo,
			"created_at": createdAt, "confirmed_at": confirmedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

// confirmTransfer 物业/管理员确认转让：
//   - 权益随房转移：认购记录过户给新业主，历史收益仍归原业主（已打入原业主余额）；
//   - 退购：按退购处理，认购标记 refunded 并退款。
//   - 存在未结草稿结算期时拒绝，要求历史收益先结清。
func (s *Server) confirmTransfer(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	tid := urlID(r, "tid")
	var bid, sid, fromID, toID *int64
	var toName, toUnit, status string
	var rights, refund bool
	err := s.Pool.QueryRow(r.Context(), `
		SELECT building_id, subscription_id, from_user_id, to_user_id,
			COALESCE(to_name,''), COALESCE(to_unit,''), rights_transfer, refund_requested, status
		FROM transfers WHERE id=$1`, tid).
		Scan(&bid, &sid, &fromID, &toID, &toName, &toUnit, &rights, &refund, &status)
	if isNoRows(err) {
		writeErr(w, 404, "转让申请不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if status != "pending" {
		writeErr(w, 409, "该转让申请已处理："+status)
		return
	}
	var pendingDraft int
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM settlement_periods WHERE building_id=$1 AND status='draft'`, *bid).Scan(&pendingDraft)
	if pendingDraft > 0 {
		writeErr(w, 409, "存在未最终定稿的月度结算，历史收益未结清，请先生成/定稿或删除草稿后再办理")
		return
	}

	var subAmount float64
	var subStatus string
	if err := s.Pool.QueryRow(r.Context(),
		`SELECT amount, status FROM subscriptions WHERE id=$1`, *sid).Scan(&subAmount, &subStatus); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if subStatus != "paid" {
		writeErr(w, 409, "认购当前状态不是已缴费，无法确认转让")
		return
	}

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	if rights && toID != nil {
		// 权益随房转移：认购过户给新业主（状态保持 paid 以参与后续月份结算）
		if _, err := tx.Exec(r.Context(),
			`UPDATE subscriptions SET user_id=$2 WHERE id=$1`, *sid, *toID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		// 设备绑定组件随认购权益一并过户给新业主（同逆变器下已有记录则合并）
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO panel_bindings(building_id,inverter_id,user_id,panel_count,ratio)
			SELECT building_id,inverter_id,$2,panel_count,ratio FROM panel_bindings
			WHERE building_id=$1 AND user_id=$3
			ON CONFLICT (inverter_id,user_id)
			DO UPDATE SET panel_count=panel_bindings.panel_count+EXCLUDED.panel_count,
			              ratio=panel_bindings.ratio+EXCLUDED.ratio`,
			*bid, *toID, *fromID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := tx.Exec(r.Context(),
			`DELETE FROM panel_bindings WHERE building_id=$1 AND user_id=$2`, *bid, *fromID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := tx.Exec(r.Context(),
			`INSERT INTO building_members(building_id,user_id,member_type) VALUES ($1,$2,'resident') ON CONFLICT DO NOTHING`,
			*bid, *toID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO ledger(building_id,user_id,direction,amount,category,ref_type,ref_id,memo)
			VALUES ($1,$2,'in',0,'transfer_in','transfer',$3,'认购权益随房屋转入')`,
			*bid, *toID, tid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	} else {
		// 退购：退款给原业主，认购份额与设备绑定组件回到公共部分（user_id=0）
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO panel_bindings(building_id,inverter_id,user_id,panel_count,ratio)
			SELECT building_id,inverter_id,0,panel_count,ratio FROM panel_bindings
			WHERE building_id=$1 AND user_id=$2
			ON CONFLICT (inverter_id,user_id)
			DO UPDATE SET panel_count=panel_bindings.panel_count+EXCLUDED.panel_count,
			              ratio=panel_bindings.ratio+EXCLUDED.ratio`,
			*bid, *fromID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := tx.Exec(r.Context(),
			`DELETE FROM panel_bindings WHERE building_id=$1 AND user_id=$2`, *bid, *fromID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := tx.Exec(r.Context(),
			`UPDATE subscriptions SET status='refunded' WHERE id=$1`, *sid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if subAmount > 0 {
			if _, err := tx.Exec(r.Context(),
				`INSERT INTO payments(building_id,user_id,amount,pay_type,memo) VALUES ($1,$2,$3,'refund','房屋转让-退购退款')`,
				*bid, *fromID, subAmount); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			if _, err := tx.Exec(r.Context(),
				`INSERT INTO ledger(building_id,user_id,direction,amount,category,ref_type,ref_id,memo)
				 VALUES ($1,$2,'out',$3,'refund','transfer',$4,'房屋转让退购退款')`,
				*bid, *fromID, subAmount, tid); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
		}
		if _, err := tx.Exec(r.Context(),
			`UPDATE buildings SET subscribed_ratio=(SELECT COALESCE(SUM(panel_ratio),0) FROM subscriptions
			  WHERE building_id=$1 AND status='paid' AND sub_type='shareholder') WHERE id=$1`, *bid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE transfers SET status='confirmed', confirmed_at=now(), history_settled=TRUE WHERE id=$1`, tid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) rejectTransfer(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	tid := urlID(r, "tid")
	var in struct {
		Memo string `json:"memo"`
	}
	_ = decodeJSON(r, &in)
	ct, err := s.Pool.Exec(r.Context(),
		`UPDATE transfers SET status='rejected', memo=COALESCE(NULLIF($2,''),memo) WHERE id=$1 AND status='pending'`,
		tid, in.Memo)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, 404, "转让申请不存在或已处理")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
