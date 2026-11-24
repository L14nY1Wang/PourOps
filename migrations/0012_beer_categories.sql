-- 将早期五色分类（fresh/hop/sour/dark/strong 及对应旧中文名）结合 style 精准迁移到新版精酿风格分类，
-- 仅处理仍为旧分类值的记录，保证幂等且不影响已迁移或自定义的分类。

update beers
set category = case
    when lower(style) ~ '(cider|perry|pomme|苹果酒|西打)' then '西打'
    when lower(style) ~ '(smoothie|puree|slush|fruited|pastry sour|果泥|奶昔)' then '果泥'
    when lower(style) ~ '(berliner|gose|lambic|gueuze|geuze|sour|wild|flanders|oude|brett|酸小麦|柏林|古斯|贵兹|兰比克|野菌|酸)' then '酸小麦'
    when lower(style) ~ '(\yipa\y|neipa|dipa|tipa|india pale|pale ale|\yapa\y|hazy|印度淡|浑浊|酒花)' then 'IPA'
    when lower(style) ~ '(stout|irish dry|imperial black|世涛|黑啤)' then '世涛'
    when lower(style) ~ '(porter|baltic|波特)' then '波特'
    when lower(style) ~ '(pilsner|pilsener|\ypils\y|bohemian|皮尔森|比尔森)' then '皮尔森'
    when lower(style) ~ '(wheat|witbier|hefeweizen|weizen|weisse|weissbier|blanche|white ale|小麦|白啤)' then '小麦'
    when lower(style) ~ '(trappist|abbey|tripel|dubbel|quadrupel|belgian strong|\yquad\y|修道院|三料|双料|四料|烈性)' then '修道院'
    when lower(style) ~ '(lager|helles|dunkel|marzen|märzen|bock|kolsch|kölsch|vienna|schwarz|拉格|海勒|博克|科隆|清爽)' then '拉格'
    when lower(btrim(category)) in ('hop', '酒花') then 'IPA'
    when lower(btrim(category)) in ('sour', '酸野菌') then '酸小麦'
    when lower(btrim(category)) in ('dark', '深色') then '世涛'
    when lower(btrim(category)) in ('strong', '烈性') then '修道院'
    else '拉格'
end
where lower(btrim(category)) in ('fresh', 'hop', 'sour', 'dark', 'strong', '清爽', '酒花', '酸野菌', '深色', '烈性');

update beer_catalog
set category = case
    when lower(style) ~ '(cider|perry|pomme|苹果酒|西打)' then '西打'
    when lower(style) ~ '(smoothie|puree|slush|fruited|pastry sour|果泥|奶昔)' then '果泥'
    when lower(style) ~ '(berliner|gose|lambic|gueuze|geuze|sour|wild|flanders|oude|brett|酸小麦|柏林|古斯|贵兹|兰比克|野菌|酸)' then '酸小麦'
    when lower(style) ~ '(\yipa\y|neipa|dipa|tipa|india pale|pale ale|\yapa\y|hazy|印度淡|浑浊|酒花)' then 'IPA'
    when lower(style) ~ '(stout|irish dry|imperial black|世涛|黑啤)' then '世涛'
    when lower(style) ~ '(porter|baltic|波特)' then '波特'
    when lower(style) ~ '(pilsner|pilsener|\ypils\y|bohemian|皮尔森|比尔森)' then '皮尔森'
    when lower(style) ~ '(wheat|witbier|hefeweizen|weizen|weisse|weissbier|blanche|white ale|小麦|白啤)' then '小麦'
    when lower(style) ~ '(trappist|abbey|tripel|dubbel|quadrupel|belgian strong|\yquad\y|修道院|三料|双料|四料|烈性)' then '修道院'
    when lower(style) ~ '(lager|helles|dunkel|marzen|märzen|bock|kolsch|kölsch|vienna|schwarz|拉格|海勒|博克|科隆|清爽)' then '拉格'
    when lower(btrim(category)) in ('hop', '酒花') then 'IPA'
    when lower(btrim(category)) in ('sour', '酸野菌') then '酸小麦'
    when lower(btrim(category)) in ('dark', '深色') then '世涛'
    when lower(btrim(category)) in ('strong', '烈性') then '修道院'
    else '拉格'
end
where lower(btrim(category)) in ('fresh', 'hop', 'sour', 'dark', 'strong', '清爽', '酒花', '酸野菌', '深色', '烈性');
