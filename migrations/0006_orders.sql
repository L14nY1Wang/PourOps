-- 台位账单：orders 一台一单（未结账的唯一），order_items 是挂着的商品，
-- 结账时才落成 keg_pours / retail_sales，所以盈亏与库存口径不用改。
create table if not exists settings (
    key        text primary key,
    value      text not null
);

insert into settings (key, value) values ('bar_count', '3'), ('table_count', '4')
on conflict (key) do nothing;

create table if not exists orders (
    id        bigserial primary key,
    spot      text not null,              -- bar-1 / table-3
    opened_at timestamptz not null default now(),
    closed_at timestamptz
);

-- 一个台位同时只能有一张未结账单
create unique index if not exists orders_open_spot on orders (spot) where closed_at is null;

create table if not exists order_items (
    id          bigserial primary key,
    order_id    bigint not null references orders (id) on delete cascade,
    kind        text not null,            -- beer | retail
    ref_id      bigint not null,          -- kegs.id 或 retail_items.id
    name        text not null,            -- 下单时的名字快照
    unit_cents  int not null,
    ml          int not null default 0,
    qty         int not null default 1,
    added_at    timestamptz not null default now(),
    paid_at     timestamptz,
    payment_id  bigint
);

create index if not exists order_items_order on order_items (order_id, paid_at);

create table if not exists payments (
    id           bigserial primary key,
    order_id     bigint not null references orders (id) on delete cascade,
    amount_cents int not null,
    items_count  int not null,
    method       text not null default 'qr',
    created_at   timestamptz not null default now()
);
