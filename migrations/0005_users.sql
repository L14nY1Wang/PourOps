-- 账号与角色：boss（最高）> manager（店长）> staff（店员，只能进点餐端）
create table users (
  id            bigserial primary key,
  username      text not null,
  password_hash text not null,
  role          text not null check (role in ('boss', 'manager', 'staff')),
  active        boolean not null default true,
  created_at    timestamptz not null default now()
);

create unique index users_username_key on users (lower(username));
