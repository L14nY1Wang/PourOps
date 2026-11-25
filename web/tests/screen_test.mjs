// 电视屏脚本回归：node web/tests/screen_test.mjs
// 从 screen.html 里抽出内联脚本，在最小 DOM 垫片上跑，只测真实渲染逻辑（不引依赖）。
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const html = readFileSync(join(here, '../templates/screen/screen.html'), 'utf8');
const code = html.match(/<script>([\s\S]*?)<\/script>/)[1];

class Node {
  constructor(tag) {
    this.tagName = String(tag).toUpperCase();
    this.children = [];
    this.className = '';
    this._text = '';
    this.style = { display: '', setProperty(k, v) { this[k] = String(v); } };
    this.parentElement = null;
  }
  appendChild(n) { this.children.push(n); n.parentElement = this; return n; }
  insertBefore(n, ref) {
    const i = this.children.indexOf(ref);
    this.children.splice(i < 0 ? this.children.length : i, 0, n);
    n.parentElement = this;
    return n;
  }
  replaceChildren(...ns) {
    const kids = [];
    for (const n of ns) n.fragment ? kids.push(...n.children) : kids.push(n);
    this.children = kids;
    kids.forEach(k => { k.parentElement = this; });
  }
  querySelector(sel) {
    const cls = String(sel).replace(/^\./, '');
    return walk(this).find(n => n !== this && n.className.split(/\s+/).includes(cls)) || null;
  }
  querySelectorAll() { return []; }
  getBoundingClientRect() { return { width: 0, height: 0 }; }
  get offsetWidth() { return 0; }
  get offsetHeight() { return 0; }
  get clientWidth() { return 0; }
  get clientHeight() { return 0; }
  get scrollWidth() { return 0; }
  get scrollHeight() { return 0; }
  set textContent(v) { this._text = String(v); this.children = []; }
  get textContent() { return this._text + this.children.map(c => c.textContent).join(''); }
  addEventListener() {}
}

function walk(root) {
  const out = [];
  (function w(n) { out.push(n); (n.children || []).forEach(w); })(root);
  return out;
}

const byTag = (root, tag) => walk(root).filter(n => n.tagName === tag.toUpperCase());

function boot({ routes = {}, cache = null, breakCache = false, search = '', width = 1920, height = 1080, events = false } = {}) {
  const ids = new Map();
  const mk = id => { const n = new Node('div'); n.id = id; ids.set(id, n); return n; };
  const board = mk('board');
  const brand = mk('brand');
  const clock = mk('clock');
  const count = mk('count');
  const hint = new Node('div');
  hint.className = 'hint';
  hint.textContent = '正在同步酒单';

  const classes = new Set();
  const body = new Node('body');
  body.classList = {
    add: c => classes.add(c),
    remove: c => classes.delete(c),
    contains: c => classes.has(c),
    toggle: (c, on) => {
      const want = on === undefined ? !classes.has(c) : !!on;
      want ? classes.add(c) : classes.delete(c);
      return want;
    },
  };

  const document = {
    fullscreenElement: null,
    body,
    documentElement: { requestFullscreen() {} },
    createElement: t => new Node(t),
    createTextNode: t => { const n = new Node('#text'); n.textContent = t; return n; },
    createDocumentFragment: () => { const f = new Node('#fragment'); f.fragment = true; return f; },
    getElementById: id => ids.get(id) || null,
    querySelector: sel => (sel === '.hint' ? hint : null),
    addEventListener() {},
    exitFullscreen() {},
  };

  const calls = [];
  const fetch = url => {
    calls.push(url);
    if (!(url in routes)) {
      return Promise.resolve({ ok: false, status: 404, json: () => Promise.reject(new Error('404')) });
    }
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(routes[url]) });
  };

  const store = new Map(cache ? [['screen:menu:v3', JSON.stringify(cache)]] : []);
  const localStorage = {
    getItem: k => (store.has(k) ? store.get(k) : null),
    setItem: (k, v) => {
      if (breakCache) throw new Error('QuotaExceededError');
      store.set(k, String(v));
    },
  };

  const esUrls = [];
  class EventSource {
    constructor(url) { esUrls.push(url); }
    close() {}
  }

  const sandbox = {
    document,
    window: { addEventListener() {}, innerWidth: width, innerHeight: height },
    location: { search },
    URLSearchParams,
    fetch,
    localStorage,
    Image: class { set src(v) { this._src = v; } decode() { return Promise.resolve(); } },
    setInterval: () => 0,
    setTimeout,
    clearTimeout,
    getComputedStyle: () => ({ fontSize: '16px', rowGap: '0px' }),
  };
  if (events) { sandbox.EventSource = EventSource; sandbox.window.EventSource = EventSource; }

  vm.createContext(sandbox);
  new vm.Script(code, { filename: 'screen/index.html' }).runInContext(sandbox);

  const flush = () => new Promise(r => setTimeout(r, 20));
  return { board, brand, clock, count, body, hint, calls, esUrls, flush, store };
}

