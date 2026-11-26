-- 0013: 数据完整性约束、外键保护、软删除与高频查询索引（P1-3..P1-5, P1-8, P2-7, P2-14, P2-19, P2-20, P2-21, P2-26）

-- 1. 对齐历史分类文本空白与旧迁移口径，合并遗留 settings 表到 store_settings
UPDATE beers SET category = btrim(category) WHERE category <> btrim(category);
UPDATE beer_catalog SET category = btrim(category) WHERE category <> btrim(category);
UPDATE retail_items SET category = btrim(category) WHERE category <> btrim(category);
UPDATE retail_items SET category = '罐装' WHERE btrim(category) = '';

-- 将 0012 误落入 酸小麦 的非小麦酸艾尔（兰比克/贵兹/野菌等）对齐为 Go 运行时 酸艾尔 口径
UPDATE beers
SET category = '酸艾尔'
WHERE btrim(category) = '酸小麦'
  AND lower(style) ~ '(lambic|gueuze|geuze|sour|wild|flanders|oude|brett|酸艾尔|兰比克|贵兹|野菌|法兰德斯|酸啤|酸)'
  AND lower(style) !~ '(berliner|gose|酸小麦|柏林|古斯)'
  AND NOT EXISTS (
      SELECT 1 FROM store_settings ss
      WHERE ss.key = 'beer_categories' AND ss.value NOT LIKE '%"酸艾尔"%'
  );

UPDATE beer_catalog
SET category = '酸艾尔'
WHERE btrim(category) = '酸小麦'
  AND lower(style) ~ '(lambic|gueuze|geuze|sour|wild|flanders|oude|brett|酸艾尔|兰比克|贵兹|野菌|法兰德斯|酸啤|酸)'
  AND lower(style) !~ '(berliner|gose|酸小麦|柏林|古斯)'
  AND NOT EXISTS (
      SELECT 1 FROM store_settings ss
      WHERE ss.key = 'beer_categories' AND ss.value NOT LIKE '%"酸艾尔"%'
  );

-- 若门店分类配置未主动启用 波特 / 修道院，且预设分类 世涛 存在，则统一收敛到 世涛
UPDATE beers
SET category = '世涛'
WHERE btrim(category) IN ('波特', '修道院')
  AND NOT EXISTS (
      SELECT 1 FROM store_settings ss
      WHERE ss.key = 'beer_categories' AND ss.value LIKE '%"' || btrim(beers.category) || '"%'
  )
  AND NOT EXISTS (
      SELECT 1 FROM store_settings ss
      WHERE ss.key = 'beer_categories' AND ss.value NOT LIKE '%"世涛"%'
  );

UPDATE beer_catalog
SET category = '世涛'
WHERE btrim(category) IN ('波特', '修道院')
  AND NOT EXISTS (
      SELECT 1 FROM store_settings ss
      WHERE ss.key = 'beer_categories' AND ss.value LIKE '%"' || btrim(beer_catalog.category) || '"%'
  )
  AND NOT EXISTS (
      SELECT 1 FROM store_settings ss
      WHERE ss.key = 'beer_categories' AND ss.value NOT LIKE '%"世涛"%'
  );

DO $$
BEGIN
    IF to_regclass('settings') IS NOT NULL THEN
        INSERT INTO store_settings (key, value)
        SELECT key, value FROM settings
        ON CONFLICT (key) DO NOTHING;
        DROP TABLE settings;
    END IF;
END $$;

-- 2. 新增审计、成本快照、幂等支付、软删除与 2FA 渐进锁定列
ALTER TABLE keg_pours ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT 'manual';
ALTER TABLE keg_pours ADD COLUMN IF NOT EXISTS payment_id bigint;
ALTER TABLE keg_pours ADD COLUMN IF NOT EXISTS cost_cents integer;

UPDATE keg_pours kp
SET cost_cents = ROUND((k.cost_cents::numeric * kp.ml::numeric) / k.capacity_ml::numeric)::integer
FROM kegs k
WHERE kp.keg_id = k.id
  AND kp.cost_cents IS NULL
  AND k.cost_cents > 0
  AND k.capacity_ml > 0;

ALTER TABLE payments ADD COLUMN IF NOT EXISTS provider text NOT NULL DEFAULT '';
ALTER TABLE payments ADD COLUMN IF NOT EXISTS reference text NOT NULL DEFAULT '';
ALTER TABLE payments ADD COLUMN IF NOT EXISTS idempotency_key text;

ALTER TABLE retail_items ADD COLUMN IF NOT EXISTS archived_at timestamptz;

ALTER TABLE users ADD COLUMN IF NOT EXISTS otp_lock_level integer NOT NULL DEFAULT 0;

-- 清理历史孤儿 payment_id（若有）后再建立外键
UPDATE order_items oi
SET payment_id = NULL
WHERE oi.payment_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.id = oi.payment_id);

UPDATE keg_pours kp
SET payment_id = NULL
WHERE kp.payment_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.id = kp.payment_id);

