-- 0014: 会员体系（档案/等级/储值/积分）、31天月卡与预设/自定义折扣

-- 1. 会员等级
CREATE TABLE IF NOT EXISTS member_tiers (
    id              bigserial PRIMARY KEY,
    name            text NOT NULL,
    sort_order      integer NOT NULL DEFAULT 0,
    min_spent_cents integer NOT NULL DEFAULT 0 CHECK (min_spent_cents >= 0),
    discount_bps    integer NOT NULL DEFAULT 10000 CHECK (discount_bps BETWEEN 100 AND 10000),
    apply_scope     text NOT NULL DEFAULT 'all' CHECK (apply_scope IN ('all', 'beer', 'retail')),
    points_rate_bps integer NOT NULL DEFAULT 10000 CHECK (points_rate_bps BETWEEN 0 AND 100000),
    active          boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS member_tiers_name_uidx
    ON member_tiers (lower(btrim(name)));

-- 2. 会员档案
CREATE TABLE IF NOT EXISTS members (
    id                      bigserial PRIMARY KEY,
    member_no               text NOT NULL DEFAULT '',
    name                    text NOT NULL,
    phone                   text NOT NULL,
    tier_id                 bigint REFERENCES member_tiers(id) ON DELETE SET NULL,
    balance_cents           integer NOT NULL DEFAULT 0 CHECK (balance_cents >= 0),
    principal_balance_cents integer NOT NULL DEFAULT 0 CHECK (principal_balance_cents >= 0),
    points                  integer NOT NULL DEFAULT 0 CHECK (points >= 0),
    lifetime_spent_cents    bigint NOT NULL DEFAULT 0 CHECK (lifetime_spent_cents >= 0),
    lifetime_orders         integer NOT NULL DEFAULT 0 CHECK (lifetime_orders >= 0),
    birthday                text NOT NULL DEFAULT '',
    note                    text NOT NULL DEFAULT '',
    active                  boolean NOT NULL DEFAULT true,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS members_phone_uidx
    ON members (btrim(phone));

CREATE UNIQUE INDEX IF NOT EXISTS members_no_uidx
    ON members (btrim(member_no))
    WHERE btrim(member_no) <> '';

-- 3. 储值流水（不可变台账）
CREATE TABLE IF NOT EXISTS member_balance_txns (
    id                    bigserial PRIMARY KEY,
    member_id             bigint NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    kind                  text NOT NULL CHECK (kind IN ('recharge', 'recharge_bonus', 'consume', 'adjust', 'refund', 'pass_buy')),
    delta_cents           integer NOT NULL,
    principal_delta_cents integer NOT NULL DEFAULT 0,
    balance_after_cents   integer NOT NULL CHECK (balance_after_cents >= 0),
    pay_method            text NOT NULL DEFAULT '',
    payment_id            bigint REFERENCES payments(id) ON DELETE SET NULL,
    actor                 text NOT NULL DEFAULT '',
    note                  text NOT NULL DEFAULT '',
    idempotency_key       text,
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS member_balance_txns_idem_uidx
    ON member_balance_txns (member_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';

CREATE INDEX IF NOT EXISTS member_balance_txns_member_idx
    ON member_balance_txns (member_id, id DESC);

CREATE INDEX IF NOT EXISTS member_balance_txns_created_idx
    ON member_balance_txns (created_at DESC);

-- 4. 积分流水（不可变台账）
CREATE TABLE IF NOT EXISTS member_point_txns (
    id           bigserial PRIMARY KEY,
    member_id    bigint NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    kind         text NOT NULL CHECK (kind IN ('earn', 'redeem', 'adjust')),
    delta_points integer NOT NULL,
    points_after integer NOT NULL CHECK (points_after >= 0),
    payment_id   bigint REFERENCES payments(id) ON DELETE SET NULL,
    actor        text NOT NULL DEFAULT '',
    note         text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS member_point_txns_member_idx
    ON member_point_txns (member_id, id DESC);

-- 5. 月卡方案模板
CREATE TABLE IF NOT EXISTS pass_plans (
    id             bigserial PRIMARY KEY,
    name           text NOT NULL,
    price_cents    integer NOT NULL DEFAULT 0 CHECK (price_cents >= 0),
    duration_days  integer NOT NULL DEFAULT 31 CHECK (duration_days BETWEEN 1 AND 3650),
    scope          text NOT NULL DEFAULT 'beer' CHECK (scope IN ('beer', 'all')),
    benefit_type   text NOT NULL DEFAULT 'free_quota' CHECK (benefit_type IN ('free_quota', 'discount', 'hybrid')),
    daily_limit    integer NOT NULL DEFAULT 1 CHECK (daily_limit BETWEEN 0 AND 99),
    total_limit    integer NOT NULL DEFAULT 0 CHECK (total_limit BETWEEN 0 AND 9999),
    max_ml         integer NOT NULL DEFAULT 0 CHECK (max_ml >= 0),
    max_unit_cents integer NOT NULL DEFAULT 0 CHECK (max_unit_cents >= 0),
    cap_mode       text NOT NULL DEFAULT 'pay_diff' CHECK (cap_mode IN ('pay_diff', 'block')),
    discount_bps   integer NOT NULL DEFAULT 10000 CHECK (discount_bps BETWEEN 100 AND 10000),
    active         boolean NOT NULL DEFAULT true,
    sort_order     integer NOT NULL DEFAULT 0,
    note           text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS pass_plans_name_uidx
    ON pass_plans (lower(btrim(name)));

-- 6. 会员持卡实例（发卡时快照规则，防止模板修改影响已有有效卡）
CREATE TABLE IF NOT EXISTS member_passes (
    id             bigserial PRIMARY KEY,
    member_id      bigint NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    plan_id        bigint REFERENCES pass_plans(id) ON DELETE SET NULL,
    plan_name      text NOT NULL,
    scope          text NOT NULL CHECK (scope IN ('beer', 'all')),
    benefit_type   text NOT NULL CHECK (benefit_type IN ('free_quota', 'discount', 'hybrid')),
    daily_limit    integer NOT NULL CHECK (daily_limit BETWEEN 0 AND 99),
    total_limit    integer NOT NULL CHECK (total_limit BETWEEN 0 AND 9999),
    max_ml         integer NOT NULL DEFAULT 0 CHECK (max_ml >= 0),
    max_unit_cents integer NOT NULL DEFAULT 0 CHECK (max_unit_cents >= 0),
    cap_mode       text NOT NULL DEFAULT 'pay_diff' CHECK (cap_mode IN ('pay_diff', 'block')),
    discount_bps   integer NOT NULL DEFAULT 10000 CHECK (discount_bps BETWEEN 100 AND 10000),
    price_cents    integer NOT NULL DEFAULT 0 CHECK (price_cents >= 0),
    pay_method     text NOT NULL DEFAULT 'qr',
    starts_at      timestamptz NOT NULL,
    expires_at     timestamptz NOT NULL,
    used_total     integer NOT NULL DEFAULT 0 CHECK (used_total >= 0),
    status         text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    actor          text NOT NULL DEFAULT '',
    note           text NOT NULL DEFAULT '',
    idempotency_key text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT member_passes_time_valid CHECK (expires_at > starts_at)
);

CREATE UNIQUE INDEX IF NOT EXISTS member_passes_idem_uidx
    ON member_passes (member_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';

CREATE INDEX IF NOT EXISTS member_passes_member_idx
    ON member_passes (member_id, status, expires_at DESC);

CREATE INDEX IF NOT EXISTS member_passes_created_idx
    ON member_passes (created_at DESC);

-- 7. 月卡核销流水（按酒吧营业日 business_day 计次）
CREATE TABLE IF NOT EXISTS member_pass_usages (
    id             bigserial PRIMARY KEY,
    member_pass_id bigint NOT NULL REFERENCES member_passes(id) ON DELETE RESTRICT,
    member_id      bigint NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    order_id       bigint NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    payment_id     bigint NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
    business_day   text NOT NULL,
    cups           integer NOT NULL CHECK (cups > 0),
    saved_cents    integer NOT NULL DEFAULT 0 CHECK (saved_cents >= 0),
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS member_pass_usages_day_idx
    ON member_pass_usages (member_pass_id, business_day);

CREATE INDEX IF NOT EXISTS member_pass_usages_created_idx
    ON member_pass_usages (created_at DESC);

-- 8. 预设折扣规则
CREATE TABLE IF NOT EXISTS discount_rules (
    id                bigserial PRIMARY KEY,
    name              text NOT NULL,
    kind              text NOT NULL CHECK (kind IN ('percent', 'fixed', 'round_down', 'comp')),
    discount_bps      integer NOT NULL DEFAULT 10000 CHECK (discount_bps BETWEEN 0 AND 10000),
    fixed_cents       integer NOT NULL DEFAULT 0 CHECK (fixed_cents >= 0),
    round_unit_cents  integer NOT NULL DEFAULT 100 CHECK (round_unit_cents > 0),
    scope             text NOT NULL DEFAULT 'all' CHECK (scope IN ('all', 'beer', 'retail')),
    min_amount_cents  integer NOT NULL DEFAULT 0 CHECK (min_amount_cents >= 0),
    allow_staff       boolean NOT NULL DEFAULT false,
    stack_with_member boolean NOT NULL DEFAULT false,
    active            boolean NOT NULL DEFAULT true,
    sort_order        integer NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS discount_rules_name_uidx
    ON discount_rules (lower(btrim(name)));

-- 9. 扩展 orders / order_items / payments / retail_sales / retail_sale_items
ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS member_id bigint REFERENCES members(id) ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS orders_member_idx
    ON orders (member_id)
    WHERE member_id IS NOT NULL;

ALTER TABLE order_items
    ADD COLUMN IF NOT EXISTS discount_cents integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS paid_cents integer,
    ADD COLUMN IF NOT EXISTS pass_redeemed_qty integer NOT NULL DEFAULT 0;

ALTER TABLE payments
    ADD COLUMN IF NOT EXISTS member_id bigint REFERENCES members(id) ON DELETE RESTRICT,
    ADD COLUMN IF NOT EXISTS gross_cents integer,
    ADD COLUMN IF NOT EXISTS discount_cents integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS pass_discount_cents integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS member_discount_cents integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS manual_discount_cents integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS points_discount_cents integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS balance_paid_cents integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS external_paid_cents integer,
    ADD COLUMN IF NOT EXISTS discount_rule_id bigint REFERENCES discount_rules(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS discount_label text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS discount_note text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS member_pass_id bigint REFERENCES member_passes(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS points_spent integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS points_earned integer NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS payments_member_idx
    ON payments (member_id)
    WHERE member_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS payments_pass_idx
    ON payments (member_pass_id)
    WHERE member_pass_id IS NOT NULL;

ALTER TABLE retail_sales
    ADD COLUMN IF NOT EXISTS gross_cents integer,
    ADD COLUMN IF NOT EXISTS discount_cents integer NOT NULL DEFAULT 0;

ALTER TABLE retail_sale_items
    ADD COLUMN IF NOT EXISTS net_cents integer;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'order_items_loyalty_nonneg' AND conrelid = 'order_items'::regclass) THEN
        ALTER TABLE order_items ADD CONSTRAINT order_items_loyalty_nonneg CHECK (
            discount_cents >= 0
            AND (paid_cents IS NULL OR paid_cents >= 0)
            AND pass_redeemed_qty >= 0
        ) NOT VALID;
        ALTER TABLE order_items VALIDATE CONSTRAINT order_items_loyalty_nonneg;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'payments_loyalty_nonneg' AND conrelid = 'payments'::regclass) THEN
        ALTER TABLE payments ADD CONSTRAINT payments_loyalty_nonneg CHECK (
            (gross_cents IS NULL OR gross_cents >= 0)
            AND discount_cents >= 0
            AND pass_discount_cents >= 0
            AND member_discount_cents >= 0
            AND manual_discount_cents >= 0
            AND points_discount_cents >= 0
            AND balance_paid_cents >= 0
            AND (external_paid_cents IS NULL OR external_paid_cents >= 0)
            AND points_spent >= 0
            AND points_earned >= 0
        ) NOT VALID;
        ALTER TABLE payments VALIDATE CONSTRAINT payments_loyalty_nonneg;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'retail_sales_loyalty_nonneg' AND conrelid = 'retail_sales'::regclass) THEN
        ALTER TABLE retail_sales ADD CONSTRAINT retail_sales_loyalty_nonneg CHECK (
            (gross_cents IS NULL OR gross_cents >= 0)
            AND discount_cents >= 0
        ) NOT VALID;
        ALTER TABLE retail_sales VALIDATE CONSTRAINT retail_sales_loyalty_nonneg;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'retail_sale_items_net_nonneg' AND conrelid = 'retail_sale_items'::regclass) THEN
        ALTER TABLE retail_sale_items ADD CONSTRAINT retail_sale_items_net_nonneg CHECK (
            net_cents IS NULL OR net_cents >= 0
        ) NOT VALID;
        ALTER TABLE retail_sale_items VALIDATE CONSTRAINT retail_sale_items_net_nonneg;
    END IF;
END $$;

-- 10. 默认初始化数据（幂等）
INSERT INTO member_tiers (name, sort_order, min_spent_cents, discount_bps, apply_scope, points_rate_bps, active)
VALUES
    ('普通会员', 10, 0, 10000, 'all', 10000, true),
    ('常客会员', 20, 100000, 9500, 'all', 12000, true),
    ('黑金会员', 30, 300000, 9000, 'all', 15000, true)
ON CONFLICT (lower(btrim(name))) DO NOTHING;

INSERT INTO pass_plans (name, price_cents, duration_days, scope, benefit_type, daily_limit, total_limit, max_ml, max_unit_cents, cap_mode, discount_bps, active, sort_order, note)
VALUES
    ('31天畅饮月卡（每日1杯）', 29900, 31, 'beer', 'free_quota', 1, 0, 0, 0, 'pay_diff', 10000, true, 10, '31天内每天免费喝1杯桶装精酿（按营业日 07:00-次日 07:00 计次）')
ON CONFLICT (lower(btrim(name))) DO NOTHING;

INSERT INTO discount_rules (name, kind, discount_bps, fixed_cents, round_unit_cents, scope, min_amount_cents, allow_staff, stack_with_member, active, sort_order)
VALUES
    ('95 折', 'percent', 9500, 0, 100, 'all', 0, true, false, true, 10),
    ('9 折', 'percent', 9000, 0, 100, 'all', 0, true, false, true, 20),
    ('85 折', 'percent', 8500, 0, 100, 'all', 0, false, false, true, 30),
    ('抹零到元', 'round_down', 10000, 0, 100, 'all', 0, true, true, true, 40),
    ('老板请客免单', 'comp', 0, 0, 100, 'all', 0, false, false, true, 50)
ON CONFLICT (lower(btrim(name))) DO NOTHING;

INSERT INTO store_settings (key, value)
VALUES
    ('loyalty_points_per_yuan', '1'),
    ('loyalty_points_redeem_rate', '100'),
    ('loyalty_points_max_pct', '50'),
    ('loyalty_recharge_presets', '[{"pay_cents":30000,"bonus_cents":2000},{"pay_cents":50000,"bonus_cents":5000},{"pay_cents":100000,"bonus_cents":15000}]')
ON CONFLICT (key) DO NOTHING;
