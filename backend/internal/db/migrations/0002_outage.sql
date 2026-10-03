-- 0002: 逆变器故障影响分摊
-- 设备保修信息、设备-组件绑定、日发电曲线、停机事件与损失分摊、同型号复查

-- 逆变器补充：型号 / 供应商 / 保修截止 / 安装日期
ALTER TABLE inverters
    ADD COLUMN IF NOT EXISTS model          TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS supplier       TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS warranty_until DATE,
    ADD COLUMN IF NOT EXISTS installed_at   DATE;

-- 设备绑定组件：某台逆变器下的组件份额归属到住户（按认购）或公共部分
-- 同一逆变器下 ratio 之和应等于其 capacity_ratio（允许绑定未完成时小于）
CREATE TABLE IF NOT EXISTS panel_bindings (
    id           BIGSERIAL PRIMARY KEY,
    building_id  BIGINT NOT NULL REFERENCES buildings(id) ON DELETE CASCADE,
    inverter_id  BIGINT NOT NULL REFERENCES inverters(id) ON DELETE CASCADE,
    user_id      BIGINT NOT NULL DEFAULT 0,         -- 0 = 公共部分组件
    panel_count  INT NOT NULL DEFAULT 0,               -- 绑定组件块数
    ratio        NUMERIC(7,4) NOT NULL DEFAULT 0,      -- 占整楼组件比例 %
    created_by   BIGINT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (inverter_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_bindings_building ON panel_bindings(building_id);

-- 日发电曲线（用于修复前后对比）。inverter_id 为空表示楼总表
CREATE TABLE IF NOT EXISTS generation_curve_points (
    id          BIGSERIAL PRIMARY KEY,
    building_id BIGINT NOT NULL REFERENCES buildings(id) ON DELETE CASCADE,
    inverter_id BIGINT REFERENCES inverters(id) ON DELETE CASCADE,
    day         DATE NOT NULL,
    kwh         NUMERIC(12,2) NOT NULL DEFAULT 0 CHECK (kwh >= 0),
    phase       TEXT NOT NULL DEFAULT 'normal'
        CHECK (phase IN ('normal','outage','recovery')),  -- 正常/停机/修复后
    note        TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (building_id, inverter_id, day)
);
CREATE INDEX IF NOT EXISTS idx_curve_day ON generation_curve_points(building_id, day);

-- 逆变器停机事件（一次故障的影响分摊主单）
CREATE TABLE IF NOT EXISTS inverter_outage_events (
    id                 BIGSERIAL PRIMARY KEY,
    building_id        BIGINT NOT NULL REFERENCES buildings(id) ON DELETE CASCADE,
    inverter_id        BIGINT NOT NULL REFERENCES inverters(id) ON DELETE CASCADE,
    work_order_id      BIGINT REFERENCES work_orders(id) ON DELETE SET NULL,
    title              TEXT NOT NULL DEFAULT '',
    period_month       TEXT NOT NULL CHECK (period_month ~ '^\d{4}-\d{2}$'),
    fault_at           TIMESTAMPTZ NOT NULL,          -- 停机开始
    responded_at       TIMESTAMPTZ,                   -- 运维到场（响应时间）
    resolved_at        TIMESTAMPTZ,                   -- 修复完成
    -- 运维响应 SLA（小时）：到场时限；保修条款
    response_sla_hours NUMERIC(6,1) NOT NULL DEFAULT 24,
    -- 责任判定：quality 设备质量（保修）/ channel 维护通道占用延迟 / mixed 两者皆有 / pending 待判定
    cause_type         TEXT NOT NULL DEFAULT 'pending'
        CHECK (cause_type IN ('pending','quality','channel','mixed')),
    channel_blocked_at TIMESTAMPTZ,                   -- 物业维护通道开始占用时刻
    channel_freed_at   TIMESTAMPTZ,                   -- 通道释放时刻
    under_warranty     BOOLEAN NOT NULL DEFAULT FALSE,-- 故障发生时是否在保修期
    warranty_note      TEXT NOT NULL DEFAULT '',      -- 保修条款说明
    -- 测算
    baseline_kwh_per_day  NUMERIC(10,2) NOT NULL DEFAULT 0, -- 正常时日发电基线
    lost_kwh_total        NUMERIC(12,2) NOT NULL DEFAULT 0, -- 停机损失电量合计
    lost_kwh_quality      NUMERIC(12,2) NOT NULL DEFAULT 0, -- 设备质量段损失电量
    lost_kwh_channel      NUMERIC(12,2) NOT NULL DEFAULT 0, -- 通道占用延迟段损失电量
    lost_kwh_pending      NUMERIC(12,2) NOT NULL DEFAULT 0, -- 责任未决段损失电量
    grid_price            NUMERIC(8,4) NOT NULL DEFAULT 0,  -- 上网电价
    subsidy_per_kwh       NUMERIC(8,4) NOT NULL DEFAULT 0,  -- 度电补贴
    loss_amount_total     NUMERIC(12,2) NOT NULL DEFAULT 0,
    -- 实际赔付
    supplier_paid_amount  NUMERIC(12,2) NOT NULL DEFAULT 0, -- 供应商已实际赔付金额
    property_paid_amount  NUMERIC(12,2) NOT NULL DEFAULT 0, -- 物业（公共账）实际承担金额
    posted_to_ledger      BOOLEAN NOT NULL DEFAULT FALSE,    -- 是否已入账（住户补偿+公共账说明）
    -- 修复前后曲线复核
    recovery_baseline_avg NUMERIC(10,2) NOT NULL DEFAULT 0,
    recovery_after_avg    NUMERIC(10,2) NOT NULL DEFAULT 0,
    recovery_rate         NUMERIC(7,4) NOT NULL DEFAULT 0,  -- 修复后/基线 %
    actual_extra_loss_kwh NUMERIC(12,2) NOT NULL DEFAULT 0, -- 复核发现的持续欠发
    cover_status          TEXT NOT NULL DEFAULT 'pending'
        CHECK (cover_status IN ('pending','covered','shortfall','surplus')),
    review_note           TEXT NOT NULL DEFAULT '',
    reviewed_by           BIGINT REFERENCES users(id),
    reviewed_at           TIMESTAMPTZ,
    status            TEXT NOT NULL DEFAULT 'open'
        CHECK (status IN ('open','assessed','recovered','closed')),
    created_by        BIGINT REFERENCES users(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_outage_building ON inverter_outage_events(building_id);
CREATE INDEX IF NOT EXISTS idx_outage_inverter ON inverter_outage_events(inverter_id);

-- 停机损失的住户/公共分摊行
CREATE TABLE IF NOT EXISTS outage_allocations (
    id                BIGSERIAL PRIMARY KEY,
    event_id          BIGINT NOT NULL REFERENCES inverter_outage_events(id) ON DELETE CASCADE,
    user_id           BIGINT NOT NULL DEFAULT 0,     -- 0 = 公共部分
    ratio             NUMERIC(7,4) NOT NULL DEFAULT 0, -- 该住户在该逆变器绑定组件中的占比 %
    panel_count       INT NOT NULL DEFAULT 0,
    lost_kwh          NUMERIC(12,2) NOT NULL DEFAULT 0,
    -- 按责任拆分的损失金额（元）
    loss_quality      NUMERIC(12,2) NOT NULL DEFAULT 0,
    loss_channel      NUMERIC(12,2) NOT NULL DEFAULT 0,
    loss_pending      NUMERIC(12,2) NOT NULL DEFAULT 0,
    loss_total        NUMERIC(12,2) NOT NULL DEFAULT 0,
    -- 供应商赔付（设备质量部分）到户
    supplier_due      NUMERIC(12,2) NOT NULL DEFAULT 0,
    supplier_paid     NUMERIC(12,2) NOT NULL DEFAULT 0,
    -- 物业责任部分赔到户（经公共账）；公共份额留存公共账
    property_paid     NUMERIC(12,2) NOT NULL DEFAULT 0,
    -- 物业责任部分只做公共账说明，不再向住户二次分摊（由公共账承担）
    property_note     TEXT NOT NULL DEFAULT '',
    settled           BOOLEAN NOT NULL DEFAULT FALSE,  -- 赔付是否已入账
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_out_alloc_event ON outage_allocations(event_id);

-- 同型号设备复查提醒
CREATE TABLE IF NOT EXISTS outage_rechecks (
    id          BIGSERIAL PRIMARY KEY,
    building_id BIGINT NOT NULL REFERENCES buildings(id) ON DELETE CASCADE,
    event_id    BIGINT NOT NULL REFERENCES inverter_outage_events(id) ON DELETE CASCADE,
    inverter_id BIGINT NOT NULL REFERENCES inverters(id) ON DELETE CASCADE, -- 被提醒复查的设备
    reason      TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','done','dismissed')),
    result      TEXT NOT NULL DEFAULT '',
    handled_by  BIGINT REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    handled_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_recheck_building ON outage_rechecks(building_id, status);
