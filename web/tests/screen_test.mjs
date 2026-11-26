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

function boot({ routes = {}, cache = null, breakCache = false, search = '', width = 1920, height = 1080, events = false, fakeTimers = false } = {}) {
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
  const images = [];
  const fetch = url => {
    calls.push(url);
    const route = routes[url];
    if (typeof route === 'function') return route();
    if (!(url in routes)) {
      return Promise.resolve({ ok: false, status: 404, json: () => Promise.reject(new Error('404')) });
    }
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(route) });
  };

  const timers = [];
  let timerSeq = 0;
  const fakeSetTimeout = (fn, ms) => {
    const t = { id: ++timerSeq, fn, ms, canceled: false };
    timers.push(t);
    return t.id;
  };
  const fakeClearTimeout = id => {
    const t = timers.find(x => x.id === id);
    if (t) t.canceled = true;
  };
  const runTimers = () => {
    const due = timers.splice(0);
    due.forEach(t => { if (!t.canceled) t.fn(); });
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
  const esInstances = [];
  class EventSource {
    constructor(url) { esUrls.push(url); this.url = url; esInstances.push(this); }
    close() { this.closed = true; }
  }

  const sandbox = {
    document,
    window: { addEventListener() {}, innerWidth: width, innerHeight: height },
    location: { search },
    URL,
    URLSearchParams,
    fetch,
    localStorage,
    Image: class {
      constructor() { images.push(this); }
      set src(v) { this._src = v; }
      decode() { return Promise.resolve(); }
    },
    setInterval: () => 0,
    setTimeout: fakeTimers ? fakeSetTimeout : setTimeout,
    clearTimeout: fakeTimers ? fakeClearTimeout : clearTimeout,
    getComputedStyle: () => ({ fontSize: '16px', rowGap: '0px' }),
  };
  if (events) { sandbox.EventSource = EventSource; sandbox.window.EventSource = EventSource; }

  vm.createContext(sandbox);
  new vm.Script(code, { filename: 'screen/index.html' }).runInContext(sandbox);

  const flush = () => new Promise(r => setTimeout(r, 20));
  return { board, brand, clock, count, body, hint, calls, esUrls, esInstances, timers, runTimers, images, flush, store };
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

// 2. P1-10: 默认不回退到演示 playlist.json，失败保留既有真酒单与缓存；显式 ?demo=1 才回退且不写缓存。
{
  const r = boot({
    cache: { brand: '真实门店', items: [{ name: '门店真酒', abv: 6, size: '330ml', price: '¥ 48' }] },
    routes: {
      '/api/screen/playlist': {},
      'playlist.json': {
        items: [{ name: '朋克 IPA', poster: 'posters/punk.svg', abv: 5.2, size: '330ml', price: '¥ 45 / 杯' }],
      },
    },
  });
  await r.flush();
  assert.deepEqual(r.calls, ['/api/screen/playlist'], '默认失败绝不能悄悄切到演示 playlist.json');
  assert.equal(r.board.children.length, 1);
  assert.equal(byTag(r.board.children[0], 'h2')[0].textContent, '门店真酒');
  assert.equal(JSON.parse(r.store.get('screen:menu:v3')).items[0].name, '门店真酒', '失败不能污染本地缓存');

  const demo = boot({
    search: '?demo=1',
    cache: { brand: '真实门店', items: [{ name: '门店真酒', abv: 6 }] },
    routes: {
      '/api/screen/playlist': {},
      'playlist.json': {
        items: [{ name: '朋克 IPA', poster: 'posters/punk.svg', abv: 5.2, size: '330ml', price: '¥ 45 / 杯' }],
      },
    },
  });
  await demo.flush();
  assert.deepEqual(demo.calls, ['/api/screen/playlist', 'playlist.json']);
  assert.equal(byTag(demo.board.children[0], 'h2')[0].textContent, '朋克 IPA');
  assert.equal(byTag(demo.board.children[0], 'img')[0].src, 'posters/punk.svg');
  assert.equal(JSON.parse(demo.store.get('screen:menu:v3')).items[0].name, '门店真酒', '演示数据不能覆盖本地缓存');
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

// 11. P2-27：¥ 0 不占价格位；多杯型缺价显示「时价」，单杯缺主价回退杯型价。
{
  const r = boot({
    routes: {
      '/api/screen/playlist': {
        items: [
          { no: '01', name: '零价单杯', price: '¥ 0', size: '330ml' },
          {
            no: '02', name: '零价多杯',
            cups: [
              { ml: 150, size: '150ml', price: '¥ 0' },
              { ml: 330, size: '330ml', price: '¥ 58' },
            ],
          },
          { no: '03', name: '零价回退单杯', price: '¥ 0', cups: [{ ml: 330, size: '330ml', price: '¥ 45' }] },
        ],
      },
    },
  });
  await r.flush();
  const [a, b, c] = r.board.children;
  assert.equal(a.querySelector('.price'), null, '¥ 0 不能显示为价格');
  assert.ok(walk(a).some(n => n.className === 'spec' && n.textContent === '330ml杯型'), '杯型仍要显示');
  const tiers = walk(b).filter(n => n.className === 'cup-tier');
  assert.deepEqual(tiers.map(t => t.textContent), ['时价150ml', '¥ 58330ml']);
  assert.equal(c.querySelector('.price').textContent, '¥ 45', '单杯缺主价时回退杯型价');
}

// 12. P2-30：SSE 250ms 防抖合并，慢的旧响应不能覆盖新的酒单。
{
  let call = 0;
  const pending = [];
  const respond = body => ({ ok: true, status: 200, json: () => Promise.resolve(body) });
  const r = boot({
    events: true,
    fakeTimers: true,
    routes: {
      '/api/screen/playlist': () => {
        call++;
        if (call === 1) return Promise.resolve(respond({ items: [{ name: '初始酒', abv: 5 }] }));
        return new Promise(resolve => pending.push(body => resolve(respond(body))));
      },
    },
  });
  await r.flush();
  assert.equal(r.board.children.length, 1, '初始酒单先上屏');
  const es = r.esInstances[0];

  es.onmessage();
  es.onmessage();
  r.runTimers();
  assert.equal(r.calls.length, 2, '250ms 防抖窗口内只应重载一次');

  es.onmessage();
  r.runTimers();
  assert.equal(r.calls.length, 3);
  assert.equal(pending.length, 2);

  pending[1]({ items: [{ name: '新酒', abv: 6 }] });
  await r.flush();
  assert.equal(byTag(r.board.children[0], 'h2')[0].textContent, '新酒');

  pending[0]({ items: [{ name: '迟到酒', abv: 4 }] });
  await r.flush();
  assert.equal(byTag(r.board.children[0], 'h2')[0].textContent, '新酒', '迟到的慢响应必须丢弃');
}

// 13. P3-33：SSE 断开按 5s→15s→60s 退避重连，连上后重置；内容没变不重复预载海报。
{
  let call = 0;
  const r = boot({
    events: true,
    fakeTimers: true,
    routes: {
      '/api/screen/playlist': () => {
        call++;
        const item = call <= 2
          ? { name: '同款', poster: '/a.jpg' }
          : { name: '换款', poster: '/b.jpg' };
        return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ items: [item] }) });
      },
    },
  });
  await r.flush();
  assert.equal(r.images.length, 1, '首次上屏应预载海报');
  const first = r.esInstances[0];

  first.onmessage();
  r.runTimers();
  await r.flush();
  assert.equal(r.images.length, 1, '内容未变的刷新不应重复预载');

  first.onmessage();
  r.runTimers();
  await r.flush();
  assert.equal(r.images.length, 2, '内容变化后应预载新海报');

  first.onerror();
  assert.equal(r.esInstances.length, 1, '断线后不立刻重连');
  assert.equal(r.timers[r.timers.length - 1].ms, 5000);
  r.runTimers();
  assert.equal(r.esInstances.length, 2);
  const second = r.esInstances[1];

  second.onerror();
  assert.equal(r.timers[r.timers.length - 1].ms, 15000);
  r.runTimers();
  const third = r.esInstances[2];

  third.onerror();
  assert.equal(r.timers[r.timers.length - 1].ms, 60000);
  r.runTimers();
  const fourth = r.esInstances[3];

  fourth.onerror();
  assert.equal(r.timers[r.timers.length - 1].ms, 60000, '退避封顶 60s');
  r.runTimers();
  const fifth = r.esInstances[4];

  fifth.onopen();
  fifth.onerror();
  assert.equal(r.timers[r.timers.length - 1].ms, 5000, '连接成功后重置退避');
}