-- 3. 加固外键策略：禁止级联删除业务与资金台账
ALTER TABLE kegs DROP CONSTRAINT IF EXISTS kegs_beer_id_fkey;
ALTER TABLE kegs ADD CONSTRAINT kegs_beer_id_fkey
    FOREIGN KEY (beer_id) REFERENCES beers(id) ON DELETE RESTRICT NOT VALID;
ALTER TABLE kegs VALIDATE CONSTRAINT kegs_beer_id_fkey;

ALTER TABLE keg_pours DROP CONSTRAINT IF EXISTS keg_pours_keg_id_fkey;
ALTER TABLE keg_pours ADD CONSTRAINT keg_pours_keg_id_fkey
    FOREIGN KEY (keg_id) REFERENCES kegs(id) ON DELETE RESTRICT NOT VALID;
ALTER TABLE keg_pours VALIDATE CONSTRAINT keg_pours_keg_id_fkey;

ALTER TABLE keg_pours DROP CONSTRAINT IF EXISTS keg_pours_payment_id_fkey;
ALTER TABLE keg_pours ADD CONSTRAINT keg_pours_payment_id_fkey
    FOREIGN KEY (payment_id) REFERENCES payments(id) ON DELETE SET NULL NOT VALID;
ALTER TABLE keg_pours VALIDATE CONSTRAINT keg_pours_payment_id_fkey;

ALTER TABLE order_items DROP CONSTRAINT IF EXISTS order_items_order_id_fkey;
ALTER TABLE order_items ADD CONSTRAINT order_items_order_id_fkey
    FOREIGN KEY (order_id) REFERENCES orders(id) ON DELETE RESTRICT NOT VALID;
ALTER TABLE order_items VALIDATE CONSTRAINT order_items_order_id_fkey;

ALTER TABLE order_items DROP CONSTRAINT IF EXISTS order_items_payment_id_fkey;
ALTER TABLE order_items ADD CONSTRAINT order_items_payment_id_fkey
    FOREIGN KEY (payment_id) REFERENCES payments(id) ON DELETE SET NULL NOT VALID;
ALTER TABLE order_items VALIDATE CONSTRAINT order_items_payment_id_fkey;

ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_order_id_fkey;
ALTER TABLE payments ADD CONSTRAINT payments_order_id_fkey
    FOREIGN KEY (order_id) REFERENCES orders(id) ON DELETE RESTRICT NOT VALID;
ALTER TABLE payments VALIDATE CONSTRAINT payments_order_id_fkey;

ALTER TABLE retail_moves DROP CONSTRAINT IF EXISTS retail_moves_item_id_fkey;
ALTER TABLE retail_moves ADD CONSTRAINT retail_moves_item_id_fkey
    FOREIGN KEY (item_id) REFERENCES retail_items(id) ON DELETE RESTRICT NOT VALID;
ALTER TABLE retail_moves VALIDATE CONSTRAINT retail_moves_item_id_fkey;

