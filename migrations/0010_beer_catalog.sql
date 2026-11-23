-- 本店历史酒单库：独立沉淀每款进过货或录入过的酒款资料，支持自动搜索填充与导入导出，
-- 避免把尚未进货的历史参考酒款混进在售酒单（beers），也不会因删掉在售酒款而丢失资料。
create table if not exists beer_catalog (
  id          bigserial primary key,
  brewery     text not null default '',
  name        text not null,
  style       text not null default '',
  category    text not null default '',
  abv         double precision,
  ibu         integer,
  ut_rating   double precision,
  ut_count    integer,
  tags        text[] not null default '{}',
  poster_url  text not null default '',
  capacity_ml int not null default 20000,
  cost_cents  int not null default 0,
  price_150   int not null default 0,
  price_330   int not null default 0,
  price_500   int not null default 0,
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now()
);

create unique index if not exists beer_catalog_key_uidx
  on beer_catalog (lower(btrim(brewery)), lower(btrim(name)));

insert into beer_catalog (
  brewery, name, style, category, abv, ibu, ut_rating, ut_count,
  tags, poster_url, capacity_ml, cost_cents, price_150, price_330, price_500,
  created_at, updated_at
)
select distinct on (lower(btrim(b.brewery)), lower(btrim(b.name)))
  btrim(b.brewery),
  btrim(b.name),
  coalesce(b.style, ''),
  coalesce(b.category, ''),
  b.abv,
  b.ibu,
  b.ut_rating,
  b.ut_count,
  coalesce(b.tags, '{}'),
  coalesce(b.poster_url, ''),
  coalesce(nullif(k.capacity_ml, 0), 20000),
  coalesce(k.cost_cents, 0),
  coalesce(b.price_150, 0),
  coalesce(b.price_330, 0),
  coalesce(b.price_500, 0),
  b.created_at,
  b.updated_at
from beers b
left join lateral (
  select capacity_ml, cost_cents
  from kegs
  where beer_id = b.id
  order by id desc
  limit 1
) k on true
where btrim(b.name) <> ''
order by lower(btrim(b.brewery)), lower(btrim(b.name)), b.updated_at desc, b.id desc
on conflict (lower(btrim(brewery)), lower(btrim(name))) do nothing;
