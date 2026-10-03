package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"pvplatform/internal/auth"
)

// ---------- 时间/金额辅助 ----------

// parseFlexTime 支持 RFC3339、"2006-01-02 15:04"、"2006-01-02"
func parseFlexTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05", "2006-01-02T15:04", // <input type="datetime-local">
		"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02",
	}
	var loc = time.Local
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// timeArg 解析可选时间字段；空串→nil，非法→错误
func timeArg(m map[string]any, key string) (*time.Time, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	s, _ := v.(string)
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	t, ok := parseFlexTime(s)
	if !ok {
		return nil, fmt.Errorf("%s 时间格式不合法", key)
	}
	return &t, nil
}

func numArg(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case float64:
			return x
		case int:
			return float64(x)
		}
	}
	return 0
}

func strArg(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// ---------- 逆变器保修信息 ----------

// updateInverter 更新设备档案（名称/型号/供应商/保修期/安装日期）。
// 字段缺省=不改；warranty_until / installed_at 传空串表示清空。
func (s *Server) updateInverter(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	iid := urlID(r, "iid")
	var in struct {
		Name          string   `json:"name"`
		Model         **string `json:"model"`
		Supplier      **string `json:"supplier"`
		WarrantyUntil *string  `json:"warranty_until"` // YYYY-MM-DD；空串清空；缺省不改
		InstalledAt   *string  `json:"installed_at"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	var exists int
	if err := s.Pool.QueryRow(r.Context(), `SELECT 1 FROM inverters WHERE id=$1`, iid).Scan(&exists); err != nil {
		writeErr(w, 404, "逆变器不存在")
		return
	}

	set := []string{}
	args := []any{iid}
	add := func(col string, idx int, val any) {
		set = append(set, fmt.Sprintf("%s=$%d", col, idx))
		args = append(args, val)
	}
	if in.Name != "" {
		add("name", len(args)+1, in.Name)
	}
	if in.Model != nil {
		add("model", len(args)+1, **in.Model)
	}
	if in.Supplier != nil {
		add("supplier", len(args)+1, **in.Supplier)
	}
	if in.WarrantyUntil != nil {
		if *in.WarrantyUntil == "" {
			set = append(set, "warranty_until=NULL")
		} else {
			t, err := time.Parse("2006-01-02", *in.WarrantyUntil)
			if err != nil {
				writeErr(w, 400, "保修截止日期格式应为 YYYY-MM-DD")
				return
			}
			add("warranty_until", len(args)+1, t)
		}
	}
	if in.InstalledAt != nil {
		if *in.InstalledAt == "" {
			set = append(set, "installed_at=NULL")
		} else {
			t, err := time.Parse("2006-01-02", *in.InstalledAt)
			if err != nil {
				writeErr(w, 400, "安装日期格式应为 YYYY-MM-DD")
				return
			}
			add("installed_at", len(args)+1, t)
		}
	}
	if len(set) == 0 {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if _, err := s.Pool.Exec(r.Context(),
		fmt.Sprintf("UPDATE inverters SET %s WHERE id=$1", strings.Join(set, ",")), args...); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 设备绑定组件 ----------

func (s *Server) listBindings(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT b.id, b.inverter_id, i.name, b.user_id, COALESCE(u.name,'公共部分'),
		       b.panel_count, b.ratio
		FROM panel_bindings b
		JOIN inverters i ON i.id=b.inverter_id
		LEFT JOIN users u ON u.id=b.user_id
		WHERE b.building_id=$1
		ORDER BY b.inverter_id, (b.user_id=0), b.id`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id, iid, uid int64
		var invName, name string
		var panels int
		var ratio float64
		if err := rows.Scan(&id, &iid, &invName, &uid, &name, &panels, &ratio); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		items = append(items, map[string]any{
			"id": id, "inverter_id": iid, "inverter_name": invName, "user_id": uid,
			"name": name, "panel_count": panels, "ratio": ratio,
		})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// upsertBinding 绑定/调整逆变器下某住户（或公共部分）的组件份额
func (s *Server) upsertBinding(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		InverterID int64   `json:"inverter_id"`
		UserID     int64   `json:"user_id"` // 0 = 公共部分
		PanelCount int     `json:"panel_count"`
		Ratio      float64 `json:"ratio"`
	}
	if err := decodeJSON(r, &in); err != nil || in.InverterID == 0 {
		writeErr(w, 400, "inverter_id 必填")
		return
	}
	if in.PanelCount < 0 || in.Ratio < 0 {
		writeErr(w, 400, "组件数/比例不能为负")
		return
	}
	var invBuilding int64
	if err := s.Pool.QueryRow(r.Context(),
		`SELECT building_id FROM inverters WHERE id=$1`, in.InverterID).Scan(&invBuilding); err != nil || invBuilding != bid {
		writeErr(w, 404, "逆变器不存在或不属于该楼栋")
		return
	}
	var id int64
	err := s.Pool.QueryRow(r.Context(), `
		INSERT INTO panel_bindings(building_id,inverter_id,user_id,panel_count,ratio,created_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (inverter_id,user_id) DO UPDATE SET
			panel_count=EXCLUDED.panel_count, ratio=EXCLUDED.ratio
		RETURNING id`,
		bid, in.InverterID, in.UserID, in.PanelCount, in.Ratio, u.ID).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) deleteBinding(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	id := urlID(r, "bid")
	if _, err := s.Pool.Exec(r.Context(), `DELETE FROM panel_bindings WHERE id=$1`, id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// autoBindings 按当前认购份额，把各住户组件按逆变器装机比例自动绑定到设备（公共部分取认购余量）。
func (s *Server) autoBindings(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	bid := urlID(r, "id")

	var cnt int
	_ = s.Pool.QueryRow(r.Context(), `SELECT count(*) FROM panel_bindings WHERE building_id=$1`, bid).Scan(&cnt)
	if cnt > 0 {
		writeErr(w, 409, "已存在绑定记录，请先清空再自动生成")
		return
	}

	type inv struct {
		ID    int64
		Ratio float64
	}
	invRows, err := s.Pool.Query(r.Context(),
		`SELECT id, capacity_ratio FROM inverters WHERE building_id=$1 ORDER BY id`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var invs []inv
	var invRatioSum float64
	for invRows.Next() {
		var v inv
		if err := invRows.Scan(&v.ID, &v.Ratio); err != nil {
			invRows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		invs = append(invs, v)
		invRatioSum += v.Ratio
	}
	invRows.Close()
	if len(invs) == 0 || invRatioSum <= 0 {
		writeErr(w, 409, "请先登记逆变器及其装机占比")
		return
	}

	// 当前权益持有人（paid/transferred）
	subRows, err := s.Pool.Query(r.Context(), `
		SELECT user_id, panel_count, panel_ratio FROM subscriptions
		WHERE building_id=$1 AND sub_type='shareholder' AND status IN ('paid','transferred')
		ORDER BY id`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	type holder struct {
		UserID int64
		Panels int
		Ratio  float64
	}
	var holders []holder
	var totalPanels int
	var holderRatio float64
	for subRows.Next() {
		var h holder
		if err := subRows.Scan(&h.UserID, &h.Panels, &h.Ratio); err != nil {
			subRows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		holders = append(holders, h)
		totalPanels += h.Panels
		holderRatio += h.Ratio
	}
	subRows.Close()

	var panelTotal int
	_ = s.Pool.QueryRow(r.Context(), `SELECT panel_total FROM buildings WHERE id=$1`, bid).Scan(&panelTotal)
	publicPanels := panelTotal - totalPanels
	publicRatio := 100 - holderRatio

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	// 对每个持有人（含公共部分，uid=0），按逆变器装机占比拆分组件数，余数依次补给占比最高的逆变器
	distribute := func(uid int64, panels int, ratio float64) error {
		if panels <= 0 && ratio <= 0 {
			return nil
		}
		assigned := make([]int, len(invs))
		rem := panels
		for i, v := range invs {
			n := int(float64(panels) * v.Ratio / invRatioSum)
			assigned[i] = n
			rem -= n
		}
		// 余数按逆变器占比从高到低补
		order := make([]int, len(invs))
		for i := range order {
			order[i] = i
		}
		for i := 0; i < len(order); i++ {
			for j := i + 1; j < len(order); j++ {
				if invs[order[j]].Ratio > invs[order[i]].Ratio {
					order[i], order[j] = order[j], order[i]
				}
			}
		}
		for k := 0; rem > 0 && k < len(order); k++ {
			assigned[order[k]]++
			rem--
		}
		for i, v := range invs {
			if assigned[i] <= 0 {
				continue
			}
			ratioVal := float64(assigned[i]) / float64(panelTotal) * 100
			if _, err := tx.Exec(r.Context(), `
				INSERT INTO panel_bindings(building_id,inverter_id,user_id,panel_count,ratio,created_by)
				VALUES ($1,$2,$3,$4,$5,$6)`,
				bid, v.ID, uid, assigned[i], r2(ratioVal), 0); err != nil {
				return err
			}
		}
		return nil
	}
	for _, h := range holders {
		if err := distribute(h.UserID, h.Panels, h.Ratio); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if publicPanels > 0 || publicRatio > 0 {
		if err := distribute(0, publicPanels, publicRatio); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func r2(x float64) float64 { return float64(int(x*10000+0.5)) / 10000 }

// ---------- 日发电曲线 ----------

func (s *Server) listCurve(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	inv := r.URL.Query().Get("inverter_id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT COALESCE(inverter_id,0), day::text, kwh, phase, COALESCE(note,'')
		FROM generation_curve_points
		WHERE building_id=$1 AND ($2='' OR inverter_id::text=$2 OR ($2='0' AND inverter_id IS NULL))
		ORDER BY day`, bid, inv)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var iid int64
		var day, phase, note string
		var kwh float64
		if err := rows.Scan(&iid, &day, &kwh, &phase, &note); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		items = append(items, map[string]any{
			"inverter_id": iid, "day": day, "kwh": kwh, "phase": phase, "note": note,
		})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) upsertCurve(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		InverterID int64   `json:"inverter_id"`
		Day        string  `json:"day"`
		Kwh        float64 `json:"kwh"`
		Phase      string  `json:"phase"`
		Note       string  `json:"note"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	day, err := time.Parse("2006-01-02", in.Day)
	if err != nil {
		writeErr(w, 400, "day 格式应为 YYYY-MM-DD")
		return
	}
	if in.Phase == "" {
		in.Phase = "normal"
	}
	if in.Phase != "normal" && in.Phase != "outage" && in.Phase != "recovery" {
		writeErr(w, 400, "phase 只能是 normal/outage/recovery")
		return
	}
	var id int64
	err = s.Pool.QueryRow(r.Context(), `
		INSERT INTO generation_curve_points(building_id,inverter_id,day,kwh,phase,note)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (building_id,inverter_id,day)
		DO UPDATE SET kwh=EXCLUDED.kwh, phase=EXCLUDED.phase, note=EXCLUDED.note
		RETURNING id`,
		bid, nullableID(in.InverterID), day, in.Kwh, in.Phase, in.Note).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

// batchCurve 批量录入曲线点 [{"day":"2026-09-01","inverter_id":2,"kwh":66.5,"phase":"normal"}, ...]
func (s *Server) batchCurve(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in []struct {
		InverterID int64   `json:"inverter_id"`
		Day        string  `json:"day"`
		Kwh        float64 `json:"kwh"`
		Phase      string  `json:"phase"`
	}
	if err := decodeJSON(r, &in); err != nil || len(in) == 0 {
		writeErr(w, 400, "请求体应为非空曲线点数组")
		return
	}
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	n := 0
	for _, p := range in {
		day, err := time.Parse("2006-01-02", p.Day)
		if err != nil {
			writeErr(w, 400, "day 格式应为 YYYY-MM-DD: "+p.Day)
			return
		}
		phase := p.Phase
		if phase == "" {
			phase = "normal"
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO generation_curve_points(building_id,inverter_id,day,kwh,phase)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (building_id,inverter_id,day)
			DO UPDATE SET kwh=EXCLUDED.kwh, phase=EXCLUDED.phase`,
			bid, nullableID(p.InverterID), day, p.Kwh, phase); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		n++
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"inserted": n})
}