-- 4. 唯一索引与防并发重复控制
CREATE UNIQUE INDEX IF NOT EXISTS kegs_one_active_per_beer_uidx
    ON kegs (beer_id)
    WHERE tapped_at IS NOT NULL AND finished_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS payments_idempotency_uidx
    ON payments (order_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';

-- 5. 值域 CHECK 约束（NOT VALID + VALIDATE 避免长时间持有阻塞锁）
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'beers_prices_nonneg' AND conrelid = 'beers'::regclass) THEN
        ALTER TABLE beers ADD CONSTRAINT beers_prices_nonneg CHECK (
            price_cents >= 0
            AND (price_150 IS NULL OR price_150 >= 0)
            AND (price_330 IS NULL OR price_330 >= 0)
            AND (price_500 IS NULL OR price_500 >= 0)
        ) NOT VALID;
        ALTER TABLE beers VALIDATE CONSTRAINT beers_prices_nonneg;
    END IF;

    UPDATE beers SET abv = NULL WHERE abv IS NOT NULL AND (abv < 0 OR abv > 100);
    UPDATE beers SET ibu = NULL WHERE ibu IS NOT NULL AND (ibu < 0 OR ibu > 1000);

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'beers_abv_ibu_range' AND conrelid = 'beers'::regclass) THEN
        ALTER TABLE beers ADD CONSTRAINT beers_abv_ibu_range CHECK (
            (abv IS NULL OR (abv >= 0 AND abv <= 100))
            AND (ibu IS NULL OR (ibu >= 0 AND ibu <= 1000))
        ) NOT VALID;
        ALTER TABLE beers VALIDATE CONSTRAINT beers_abv_ibu_range;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'beer_catalog_nonneg' AND conrelid = 'beer_catalog'::regclass) THEN
        ALTER TABLE beer_catalog ADD CONSTRAINT beer_catalog_nonneg CHECK (
            capacity_ml > 0 AND cost_cents >= 0 AND price_150 >= 0 AND price_330 >= 0 AND price_500 >= 0
        ) NOT VALID;
        ALTER TABLE beer_catalog VALIDATE CONSTRAINT beer_catalog_nonneg;
    END IF;

    UPDATE kegs
    SET tapped_at = COALESCE(placed_at, finished_at)
    WHERE finished_at IS NOT NULL AND tapped_at IS NULL;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'kegs_valid_bounds' AND conrelid = 'kegs'::regclass) THEN
        ALTER TABLE kegs ADD CONSTRAINT kegs_valid_bounds CHECK (
            capacity_ml > 0 AND cost_cents >= 0
            AND (finished_at IS NULL OR tapped_at IS NOT NULL)
        ) NOT VALID;
        ALTER TABLE kegs VALIDATE CONSTRAINT kegs_valid_bounds;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'keg_pours_nonneg' AND conrelid = 'keg_pours'::regclass) THEN
        ALTER TABLE keg_pours ADD CONSTRAINT keg_pours_nonneg CHECK (
            cups >= 0 AND ml >= 0 AND amount_cents >= 0 AND (cost_cents IS NULL OR cost_cents >= 0)
        ) NOT VALID;
        ALTER TABLE keg_pours VALIDATE CONSTRAINT keg_pours_nonneg;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'keg_pours_source_check' AND conrelid = 'keg_pours'::regclass) THEN
        ALTER TABLE keg_pours ADD CONSTRAINT keg_pours_source_check CHECK (
            source IN ('manual', 'pos')
        ) NOT VALID;
        ALTER TABLE keg_pours VALIDATE CONSTRAINT keg_pours_source_check;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'retail_items_nonneg' AND conrelid = 'retail_items'::regclass) THEN
        ALTER TABLE retail_items ADD CONSTRAINT retail_items_nonneg CHECK (
            price_cents >= 0 AND cost_cents >= 0 AND stock >= 0
        ) NOT VALID;
        ALTER TABLE retail_items VALIDATE CONSTRAINT retail_items_nonneg;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'retail_sales_nonneg' AND conrelid = 'retail_sales'::regclass) THEN
        ALTER TABLE retail_sales ADD CONSTRAINT retail_sales_nonneg CHECK (
            total_cents >= 0 AND cost_cents >= 0
        ) NOT VALID;
        ALTER TABLE retail_sales VALIDATE CONSTRAINT retail_sales_nonneg;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'retail_sale_items_valid' AND conrelid = 'retail_sale_items'::regclass) THEN
        ALTER TABLE retail_sale_items ADD CONSTRAINT retail_sale_items_valid CHECK (
            qty > 0 AND price_cents >= 0 AND cost_cents >= 0
        ) NOT VALID;
        ALTER TABLE retail_sale_items VALIDATE CONSTRAINT retail_sale_items_valid;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'orders_spot_nonempty' AND conrelid = 'orders'::regclass) THEN
        ALTER TABLE orders ADD CONSTRAINT orders_spot_nonempty CHECK (
            btrim(spot) <> ''
        ) NOT VALID;
        ALTER TABLE orders VALIDATE CONSTRAINT orders_spot_nonempty;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'order_items_valid' AND conrelid = 'order_items'::regclass) THEN
        ALTER TABLE order_items ADD CONSTRAINT order_items_valid CHECK (
            kind IN ('beer', 'retail') AND unit_cents >= 0 AND ml >= 0 AND qty > 0
        ) NOT VALID;
        ALTER TABLE order_items VALIDATE CONSTRAINT order_items_valid;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'payments_valid' AND conrelid = 'payments'::regclass) THEN
        ALTER TABLE payments ADD CONSTRAINT payments_valid CHECK (
            amount_cents >= 0 AND items_count > 0
        ) NOT VALID;
        ALTER TABLE payments VALIDATE CONSTRAINT payments_valid;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'users_otp_nonneg' AND conrelid = 'users'::regclass) THEN
        ALTER TABLE users ADD CONSTRAINT users_otp_nonneg CHECK (
            otp_failures >= 0 AND otp_lock_level >= 0 AND auth_version >= 0
        ) NOT VALID;
        ALTER TABLE users VALIDATE CONSTRAINT users_otp_nonneg;
    END IF;
END $$;

-- 6. 高频经营报表、POS 与库存查询索引
CREATE INDEX IF NOT EXISTS keg_pours_poured_at_idx ON keg_pours (poured_at DESC);
CREATE INDEX IF NOT EXISTS keg_pours_payment_idx ON keg_pours (payment_id) WHERE payment_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS order_items_paid_at_idx ON order_items (paid_at) WHERE paid_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS order_items_payment_idx ON order_items (payment_id) WHERE payment_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS orders_closed_at_idx ON orders (closed_at DESC) WHERE closed_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS payments_created_at_idx ON payments (created_at DESC);
CREATE INDEX IF NOT EXISTS retail_sales_created_at_idx ON retail_sales (created_at DESC);
CREATE INDEX IF NOT EXISTS kegs_beer_idx ON kegs (beer_id, finished_at, placed_at);
CREATE INDEX IF NOT EXISTS operation_events_kind_time_idx ON operation_events (kind, occurred_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS beers_poster_url_idx ON beers (poster_url) WHERE poster_url <> '';
CREATE INDEX IF NOT EXISTS beer_catalog_poster_url_idx ON beer_catalog (poster_url) WHERE poster_url <> '';
