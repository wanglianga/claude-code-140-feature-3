-- 逆变器故障影响分摊：设备档案（型号/供应商/保修条款）+ 故障台账 + 住户损失明细
-- 金额单位: 元; 比例单位: % (0~100), numeric 精确存储

-- 1) 逆变器设备档案补充：型号、供应商、保修期、安装日期（保修条款判定依据）
ALTER TABLE inverters
    ADD COLUMN model           TEXT NOT NULL DEFAULT '',
    ADD COLUMN supplier        TEXT NOT NULL DEFAULT '',
    ADD COLUMN warranty_months INT  NOT NULL DEFAULT 0,   -- 整机保修月数
    ADD COLUMN install_date    DATE;

-- 2) 逆变器故障台账：停机时长、运维响应、责任（质量/通道占用）、保修评估、补偿、曲线对比、同型号复查
CREATE TABLE inverter_faults (
    id                    BIGSERIAL PRIMARY KEY,
    building_id           BIGINT NOT NULL REFERENCES buildings(id) ON DELETE CASCADE,
    inverter_id           BIGINT NOT NULL REFERENCES inverters(id) ON DELETE CASCADE,
    period_month          TEXT NOT NULL CHECK (period_month ~ '^\d{4}-\d{2}$'),
    title                 TEXT NOT NULL,
    description           TEXT NOT NULL DEFAULT '',
    status                TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','responded','resolved')),

    -- 停机与运维响应时间线
    fault_at              TIMESTAMPTZ NOT NULL DEFAULT now(),  -- 停机时间
    responded_at          TIMESTAMPTZ,                         -- 运维到场响应时间
    resolved_at           TIMESTAMPTZ,                         -- 修复完成时间
    downtime_hours        NUMERIC(10,2) NOT NULL DEFAULT 0,    -- 停机总时长（故障→修复）
    response_hours        NUMERIC(10,2) NOT NULL DEFAULT 0,    -- 运维响应时长（故障→到场）
    sla_response_hours    NUMERIC(10,2) NOT NULL DEFAULT 24,   -- 保修条款承诺响应时长
    sla_met               BOOLEAN NOT NULL DEFAULT TRUE,       -- 响应是否满足 SLA

    -- 责任判定：设备质量 / 屋顶维护通道占用致维修延迟 / 混合
    cause                 TEXT NOT NULL DEFAULT 'undetermined'
        CHECK (cause IN ('undetermined','quality','channel','mixed')),
    delay_hours           NUMERIC(10,2) NOT NULL DEFAULT 0,    -- 通道占用导致的维修延迟小时数
    quality_hours         NUMERIC(10,2) NOT NULL DEFAULT 0,    -- 设备质量导致的停机小时数

    -- 损失电量与收益损失
    baseline_daily_kwh    NUMERIC(12,2) NOT NULL DEFAULT 0,    -- 故障前日均发电量（基准曲线）
    grid_price            NUMERIC(8,4)  NOT NULL DEFAULT 0,    -- 损失电量按上网电价计价
    subsidy_per_kwh       NUMERIC(8,4)  NOT NULL DEFAULT 0,    -- 损失电量对应的度电补贴
    lost_kwh              NUMERIC(12,2) NOT NULL DEFAULT 0,    -- 总损失电量
    quality_lost_kwh      NUMERIC(12,2) NOT NULL DEFAULT 0,    -- 质量时段损失电量
    channel_lost_kwh      NUMERIC(12,2) NOT NULL DEFAULT 0,    -- 通道延迟时段损失电量
    total_loss            NUMERIC(12,2) NOT NULL DEFAULT 0,    -- 总收益损失（电费+补贴）

    -- 保修条款与赔付归集
    warranty_covered      BOOLEAN NOT NULL DEFAULT FALSE,      -- 质量损失是否在保修范围内由供应商赔付
    warranty_note         TEXT NOT NULL DEFAULT '',
    quality_coverage_pct  NUMERIC(7,2)  NOT NULL DEFAULT 100,  -- 保修条款确认供应商承担的质量损失比例 %
    supplier_compensation NUMERIC(12,2) NOT NULL DEFAULT 0,    -- 供应商应赔付（进入住户收益补偿）
    property_compensation NUMERIC(12,2) NOT NULL DEFAULT 0,    -- 物业责任赔偿（进入楼栋公共账说明）
    assessed              BOOLEAN NOT NULL DEFAULT FALSE,
    assessed_at           TIMESTAMPTZ,
    assessed_by           BIGINT REFERENCES users(id),

    -- 修复前后发电曲线对比：确认补偿是否覆盖真实损失
    compare_status        TEXT NOT NULL DEFAULT ''
        CHECK (compare_status IN ('','pending','covered','insufficient')),
    pre_daily_kwh         NUMERIC(12,2) NOT NULL DEFAULT 0,    -- 修复前基准日均
    post_daily_kwh        NUMERIC(12,2) NOT NULL DEFAULT 0,    -- 修复后实测日均
    recovery_ratio        NUMERIC(7,2)  NOT NULL DEFAULT 0,    -- 出力恢复率 %
    cover_ratio           NUMERIC(7,2)  NOT NULL DEFAULT 0,    -- 补偿/真实损失覆盖率 %
    compare_note          TEXT NOT NULL DEFAULT '',
    compared_at           TIMESTAMPTZ,

    -- 补偿发放与同型号设备复查
    paid                  BOOLEAN NOT NULL DEFAULT FALSE,
    paid_at               TIMESTAMPTZ,
    review_needed         BOOLEAN NOT NULL DEFAULT FALSE,       -- 是否提醒运维复查同型号设备
    same_model_ids        BIGINT[] NOT NULL DEFAULT '{}',       -- 建议复查的同型号设备
    review_done           BOOLEAN NOT NULL DEFAULT FALSE,
    review_note           TEXT NOT NULL DEFAULT '',
    reviewed_at           TIMESTAMPTZ,

    created_by            BIGINT REFERENCES users(id),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_faults_building ON inverter_faults(building_id, period_month);
CREATE INDEX idx_faults_inverter ON inverter_faults(inverter_id);

-- 3) 受影响住户（及公共部分）收益损失与赔付明细
CREATE TABLE fault_compensation_items (
    id              BIGSERIAL PRIMARY KEY,
    fault_id        BIGINT NOT NULL REFERENCES inverter_faults(id) ON DELETE CASCADE,
    user_id         BIGINT REFERENCES users(id),   -- NULL = 公共部分（未认购组件）
    party           TEXT NOT NULL CHECK (party IN ('shareholder','public')),
    ratio           NUMERIC(7,4)  NOT NULL DEFAULT 0,  -- 绑定组件占整楼比例 %
    lost_kwh        NUMERIC(12,2) NOT NULL DEFAULT 0,
    supplier_amount NUMERIC(12,2) NOT NULL DEFAULT 0,  -- 供应商赔付（住户部分直接入户，公共部分入公共账）
    property_amount NUMERIC(12,2) NOT NULL DEFAULT 0   -- 物业延迟赔偿（先入公共账说明，再按比例代发住户）
);
CREATE INDEX idx_fault_items_fault ON fault_compensation_items(fault_id);