const EMPTY = { brand: 'NEW PIER', show_clock: true, items: [] };
const menu = (n, extra = () => ({})) => ({
  brand: 'PourOps',
  items: Array.from({ length: n }, (_, i) => ({ name: `酒款 ${i + 1}`, abv: 5, size: '330ml', price: '¥ 40', ...extra(i) })),
});

// 1. 合法空响应：清掉缓存里的陈酒，且不把空酒单当坏源继续回退到静态文件。
{
  const r = boot({
    cache: { brand: 'OLD', items: [{ name: '旧酒', abv: 5 }] },
    routes: { '/api/screen/playlist': EMPTY, 'playlist.json': { items: [{ name: '静态酒' }] } },
  });
  assert.equal(r.board.children.length, 1, '缓存先上屏');
  await r.flush();
  assert.equal(r.board.children.length, 0, '空 items 必须清空酒单');
  assert.deepEqual(r.calls, ['/api/screen/playlist'], '空响应是有效源，不该继续回退');
  assert.equal(r.brand.textContent, 'NEW PIER');
  assert.equal(r.hint.textContent, '暂无酒款');
  assert.ok(r.body.classList.contains('empty'));
}

// 2. 结构错误（{}）：换下一个源；酒标、价格等字段保留。
{
  const r = boot({
    routes: {
      '/api/screen/playlist': {},
      'playlist.json': {
        items: [{ name: '朋克 IPA', poster: 'posters/punk.svg', abv: 5.2, size: '330ml', price: '¥ 45 / 杯' }],
      },
    },
  });
  await r.flush();
  assert.deepEqual(r.calls, ['/api/screen/playlist', 'playlist.json']);
  assert.equal(r.board.children.length, 1);
  const [card] = r.board.children;
  assert.equal(byTag(card, 'h2')[0].textContent, '朋克 IPA');
  assert.equal(byTag(card, 'img')[0].src, 'posters/punk.svg');
  assert.equal(card.querySelector('.price').textContent, '¥ 45 / 杯');
  assert.ok(!r.body.classList.contains('empty'));
}

// 3. 缓存写失败不能丢掉已经拿到的有效响应，也不能多打一次源。
{
  const r = boot({
    breakCache: true,
    routes: { '/api/screen/playlist': { items: [{ name: '世涛' }] }, 'playlist.json': { items: [] } },
  });
  await r.flush();
  assert.deepEqual(r.calls, ['/api/screen/playlist']);
  assert.equal(r.board.children.length, 1);
  assert.equal(byTag(r.board.children[0], 'h2')[0].textContent, '世涛');
}

