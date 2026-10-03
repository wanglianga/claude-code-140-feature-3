#!/usr/bin/env python3
"""端到端 API 验证脚本（依赖 python3，无第三方包）。

用法：
    python3 scripts/verify_api.py [BASE_URL]

前置：服务已启动且数据库为初始种子状态（SEED_ON_START=true 首次启动后的干净库）。
脚本会校验：驾驶舱 → 9 月结算（维修基金动用+分摊）→ 定稿入账 → 房屋转让 →
次月分成归属切换 → 公告/异议/异常解释/年报 → 阶段回退原因强制。
任一断言失败立即退出（exit 1）。
"""
import json
import sys
import urllib.request
import urllib.error

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080"


def call(method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req) as r:
            raw = r.read().decode()
            return r.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {"raw": raw}


def check(name, cond, detail=""):
    if not cond:
        print(f"FAIL: {name} {detail}")
        sys.exit(1)
    print(f"PASS: {name} {detail}")


def approx(a, b, eps=0.01):
    return abs(float(a) - float(b)) < eps


def login(phone, pwd):
    st, r = call("POST", "/api/auth/login", body={"phone": phone, "password": pwd})
    check(f"login {phone}", st == 200)
    return r["token"]


def main():
    prop = login("13800000001", "property123")
    zhang = login("13900000001", "resident123")
    liu = login("13900000006", "resident123")

    st, r = call("GET", "/api/buildings", prop)
    b1 = next(b for b in r["items"] if "1 号楼" in b["name"])
    bid = b1["id"]
    print(f"-- 目标楼栋：{b1['name']} (id={bid})，阶段 {b1['stage']}")

    # A. 8 月已定稿的驾驶舱
    st, d = call("GET", f"/api/buildings/{bid}/dashboard", prop)
    check("8月后公共账户余额=85631.00", approx(d["public_balance"], 85631.00), str(d["public_balance"]))
    check("维修基金余额=97.86", approx(d["maintenance_fund_balance"], 97.86), str(d["maintenance_fund_balance"]))
    check("8月累计发电4150", approx(d["total_kwh"], 4150))
    check("碳减排2411.15kg", approx(d["carbon_kg"], 2411.15))

    # B. 9 月试算：2600kWh，漏水 600 物业责任，基金期初 97.86
    st, r = call("POST", f"/api/buildings/{bid}/settlements", prop, {
        "period_month": "2026-09", "grid_price": 0.4153, "subsidy_per_kwh": 0.05,
        "om_fee": 200, "tax": 120,
    })
    s = r["settlement"]
    check("9月动用基金97.86", approx(s["fund_used"], 97.86), str(s["fund_used"]))
    check("9月维修分摊502.14", approx(s["repair_deduct"], 502.14), str(s["repair_deduct"]))
    check("9月计提基金23.26", approx(s["fund_addition"], 23.26), str(s["fund_addition"]))
    zhang_item = next(i for i in s["items"] if i["item_type"] == "shareholder" and i["name"].startswith("张伟"))
    check("张伟9月分成116.29", approx(zhang_item["amount"], 116.29), str(zhang_item["amount"]))
    spid = r["id"]

    # C. 定稿
    st, _ = call("POST", f"/api/settlements/{spid}/finalize", prop)
    check("9月结算定稿", st == 200)
    st, _ = call("POST", f"/api/buildings/{bid}/settlements", prop,
                 {"period_month": "2026-09", "grid_price": 0.5})
    check("定稿后禁止重算(409)", st == 409, str(st))
    st, me = call("GET", "/api/me", zhang)
    check("张伟余额=8月489.30+9月116.29=605.59", approx(me["balance"], 605.59), str(me["balance"]))
    st, lg = call("GET", f"/api/buildings/{bid}/ledger", prop)
    check("9月后公共账户=85920.78", approx(lg["public_balance"], 85920.78), str(lg["public_balance"]))
    check("9月后维修基金=23.26", approx(lg["maintenance_fund_balance"], 23.26), str(lg["maintenance_fund_balance"]))

    # C2. 9 月 2# 逆变器质量故障：供应商保修赔付进入住户收益补偿
    st, fl = call("GET", f"/api/buildings/{bid}/faults", prop)
    check("故障台账1条", st == 200 and len(fl["items"]) == 1, str(len(fl["items"])))
    fid = fl["items"][0]["id"]
    st, f = call("GET", f"/api/faults/{fid}", prop)
    check("故障停机168h/响应6h/SLA达标", approx(f["downtime_hours"], 168) and approx(f["response_hours"], 6) and f["sla_met"] is True)
    check("责任=设备质量且保修内", f["cause"] == "quality" and f["warranty_covered"] is True)
    check("损失电量490kWh", approx(f["lost_kwh"], 490), str(f["lost_kwh"]))
    check("供应商赔付228.00", approx(f["supplier_compensation"], 228.00), str(f["supplier_compensation"]))
    check("物业赔付为0", approx(f["property_compensation"], 0))
    check("曲线已对比且覆盖", f["compare_status"] == "covered" and approx(f["recovery_ratio"], 97.14) and approx(f["cover_ratio"], 100))
    check("待复查同型号1#逆变器", f["review_needed"] and (f["same_model_names"] or [""])[0] == "1#逆变器", str(f["same_model_names"]))
    item_by_name = {i["name"]: i for i in f["items"]}
    check("张伟质量赔付68.40", approx(item_by_name["张伟(顶楼)"]["supplier_amount"], 68.40))
    check("李娜质量赔付57.00", approx(item_by_name["李娜"]["supplier_amount"], 57.00))
    check("王强质量赔付34.20", approx(item_by_name["王强"]["supplier_amount"], 34.20))
    pub_item = next(i for i in f["items"] if i["party"] == "public")
    check("未认购公共组件赔付68.40入公共账", approx(pub_item["supplier_amount"], 68.40))
    # 未做曲线/已发放保护：种子已对比为 covered，直接发放；重复发放 409
    st, paid = call("POST", f"/api/faults/{fid}/pay", prop)
    check("发放供应商赔付", st == 200 and paid["paid"] is True, str(st))
    st, _ = call("POST", f"/api/faults/{fid}/pay", prop)
    check("故障赔付不可重复发放(409)", st == 409, str(st))
    st, me = call("GET", "/api/me", zhang)
    check("张伟余额=605.59+68.40=673.99", approx(me["balance"], 673.99), str(me["balance"]))
    st, lg = call("GET", f"/api/buildings/{bid}/ledger", prop)
    check("发放后公共账户=85989.18(+公共组件68.40)", approx(lg["public_balance"], 85989.18), str(lg["public_balance"]))
    st, _ = call("POST", f"/api/faults/{fid}/review", prop, {"note": "已巡检1#逆变器同型号模块"})
    check("同型号设备复查登记", st == 200, str(st))

    # C3. 10 月 1# 逆变器：屋顶维护通道占用致维修延迟，物业赔偿进公共账后代发住户
    st, invall = call("GET", f"/api/buildings/{bid}/inverters", prop)
    inv1_id = next(i["id"] for i in invall["items"] if i["name"] == "1#逆变器")
    st, cf = call("POST", f"/api/buildings/{bid}/faults", prop, {
        "inverter_id": inv1_id, "period_month": "2026-10",
        "title": "屋顶维护通道被占用无法进场维修", "baseline_daily_kwh": 70,
        "grid_price": 0.4153, "subsidy_per_kwh": 0.05,
        "fault_at": "2026-10-20T08:00:00+08:00",
    })
    check("登记通道责任故障", st == 201, str(st))
    cfid = cf["id"]
    call("POST", f"/api/faults/{cfid}/respond", prop, {"responded_at": "2026-10-20T12:00:00+08:00"})
    st, rr = call("POST", f"/api/faults/{cfid}/resolve", prop, {"resolved_at": "2026-10-21T08:00:00+08:00"})
    check("通道故障停机24h", st == 200 and approx(rr["downtime_hours"], 24), str(rr))
    st, fa = call("POST", f"/api/faults/{cfid}/assess", prop, {"cause": "channel"})
    check("物业赔偿32.57入公共账", st == 200 and approx(fa["property_compensation"], 32.57), str(fa.get("property_compensation")))
    check("通道责任供应商不赔", approx(fa["supplier_compensation"], 0) and fa["review_needed"] is False)
    st, _ = call("POST", f"/api/faults/{cfid}/pay", prop)
    check("未对比曲线不得发放(409)", st == 409, str(st))
    st, cc = call("POST", f"/api/faults/{cfid}/compare-curve", prop, {"post_daily_kwh": 70})
    check("曲线对比覆盖", st == 200 and cc["compare_status"] == "covered",
          f"{cc.get('compare_status')} 恢复率{cc.get('recovery_ratio')}%")
    st, _ = call("POST", f"/api/faults/{cfid}/pay", prop)
    check("物业赔偿发放", st == 200, str(st))
    st, lg = call("GET", f"/api/buildings/{bid}/ledger?period_month=2026-10", prop)
    fc = [x for x in lg["items"] if x["category"] == "fault_comp"]
    check("公共账先收物业赔偿32.57", any(x["party"] == "公共账户" and x["direction"] == "in" and approx(x["amount"], 32.57) for x in fc))
    check("公共账代发住户22.80(70%)", approx(sum(x["amount"] for x in fc if x["party"] == "公共账户" and x["direction"] == "out"), 22.80))
    zhang_fc = [x for x in fc if x["party"].startswith("张伟")]
    check("张伟获通道赔偿9.77(30%)", approx(sum(x["amount"] for x in zhang_fc), 9.77))
    st, me = call("GET", "/api/me", zhang)
    check("张伟余额=673.99+9.77=683.76", approx(me["balance"], 683.76), str(me["balance"]))

    # D. 房屋转让：张伟 -> 刘洋
    st, subs = call("GET", f"/api/buildings/{bid}/subscriptions", prop)
    sid = next(x["id"] for x in subs["items"] if x["name"].startswith("张伟") and x["type"] == "shareholder")
    st, users = call("GET", "/api/users?q=13900000006", prop)
    liu_id = next(u["id"] for u in users["items"] if u["phone"] == "13900000006")
    st, tr = call("POST", f"/api/buildings/{bid}/transfers", zhang, {
        "subscription_id": sid, "to_user_id": liu_id, "rights_transfer": True,
    })
    check("发起转让", st == 201 and tr["history_settled"] is True, str(tr))
    tid = tr["id"]
    st, _ = call("POST", f"/api/transfers/{tid}/confirm", prop)
    check("物业确认转让", st == 200)

    # E. 10 月发电与结算：30% 份额应归刘洋
    st, invs = call("GET", f"/api/buildings/{bid}/inverters", prop)
    for iv in invs["items"]:
        st, _ = call("POST", f"/api/buildings/{bid}/generation", prop, {
            "period_month": "2026-10", "inverter_id": iv["id"], "kwh": 1200,
        })
        check(f"录入{iv['name']}10月电量", st == 200)
    st, r = call("POST", f"/api/buildings/{bid}/settlements", prop, {
        "period_month": "2026-10", "grid_price": 0.4153, "subsidy_per_kwh": 0.05,
    })
    check("10月结算生成", st == 200, str(r)[:200])
    octid = r["id"]
    st, detail = call("GET", f"/api/settlements/{octid}", prop)
    holders = {i["name"]: i["amount"] for i in detail["items"] if i["item_type"] == "shareholder"}
    check("张伟已无10月分成", not any(k.startswith("张伟") for k in holders), str(holders))
    check("刘洋(新业主)获30%分成335.02", approx(holders.get("刘洋(新业主)", 0), 335.02), str(holders))
    call("POST", f"/api/settlements/{octid}/finalize", prop)
    st, me = call("GET", "/api/me", liu)
    check("刘洋余额=335.02", approx(me["balance"], 335.02), str(me["balance"]))
    st, me = call("GET", "/api/me", zhang)
    check("张伟余额保持683.76（历史收益+故障赔付，不随转让转移）", approx(me["balance"], 683.76), str(me["balance"]))

    # F. 公告 / 异议 / 异常解释 / 年报
    st, _ = call("POST", f"/api/buildings/{bid}/announcements", prop,
                 {"title": "10月公示", "content": "见结算", "pinned": True})
    check("发布公告", st == 201)
    st, poll = call("POST", f"/api/buildings/{bid}/polls", zhang,
                    {"title": "验证用异议", "content": "测试"})
    pid = poll["id"]
    call("POST", f"/api/polls/{pid}/vote", zhang, {"support": True})
    call("POST", f"/api/polls/{pid}/vote", liu, {"support": True})
    st, r = call("POST", f"/api/polls/{pid}/close", prop)
    check("异议投票成立", r["result"] == "approved", str(r))
    st, al = call("POST", f"/api/buildings/{bid}/alerts", zhang,
                  {"period_month": "2026-10", "reason": "脚本验证异常"})
    aid = al["id"]
    st, _ = call("POST", f"/api/alerts/{aid}/explain", prop, {"explanation": "脚本验证：计划检修 1 天"})
    check("异常解释", st == 200)
    st, r = call("POST", f"/api/buildings/{bid}/annual-reports", prop, {"year": 2026})
    check("年度报告生成（3个定稿月份）", st == 200 and r["summary"]["finalized_months"] == 3, str(st))
    rid = r["id"]
    st, _ = call("POST", f"/api/annual-reports/{rid}/publish", prop)
    check("年报发布", st == 200)

    # G. 阶段回退必须有原因
    st, _ = call("POST", f"/api/buildings/{bid}/stage", prop, {"to_stage": "construction"})
    check("无原因回退被拒(409)", st == 409, str(st))
    st, r = call("POST", f"/api/buildings/{bid}/stage", prop,
                 {"to_stage": "construction", "reason": "设备供应延迟，组件到货推迟"})
    check("有原因回退成功", st == 200, str(r))

    print("\n全部端到端断言通过 ✅")


if __name__ == "__main__":
    main()
