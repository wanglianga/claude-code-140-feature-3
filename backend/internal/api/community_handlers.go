package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"pvplatform/internal/auth"
	"pvplatform/internal/settlement"
)

// ---------- 公告 ----------

func (s *Server) createAnnouncement(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		Title   string `json:"title"`
		Content string `json:"content"`
		Pinned  bool   `json:"pinned"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Title == "" {
		writeErr(w, 400, "公告标题必填")
		return
	}
	var id int64
	err := s.Pool.QueryRow(r.Context(),
		`INSERT INTO announcements(building_id,title,content,pinned,author_id) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		bid, in.Title, in.Content, in.Pinned, u.ID).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id})
}

func (s *Server) listAnnouncements(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT a.id, a.title, a.content, a.pinned, COALESCE(u.name,''), a.created_at
		FROM announcements a LEFT JOIN users u ON u.id=a.author_id
		WHERE a.building_id=$1 ORDER BY a.pinned DESC, a.id DESC LIMIT 100`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id int64
		var title, content, author string
		var pinned bool
		var at any
		if err := rows.Scan(&id, &title, &content, &pinned, &author, &at); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id": id, "title": title, "content": content, "pinned": pinned,
			"author": author, "created_at": at,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) deleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	aid := urlID(r, "aid")
	ct, err := s.Pool.Exec(r.Context(), `DELETE FROM announcements WHERE id=$1`, aid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, 404, "公告不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 住户异议投票 ----------

func (s *Server) createPoll(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Title == "" {
		writeErr(w, 400, "异议议题必填")
		return
	}
	var id int64
	err := s.Pool.QueryRow(r.Context(),
		`INSERT INTO polls(building_id,title,content,created_by) VALUES ($1,$2,$3,$4) RETURNING id`,
		bid, in.Title, in.Content, u.ID).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id})
}

func (s *Server) listPolls(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT p.id, p.title, p.content, p.status, p.result, p.agree, p.disagree,
			COALESCE(c.name,''), p.created_at
		FROM polls p LEFT JOIN users c ON c.id=p.created_by
		WHERE p.building_id=$1 ORDER BY p.id DESC`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id int64
		var title, content, status, result, creator string
		var agree, disagree int
		var at any
		if err := rows.Scan(&id, &title, &content, &status, &result, &agree, &disagree, &creator, &at); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id": id, "title": title, "content": content, "status": status,
			"result": result, "agree": agree, "disagree": disagree,
			"creator": creator, "created_at": at,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) votePoll(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	pid := urlID(r, "pid")
	var in struct {
		Support bool `json:"support"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	var status string
	if err := s.Pool.QueryRow(r.Context(), `SELECT status FROM polls WHERE id=$1`, pid).Scan(&status); err != nil {
		writeErr(w, 404, "异议投票不存在")
		return
	}
	if status != "open" {
		writeErr(w, 409, "投票已结束")
		return
	}
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(),
		`DELETE FROM poll_votes WHERE poll_id=$1 AND user_id=$2`, pid, u.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO poll_votes(poll_id,user_id,support) VALUES ($1,$2,$3)`, pid, u.ID, in.Support); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE polls SET agree=(SELECT count(*) FROM poll_votes WHERE poll_id=$1 AND support),
			disagree=(SELECT count(*) FROM poll_votes WHERE poll_id=$1 AND NOT support) WHERE id=$1`, pid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// closePoll 关闭异议投票，同意 > 反对 视为异议成立（approved）
func (s *Server) closePoll(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	pid := urlID(r, "pid")
	var status string
	var agree, disagree int
	if err := s.Pool.QueryRow(r.Context(),
		`SELECT status, agree, disagree FROM polls WHERE id=$1`, pid).Scan(&status, &agree, &disagree); err != nil {
		writeErr(w, 404, "异议投票不存在")
		return
	}
	if status != "open" {
		writeErr(w, 409, "投票已结束")
		return
	}
	result := "approved"
	if agree <= disagree {
		result = "rejected"
	}
	if _, err := s.Pool.Exec(r.Context(),
		`UPDATE polls SET status='closed', result=$2 WHERE id=$1`, pid, result); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": result})
}

// ---------- 年度收益报告 ----------

func (s *Server) createAnnualReport(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		Year int `json:"year"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Year == 0 {
		writeErr(w, 400, "year 必填")
		return
	}
	prefix := fmt.Sprintf("%04d", in.Year)
	summary := map[string]any{}
	var kwh, gross, sub, dist, om, tax, woCost float64
	var finalizedMonths int
	err := s.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(total_kwh),0), COALESCE(SUM(gross_revenue),0), COALESCE(SUM(total_subsidy),0),
			COALESCE(SUM(distributable),0), COALESCE(SUM(om_fee),0), COALESCE(SUM(tax),0),
			COALESCE(SUM(work_order_cost),0), count(*)
		FROM settlement_periods
		WHERE building_id=$1 AND period_month LIKE $2 AND status='finalized'`,
		bid, prefix+"%").
		Scan(&kwh, &gross, &sub, &dist, &om, &tax, &woCost, &finalizedMonths)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if finalizedMonths == 0 {
		writeErr(w, 409, "该年度没有已定稿的月度结算，无法生成年报")
		return
	}
	summary["year"] = in.Year
	summary["finalized_months"] = finalizedMonths
	summary["total_kwh"] = kwh
	summary["carbon_kg"] = kwh * settlement.CarbonFactorKgPerKwh
	summary["gross_revenue"] = gross
	summary["total_subsidy"] = sub
	summary["distributable"] = dist
	summary["om_fee"] = om
	summary["tax"] = tax
	summary["work_order_cost"] = woCost

	// 住户年度分成
	rows, err := s.Pool.Query(r.Context(), `
		SELECT COALESCE(u.name,'公共账户'),
			COALESCE(SUM(si.amount) FILTER (WHERE si.item_type='shareholder'),0),
			COALESCE(SUM(si.kwh) FILTER (WHERE si.item_type='shareholder'),0)
		FROM settlement_items si
		JOIN settlement_periods sp ON sp.id=si.period_id
		LEFT JOIN users u ON u.id=si.user_id
		WHERE sp.building_id=$1 AND sp.period_month LIKE $2 AND sp.status='finalized'
		  AND si.item_type='shareholder'
		GROUP BY u.name ORDER BY 2 DESC`, bid, prefix+"%")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	holders := []any{}
	for rows.Next() {
		var name string
		var amt, hkwh float64
		if err := rows.Scan(&name, &amt, &hkwh); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		holders = append(holders, map[string]any{"name": name, "amount": amt, "kwh": hkwh})
	}
	summary["shareholder_earnings"] = holders

	// 年度维修记录
	woRows, err := s.Pool.Query(r.Context(), `
		SELECT period_month, title, wo_type, responsibility, cost, status
		FROM work_orders WHERE building_id=$1 AND period_month LIKE $2 ORDER BY period_month`,
		bid, prefix+"%")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer woRows.Close()
	wos := []any{}
	for woRows.Next() {
		var month, title, wtype, resp, st string
		var cost float64
		if err := woRows.Scan(&month, &title, &wtype, &resp, &cost, &st); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		wos = append(wos, map[string]any{
			"period_month": month, "title": title, "wo_type": wtype,
			"responsibility": resp, "cost": cost, "status": st,
		})
	}
	summary["work_orders"] = wos

	// 年度逆变器故障赔付记录（供应商质量赔付 / 物业通道延迟赔偿）
	fltRows, err := s.Pool.Query(r.Context(), `
		SELECT f.period_month, COALESCE(i.name,''), f.cause,
			f.lost_kwh, f.supplier_compensation, f.property_compensation,
			f.recovery_ratio, f.cover_ratio, f.paid
		FROM inverter_faults f LEFT JOIN inverters i ON i.id=f.inverter_id
		WHERE f.building_id=$1 AND f.period_month LIKE $2 AND f.assessed=TRUE
		ORDER BY f.period_month`,
		bid, prefix+"%")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer fltRows.Close()
	faults := []any{}
	var faultSup, faultProp float64
	for fltRows.Next() {
		var month, inv, cause string
		var lostKwh, sup, prop, recovery, cover float64
		var paid bool
		if err := fltRows.Scan(&month, &inv, &cause, &lostKwh, &sup, &prop, &recovery, &cover, &paid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		faultSup += sup
		faultProp += prop
		faults = append(faults, map[string]any{
			"period_month": month, "inverter": inv, "cause": cause,
			"lost_kwh": lostKwh, "supplier_compensation": sup, "property_compensation": prop,
			"recovery_ratio": recovery, "cover_ratio": cover, "paid": paid,
		})
	}
	summary["inverter_faults"] = faults
	summary["fault_supplier_compensation"] = faultSup
	summary["fault_property_compensation"] = faultProp

	var rid int
	err = s.Pool.QueryRow(r.Context(), `
		INSERT INTO annual_reports(building_id,year,summary,created_by)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (building_id,year) DO UPDATE SET summary=EXCLUDED.summary
		RETURNING id`, bid, in.Year, mustJSON(summary), u.ID).Scan(&rid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": rid, "summary": summary})
}

func (s *Server) listAnnualReports(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT id, year, published, created_at FROM annual_reports WHERE building_id=$1 ORDER BY year DESC`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, year int
		var published bool
		var at any
		if err := rows.Scan(&id, &year, &published, &at); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{"id": id, "year": year, "published": published, "created_at": at})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) getAnnualReport(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	rid := urlID(r, "rid")
	var bid, year int
	var published bool
	var summaryJSON string
	var at any
	err := s.Pool.QueryRow(r.Context(),
		`SELECT building_id, year, summary::text, published, created_at FROM annual_reports WHERE id=$1`, rid).
		Scan(&bid, &year, &summaryJSON, &published, &at)
	if isNoRows(err) {
		writeErr(w, 404, "年度报告不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var summary map[string]any
	_ = json.Unmarshal([]byte(summaryJSON), &summary)
	writeJSON(w, 200, map[string]any{
		"id": rid, "building_id": bid, "year": year,
		"published": published, "created_at": at, "summary": summary,
	})
}

func (s *Server) publishAnnualReport(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	rid := urlID(r, "rid")
	ct, err := s.Pool.Exec(r.Context(),
		`UPDATE annual_reports SET published=TRUE WHERE id=$1 AND published=FALSE`, rid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, 404, "报告不存在或已发布")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
