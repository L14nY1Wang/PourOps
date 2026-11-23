-- 营运流水：进货、开桶上架/下架、手工售出、零售库存与上下架都记在一张台账里。
-- subject/detail/actor 都是当时的快照，原对象删了流水仍然读得懂。
create table if not exists operation_events (
  id          bigserial primary key,
  occurred_at timestamptz not null default now(),
  kind        text not null,
  subject     text not null,
  detail      text not null,
  actor       text not null default ''
);

create index if not exists operation_events_time_idx on operation_events (occurred_at desc, id desc);

-- 结账时记下操作者；旧支付没有这个字段，留空表示未记录，不回填。
alter table payments add column if not exists actor text not null default '';

create index if not exists payments_order_idx on payments (order_id, id);