// 14. 屏蔽跨域 ?src= 与 ?events=；仅缓存 /api/screen/playlist 且未变更时不重写缓存。
{
  const r = boot({
    events: true,
    search: '?src=https://evil.example/menu.json&events=//evil.example/ev',
    routes: {
      '/api/screen/playlist': { brand: 'SAFE', items: [{ name: '本地酒', abv: 5 }] },
    },
  });
  await r.flush();
  assert.deepEqual(r.calls, ['/api/screen/playlist'], '跨域 ?src= 必须被忽略');
  assert.deepEqual(r.esUrls, ['/api/screen/events'], '跨域 ?events= 必须被忽略并回退默认 /api/screen/events');
}
{
  let sets = 0;
  let call = 0;
  const here2 = dirname(fileURLToPath(import.meta.url));
  const html2 = readFileSync(join(here2, '../templates/screen/screen.html'), 'utf8');
  const code2 = html2.match(/<script>([\s\S]*?)<\/script>/)[1];
  const ids = new Map();
  const mk = id => { const n = new Node('div'); n.id = id; ids.set(id, n); return n; };
  const board = mk('board'), brand = mk('brand'), clock = mk('clock'), count = mk('count');
  const hint = new Node('div'); hint.className = 'hint';
  const classes = new Set();
  const body = new Node('body');
  body.classList = { add: c => classes.add(c), remove: c => classes.delete(c), contains: c => classes.has(c), toggle: (c, on) => (on === undefined ? !classes.has(c) : !!on) ? (classes.add(c), true) : (classes.delete(c), false) };
  const store = new Map();
  const esList = [];
  class ES { constructor(u) { this.url = u; esList.push(this); } close() {} }
  const sandbox = {
    document: { body, documentElement: { requestFullscreen() {} }, createElement: t => new Node(t), createTextNode: t => { const n = new Node('#text'); n.textContent = t; return n; }, createDocumentFragment: () => { const f = new Node('#fragment'); f.fragment = true; return f; }, getElementById: id => ids.get(id) || null, querySelector: s => s === '.hint' ? hint : null, addEventListener() {} },
    window: { addEventListener() {}, innerWidth: 1920, innerHeight: 1080, EventSource: ES },
    EventSource: ES,
    location: { search: '?src=/custom.json' },
    URL,
    URLSearchParams,
    fetch: url => Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(url === '/custom.json' ? { brand: 'CUSTOM', items: [] } : { brand: 'API', items: [{ name: `酒${call}` }] }) }),
    localStorage: { getItem: k => store.get(k) || null, setItem: (k, v) => { sets++; store.set(k, v); } },
    Image: class { set src(_) {} decode() { return Promise.resolve(); } },
    setInterval: () => 0, setTimeout, clearTimeout,
    getComputedStyle: () => ({ fontSize: '16px', rowGap: '0px' }),
  };
  vm.createContext(sandbox);
  new vm.Script(code2).runInContext(sandbox);
  await new Promise(r => setTimeout(r, 30));
  assert.equal(sets, 0, '自定义 ?src=/custom.json 不应污染 /api/screen/playlist 本地缓存');

  const r2 = boot({
    events: true,
    fakeTimers: true,
    routes: {
      '/api/screen/playlist': () => {
        call++;
        return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ brand: 'PourOps', items: [{ name: call <= 2 ? 'A' : 'B' }] }) });
      },
    },
  });
  await r2.flush();
  const rawSet = r2.store.set.bind(r2.store);
  let writeCount = 0;
  r2.store.set = (k, v) => { writeCount++; return rawSet(k, v); };
  r2.esInstances[0].onmessage();
  r2.runTimers();
  await r2.flush();
  assert.equal(writeCount, 0, '酒单内容未变化时不应重写 localStorage');
  r2.esInstances[0].onmessage();
  r2.runTimers();
  await r2.flush();
  assert.equal(writeCount, 1, '酒单变化后应更新 localStorage');
}

console.log('web/templates/screen/screen.html: 14 组回归通过');
