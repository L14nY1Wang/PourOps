-- 删酒款时把它的酒桶和售出记录一起删掉（后台删除按钮已二次确认）。
alter table kegs drop constraint kegs_beer_id_fkey;
alter table kegs add constraint kegs_beer_id_fkey
  foreign key (beer_id) references beers(id) on delete cascade;
