-- 酒款。价格用「分」存，避免浮点；口味/售卖标签是文本数组，后台按逗号分隔编辑。
create table if not exists beers (
  id          bigserial primary key,
  no          text not null default '',           -- 酒单编号，空则由 sort_order 决定顺序
  brewery     text not null default '',
  name        text not null,
  style       text not null default '',
  category    text not null default '',           -- fresh/hop/sour/dark/strong，空则由 style 推断
  abv         double precision,
  ibu         integer,
  size        text not null default '',
  price_cents integer not null default 0,
  ut_rating   double precision,
  ut_count    integer,
  tags        text[] not null default '{}',
  sales_tags  text[] not null default '{}',
  poster_url  text not null default '',
  on_screen   boolean not null default false,     -- 是否出现在电视酒单
  sort_order  integer not null default 0,
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now()
);

create index if not exists beers_screen_idx on beers (on_screen, sort_order, id);
