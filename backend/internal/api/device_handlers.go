package api

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"pvplatform/internal/auth"
)

// ---------- 逆变器 ----------

func (s *Server) createInverter(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		Name           string  `json:"name"`
		CapacityKw     float64 `json:"capacity_kw"`
		CapacityRatio  float64 `json:"capacity_ratio"`
		Model          string  `json:"model"`
		Supplier       string  `json:"supplier"`
		WarrantyMonths int     `json:"warranty_months"`
		InstallDate    string  `json:"install_date"` // YYYY-MM-DD
	}
	if err := decodeJSON(r, &in); err != nil || in.Name == "" {
		writeErr(w, 400, "逆变器名称必填")
		return
	}
	if in.CapacityRatio <= 0 && in.CapacityKw > 0 {
		var total float64
		_ = s.Pool.QueryRow(r.Context(), `SELECT total_capacity_kw FROM buildings WHERE id=$1`, bid).Scan(&total)
		if total > 0 {
			in.CapacityRatio = in.CapacityKw / total * 100
		}
	}
	var installDate any
	if in.InstallDate != "" {
		if t, err := time.Parse("2006-01-02", in.InstallDate); err == nil {
			installDate = t
		} else {
			writeErr(w, 400, "install_date 格式须为 YYYY-MM-DD")
			return
		}
	}
	var id int64
	err := s.Pool.QueryRow(r.Context(),
		`INSERT INTO inverters(building_id,name,capacity_kw,capacity_ratio,model,supplier,warranty_months,install_date)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		bid, in.Name, in.CapacityKw, in.CapacityRatio, in.Model, in.Supplier, in.WarrantyMonths, installDate).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id})
}

func (s *Server) listInverters(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT id, name, capacity_kw, capacity_ratio, status, COALESCE(fault_note,''), fault_at,
			COALESCE(model,''), COALESCE(supplier,''), COALESCE(warranty_months,0), install_date
		FROM inverters WHERE building_id=$1 ORDER BY id`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id int64
		var name string
		var kw, ratio float64
		var status, note, model, supplier string
		var warrantyMonths int
		var faultAt, installDate any
		if err := rows.Scan(&id, &name, &kw, &ratio, &status, &note, &faultAt,
			&model, &supplier, &warrantyMonths, &installDate); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id": id, "name": name, "capacity_kw": kw, "capacity_ratio": ratio,
			"status": status, "fault_note": note, "fault_at": faultAt,
			"model": model, "supplier": supplier,
			"warranty_months": warrantyMonths, "install_date": installDate,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) faultInverter(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	iid := urlID(r, "iid")
	var in struct {
		Note string `json:"note"`
	}
	_ = decodeJSON(r, &in)
	ct, err := s.Pool.Exec(r.Context(),
		`UPDATE inverters SET status='faulty', fault_note=$2, fault_at=now() WHERE id=$1 AND status<>'faulty'`,
		iid, in.Note)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, 404, "逆变器不存在或已处于故障状态")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// updateInverterProfile 更新设备档案（型号/供应商/保修条款），供保修责任判定使用
