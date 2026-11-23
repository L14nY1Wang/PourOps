-- 零售收银：订单 + 明细 + 库存流水
create table retail_sales (
  id           bigserial primary key,
  total_cents  int not null default 0,
  cost_cents   int not null default 0,
  created_at   timestamptz not null default now()
);

create table retail_sale_items (
  id          bigserial primary key,
  sale_id     bigint not null references retail_sales(id) on delete cascade,
  item_id     bigint,                      -- 不设外键：商品删了，历史订单还得留着
  name        text not null default '',
  qty         int not null default 1,
  price_cents int not null default 0,
  cost_cents  int not null default 0
);
create index retail_sale_items_sale_idx on retail_sale_items (sale_id);

create table retail_moves (
  id         bigserial primary key,
  item_id    bigint not null references retail_items(id) on delete cascade,
  delta      int not null,
  reason     text not null,                -- 入库 / 售出 / 调整 / 撤销
  ref_id     bigint,
  created_at timestamptz not null default now()
);
create index retail_moves_item_idx on retail_moves (item_id, created_at desc);
