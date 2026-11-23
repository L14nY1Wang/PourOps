-- 桶装精酿（按桶管理）与零售商品
create table kegs (
  id          bigserial primary key,
  beer_id     bigint not null references beers(id) on delete restrict,
  keg_no      text not null default '',
  capacity_ml int not null default 20000,
  cost_cents  int not null default 0,          -- 本桶成本（进价）
  placed_at   timestamptz,                     -- 上架时间
  tapped_at   timestamptz,                     -- 开桶时间
  finished_at timestamptz,                     -- 结桶时间（空桶）
  note        text not null default '',
  created_at  timestamptz not null default now()
);

-- 每记一笔 = 一次售出；金额与毫升都由这里加总，不另存冗余字段。
create table keg_pours (
  id           bigserial primary key,
  keg_id       bigint not null references kegs(id) on delete cascade,
  cups         int not null default 1,
  ml           int not null default 0,
  amount_cents int not null default 0,
  poured_at    timestamptz not null default now()
);

create index kegs_active_idx on kegs (finished_at, placed_at);
create index keg_pours_keg_idx on keg_pours (keg_id, poured_at);

create table retail_items (
  id          bigserial primary key,
  name        text not null,
  category    text not null default '罐装',
  spec        text not null default '',        -- 330ml / 40g 之类
  price_cents int not null default 0,
  cost_cents  int not null default 0,
  stock       int not null default 0,
  active      bool not null default true
);
