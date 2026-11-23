-- 一杯酒按杯型分别定价：150 / 330 / 500ml，空着就是不提供这个杯型。
alter table beers add column if not exists price_150 int;
alter table beers add column if not exists price_330 int;
alter table beers add column if not exists price_500 int;

-- 原来的单一价格按 size 里的毫升数落到对应杯型
update beers set
	price_150 = case when size ~ '150' then price_cents end,
	price_330 = case when size ~ '330' then price_cents end,
	price_500 = case when size ~ '500' then price_cents end
where coalesce(price_cents, 0) > 0 and coalesce(size, '') ~ '(150|330|500)';

-- size 里没写毫升的，统一当成 330ml 一杯
update beers set price_330 = price_cents
where coalesce(price_cents, 0) > 0
	and coalesce(price_150, 0) + coalesce(price_330, 0) + coalesce(price_500, 0) = 0;