// 4. 全部源都不可用：空屏 + 同步提示，不渲染伪数据。
{
  const r = boot({ routes: { '/api/screen/playlist': {}, 'playlist.json': { nope: 1 } } });
  await r.flush();
  assert.equal(r.board.children.length, 0);
  assert.ok(r.body.classList.contains('empty'));
  assert.equal(r.hint.textContent, '正在同步酒单');
}

// 5. 12 酒头在 1920×1080 排 4×3；窄端（1080×1920）收成 3×4，不把卡片压扁。
{
  const wide = boot({ routes: { '/api/screen/playlist': menu(12) } });
  await wide.flush();
  assert.equal(wide.board.style['--cols'], '4');
  assert.equal(wide.board.style['--rows'], '3');
  assert.equal(wide.board.children.length, 12);

  const narrow = boot({ routes: { '/api/screen/playlist': menu(12) }, width: 1080, height: 1920 });
  await narrow.flush();
  assert.equal(narrow.board.style['--cols'], '3');
  assert.equal(narrow.board.style['--rows'], '4');
}

// 6. 售罄/无海报/长酒名：编号、品名、杯型、价格、状态全部保留，不靠图片也不截断。
{
  const r = boot({
    routes: {
      '/api/screen/playlist': {
        items: [{
          no: '11', brewery: '泵', name: '诺亚山核桃泥蛋糕世涛',
          tags: ['山核桃', '巧克力', '甜点'], abv: 11, ibu: 60, size: '330ml',
          price: '¥ 198', sold: false,
        }],
      },
    },
  });
  await r.flush();
  const [card] = r.board.children;
  assert.ok(card.className.includes('off'));
  assert.equal(card.querySelector('.tap').textContent, '11TAP');
  assert.equal(byTag(card, 'h2')[0].textContent, '诺亚山核桃泥蛋糕世涛');
  assert.equal(card.querySelector('.status').textContent, '售罄');
  assert.equal(card.querySelector('.price').textContent, '¥ 198');
  const specs = walk(card).filter(n => n.className === 'spec').map(n => n.textContent);
  assert.ok(specs.includes('330ml杯型'), '杯型必须可见');
  assert.equal(byTag(card, 'img').length, 0, '无海报时不生成图片节点');
  assert.equal(byTag(card, 'h2')[0].textContent.includes('…'), false, '长酒名不得用省略号截断');
}

// 7. ?src= 优先，且静态源不订阅 SSE；API 源才订阅。
{
  const r = boot({
    search: '?src=/custom/menu.json',
    routes: { '/custom/menu.json': { items: [{ name: '自定义酒款' }] } },
    events: true,
  });
  await r.flush();
  assert.deepEqual(r.calls, ['/custom/menu.json']);
  assert.deepEqual(r.esUrls, []);
  assert.equal(r.board.children.length, 1);

  const api = boot({ routes: { '/api/screen/playlist': menu(2) }, events: true });
  await api.flush();
  assert.deepEqual(api.esUrls, ['/api/screen/events']);
}

// 8. 顶栏统计：总款数 + 售罄数，不伪装成"在线"状态。
{
  const r = boot({ routes: { '/api/screen/playlist': menu(12, i => ({ sold: i !== 2 })) } });
  await r.flush();
  assert.equal(r.count.textContent, '12 款 · 1 售罄');
}