func (s *Server) updateInverterProfile(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	iid := urlID(r, "iid")
	var in struct {
		Model          *string `json:"model"`
		Supplier       *string `json:"supplier"`
		WarrantyMonths *int    `json:"warranty_months"`
		InstallDate    *string `json:"install_date"` // YYYY-MM-DD，空串清空
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	var installArg any
	clearInstall := false
	if in.InstallDate != nil {
		if *in.InstallDate == "" {
			clearInstall = true
		} else if t, err := time.Parse("2006-01-02", *in.InstallDate); err == nil {
			installArg = t
		} else {
			writeErr(w, 400, "install_date 格式须为 YYYY-MM-DD")
			return
		}
	}
	var ct pgconn.CommandTag
	var err error
	if clearInstall {
		ct, err = s.Pool.Exec(r.Context(), `
			UPDATE inverters SET
				model=COALESCE($2,model), supplier=COALESCE($3,supplier),
				warranty_months=COALESCE($4,warranty_months), install_date=NULL
			WHERE id=$1`,
			iid, in.Model, in.Supplier, in.WarrantyMonths)
	} else {
		ct, err = s.Pool.Exec(r.Context(), `
			UPDATE inverters SET
				model=COALESCE($2,model), supplier=COALESCE($3,supplier),
				warranty_months=COALESCE($4,warranty_months), install_date=COALESCE($5,install_date)
			WHERE id=$1`,
			iid, in.Model, in.Supplier, in.WarrantyMonths, installArg)
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, 404, "逆变器不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) resolveInverter(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	iid := urlID(r, "iid")
	ct, err := s.Pool.Exec(r.Context(),
		`UPDATE inverters SET status='normal', fault_note='', fault_at=NULL WHERE id=$1 AND status='faulty'`, iid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, 404, "逆变器不存在或当前无故障")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 发电计量 ----------

func (s *Server) uploadGeneration(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		PeriodMonth string  `json:"period_month"`
		Kwh         float64 `json:"kwh"`
		InverterID  int64   `json:"inverter_id"`
		Note        string  `json:"note"`
	}
	if err := decodeJSON(r, &in); err != nil || in.PeriodMonth == "" {
		writeErr(w, 400, "period_month（YYYY-MM）与 kwh 必填")
		return
	}
	if in.Kwh < 0 {
		writeErr(w, 400, "发电量不能为负")
		return
	}
	var stage string
	_ = s.Pool.QueryRow(r.Context(), `SELECT stage FROM buildings WHERE id=$1`, bid).Scan(&stage)
	if stage != "operating" {
		writeErr(w, 409, "项目尚未并网运行，不能录入发电量")
		return
	}
	var id int64
	err := s.Pool.QueryRow(r.Context(), `
		INSERT INTO generation_records(building_id,inverter_id,period_month,kwh,note,created_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (building_id,inverter_id,period_month)
		DO UPDATE SET kwh=EXCLUDED.kwh, note=EXCLUDED.note
		RETURNING id`, bid, nullableID(in.InverterID), in.PeriodMonth, in.Kwh, in.Note, u.ID).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": id})
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *Server) listGeneration(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT g.period_month, COALESCE(g.inverter_id,0), COALESCE(i.name,'楼总表'), g.kwh, COALESCE(g.note,''), g.created_at
		FROM generation_records g LEFT JOIN inverters i ON i.id=g.inverter_id
		WHERE g.building_id=$1 ORDER BY g.period_month, g.inverter_id`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	monthTotals := map[string]float64{}
	out := []any{}
	for rows.Next() {
		var month, name, note string
		var invID int64
		var kwh float64
		var at any
		if err := rows.Scan(&month, &invID, &name, &kwh, &note, &at); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if invID == 0 {
			monthTotals[month] += kwh
		}
		out = append(out, map[string]any{
			"period_month": month, "inverter_id": invID, "inverter_name": name,
			"kwh": kwh, "note": note, "created_at": at,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out, "month_totals": monthTotals})
}

// ---------- 发电异常 ----------

func (s *Server) createAlert(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		PeriodMonth string `json:"period_month"`
		InverterID  int64  `json:"inverter_id"`
		Reason      string `json:"reason"`
	}
	if err := decodeJSON(r, &in); err != nil || in.PeriodMonth == "" {
		writeErr(w, 400, "period_month 与 reason 必填")
		return
	}
	var id int64
	err := s.Pool.QueryRow(r.Context(), `
		INSERT INTO generation_alerts(building_id,inverter_id,period_month,reason,created_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		bid, nullableID(in.InverterID), in.PeriodMonth, in.Reason, u.ID).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id})
}

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT a.id, a.period_month, COALESCE(a.inverter_id,0), COALESCE(i.name,'楼总表'),
			a.reason, COALESCE(a.explanation,''), a.status, a.created_at
		FROM generation_alerts a LEFT JOIN inverters i ON i.id=a.inverter_id
		WHERE a.building_id=$1 ORDER BY a.id DESC`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, invID int64
		var month, name, reason, explanation, status string
		var at any
		if err := rows.Scan(&id, &month, &invID, &name, &reason, &explanation, &status, &at); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id": id, "period_month": month, "inverter_id": invID, "inverter_name": name,
			"reason": reason, "explanation": explanation, "status": status, "created_at": at,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

// explainAlert 物业对发电异常给出解释
func (s *Server) explainAlert(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	aid := urlID(r, "aid")
	var in struct {
		Explanation string `json:"explanation"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Explanation == "" {
		writeErr(w, 400, "解释内容必填")
		return
	}
	ct, err := s.Pool.Exec(r.Context(),
		`UPDATE generation_alerts SET explanation=$2, status='explained', explained_at=now() WHERE id=$1`,
		aid, in.Explanation)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, 404, "异常记录不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