// 9. 酒款分类显示：支持新精酿分类、旧值兼容以及后台自定义分类名称。
{
  const r = boot({
    routes: {
      '/api/screen/playlist': {
        items: [
          { no: '01', name: '苹果西打', category: '西打', style: 'Dry Cider' },
          { no: '02', name: '农舍赛松', category: '赛松', style: 'Farmhouse Saison' },
          { no: '03', name: '旧酒花款', category: 'hop', style: 'American IPA' },
          { no: '04', name: '贵兹酸啤', style: 'Lambic - Gueuze' },
        ],
      },
    },
  });
  await r.flush();
  const [c1, c2, c3, c4] = r.board.children;
  assert.equal(c1.querySelector('.cat').textContent, '西打');
  assert.match(c1.style['--tone'], /^#[0-9a-f]{6}$/i);
  assert.equal(c2.querySelector('.cat').textContent, '赛松');
  assert.match(c2.style['--tone'], /^#[0-9a-f]{6}$/i);
  assert.equal(c3.querySelector('.cat').textContent, 'IPA');
  assert.equal(c3.style['--tone'], '#57cf9b');
  assert.equal(c4.querySelector('.cat').textContent, '酸艾尔');
  assert.equal(c4.style['--tone'], '#a48bf0');

  const allDefaults = ['IPA', '拉格', '皮尔森', '小麦', '酸小麦', '酸艾尔', '果泥', '西打', '世涛'];
  const allScreen = boot({
    routes: {
      '/api/screen/playlist': {
        items: allDefaults.map((cat, idx) => ({ no: String(idx + 1), name: `酒款 ${cat}`, category: cat })),
      },
    },
  });
  await allScreen.flush();
  const tones = allScreen.board.children.map(card => card.style['--tone'].toLowerCase());
  assert.equal(new Set(tones).size, allDefaults.length, '9 个默认分类在电视屏上应有 9 种互不重复的色调');
}

// 10. 多杯型分档价：同时配置多个杯型时，卡片分别展示每档杯型与价格，单杯保持兼容。
{
  const r = boot({
    routes: {
      '/api/screen/playlist': {
        items: [
          {
            no: '01', name: '三杯型 IPA', abv: 6.8, ibu: 72,
            size: '150/330/500ml', price: '¥ 58', sold: true,
            cups: [
              { ml: 150, size: '150ml', price: '¥ 28' },
              { ml: 330, size: '330ml', price: '¥ 58' },
              { ml: 500, size: '500ml', price: '¥ 78' },
            ],
          },
          {
            no: '02', name: '双杯型售罄', abv: 5.2, ibu: 35,
            size: '150/330ml', price: '¥ 48', sold: false,
            cups: [
              { ml: 150, size: '150ml', price: '¥ 25' },
              { ml: 330, size: '330ml', price: '¥ 48' },
            ],
          },
          {
            no: '03', name: '单杯兼容款', abv: 4.5,
            cups: [{ ml: 330, size: '330ml', price: '¥ 45' }],
          },
        ],
      },
    },
  });
  await r.flush();
  const [c1, c2, c3] = r.board.children;

  // 3 杯型：展示 3 个 .cup-tier，且底部规格不再重复展示 150/330/500ml 杯型
  const price1 = c1.querySelector('.price');
  assert.ok(price1.className.includes('price--multi'));
  assert.ok(!price1.className.includes('price--2'));
  const tiers1 = walk(price1).filter(n => n.className === 'cup-tier');
  assert.deepEqual(tiers1.map(t => t.textContent), ['¥ 28150ml', '¥ 58330ml', '¥ 78500ml']);
  const specs1 = walk(c1).filter(n => n.className === 'spec').map(n => n.textContent);
  assert.deepEqual(specs1, ['6.8%ABV', '72IBU']);

  // 2 杯型售罄：带 price--2 与 card.off
  assert.ok(c2.className.includes('off'));
  const price2 = c2.querySelector('.price');
  assert.ok(price2.className.includes('price--multi') && price2.className.includes('price--2'));
  const tiers2 = walk(price2).filter(n => n.className === 'cup-tier');
  assert.deepEqual(tiers2.map(t => t.textContent), ['¥ 25150ml', '¥ 48330ml']);

  // 单杯：保留单主价与规格里的杯型栏
  const price3 = c3.querySelector('.price');
  assert.equal(price3.className, 'price');
  assert.equal(price3.textContent, '¥ 45');
  const specs3 = walk(c3).filter(n => n.className === 'spec').map(n => n.textContent);
  assert.ok(specs3.includes('330ml杯型'));
}

console.log('web/templates/screen/screen.html: 10 组回归通过');
