// 酒款检索脚本回归：node web/tests/beer_lookup_test.mjs
// 从 beer-lookup.js 直接加载真实脚本，在最小 DOM 垫片上跑，只测表单填充与请求时序。
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const code = readFileSync(join(here, '../static/js/beer-lookup.js'), 'utf8');

class ClassList {
  constructor(node) { this.node = node; }
  _get() { return this.node.className.split(/\s+/).filter(Boolean); }
  _set(list) { this.node.className = list.join(' '); }
  add(c) { const a = this._get(); if (!a.includes(c)) { a.push(c); this._set(a); } }
  remove(c) { this._set(this._get().filter(x => x !== c)); }
  contains(c) { return this._get().includes(c); }
  toggle(c, on) {
    const want = on === undefined ? !this.contains(c) : !!on;
    want ? this.add(c) : this.remove(c);
    return want;
  }
}

class Node {
  constructor(tag) {
    this.tagName = String(tag).toUpperCase();
    this.children = [];
    this.className = '';
    this.dataset = {};
    this.style = {};
    this.listeners = {};
    this.value = '';
    this.options = [];
    this.hidden = false;
    this.disabled = false;
    this._text = '';
    this._html = '';
    this.classList = new ClassList(this);
  }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  fire(type, ev = {}) { (this.listeners[type] || []).forEach(fn => fn(ev)); }
  dispatchEvent(ev) { this.fire(ev.type, ev); }
  appendChild(n) { this.children.push(n); n.parentElement = this; return n; }
  append(...ns) { ns.forEach(n => this.appendChild(n)); }
  replaceChildren(...ns) {
    this.children = [];
    ns.forEach(n => this.appendChild(n));
  }
  setAttribute(k, v) { this[k] = String(v); }
  removeAttribute(k) { delete this[k]; }
  querySelectorAll(sel) {
    const cls = String(sel).replace(/^\./, '');
    return walk(this).filter(n => n !== this && n.className.split(/\s+/).includes(cls));
  }
  scrollIntoView() {}
  get textContent() { return this._text + this.children.map(c => c.textContent).join(''); }
  set textContent(v) { this._text = String(v); this.children = []; }
  get innerHTML() { return this._html; }
  set innerHTML(v) { this._html = String(v); this.children = []; }
}

function walk(root) {
  const out = [];
  (function w(n) { out.push(n); (n.children || []).forEach(w); })(root);
  return out;
}

class DomEvent {
  constructor(type, opts = {}) {
    this.type = type;
    this.isTrusted = false;
    this.bubbles = !!opts.bubbles;
  }
}

function boot({ mode = 'purchase', onFetch, lookupDelay = () => 0, detailDelay = () => 0 } = {}) {
  const ids = new Map();
  const mk = (tag, id) => {
    const n = new Node(tag);
    if (id) { n.id = id; ids.set(id, n); }
    return n;
  };

  const wrap = mk('div', 'beer-lookup-wrap');
  wrap.dataset.mode = mode;
  const nameInput = mk('input', 'name');
  const searchBtn = mk('button', 'beer-lookup-btn');
  const menu = mk('div', 'beer-lookup-menu');
  menu.hidden = true;
  const statusEl = mk('div', 'beer-lookup-status');
  statusEl.hidden = true;

  const fieldIds = [
    'brewery', 'style', 'category', 'abv', 'ibu', 'ut_rating', 'ut_count', 'tags',
    'capacity_ml', 'cost', 'price_150', 'price_330', 'price_500',
  ];
  fieldIds.forEach(id => mk('input', id));
  mk('input', 'poster_url');
  mk('input', 'poster_file');
  mk('img', 'poster-preview-img');
  mk('div', 'poster-preview-fallback');
  mk('div', 'poster-placeholder-title');
  mk('div', 'poster-placeholder-note');

  const calls = [];
  const aborted = [];
  let lookupCalls = 0;

  const fetch = (url, opts = {}) => {
    calls.push(url);
    const isLookup = url.startsWith('/admin/beers/lookup?');
    if (isLookup) lookupCalls++;
    const delay = isLookup ? lookupDelay(lookupCalls) : detailDelay();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => resolve(mockResponse(url, onFetch)), delay);
      if (opts.signal) {
        opts.signal.addEventListener('abort', () => {
          clearTimeout(timer);
          aborted.push(url);
          const err = new Error('aborted');
          err.name = 'AbortError';
          reject(err);
        });
      }
    });
  };

  const document = {
    createElement: t => new Node(t),
    createTextNode: t => { const n = new Node('#text'); n.textContent = t; return n; },
    getElementById: id => ids.get(id) || null,
    dispatchEvent() {},
    addEventListener() {},
  };

  const sandbox = {
    document,
    window: {
      setTimeout,
      clearTimeout,
      URL: { createObjectURL: () => 'blob:preview', revokeObjectURL() {} },
      URLSearchParams,
    },
    URL: { createObjectURL: () => 'blob:preview', revokeObjectURL() {} },
    AbortController,
    Event: DomEvent,
    fetch,
    console,
    encodeURIComponent,
    setTimeout,
    clearTimeout,
  };

  vm.createContext(sandbox);
  new vm.Script(code, { filename: 'web/static/js/beer-lookup.js' }).runInContext(sandbox);

  const input = id => ids.get(id);
  const flush = (ms = 40) => new Promise(r => setTimeout(r, ms));
  const typeInto = (id, value) => {
    const el = input(id);
    el.value = value;
    el.fire('input', { isTrusted: true });
  };

  return { wrap, nameInput, searchBtn, menu, statusEl, input, flush, typeInto, calls, aborted, lookupCount: () => lookupCalls };
}

function mockResponse(url, onFetch) {
  const body = onFetch(url);
  return {
    ok: true,
    status: 200,
    json: () => Promise.resolve(body),
  };
}

const candidates = {
  items: [
    { source: 'catalog', id: 1, name: '候选酒', brewery: '候选厂', style: 'IPA' },
    { source: 'jiuhe', id: 7, name: '候选二号', brewery: '远端厂' },
  ],
};

// 1. P2-28：手工改过的字段不允许被自动填充覆盖，其余空字段照常填充。
{
  const r = boot({
    onFetch: url => (
      url.startsWith('/admin/beers/lookup?')
        ? candidates
        : { source: 'catalog', source_label: '本店历史酒单', brewery: '详情厂', style: 'IPA - American', abv: '5.2', ibu: '35' }
    ),
  });
  r.typeInto('name', '候选酒');
  r.typeInto('brewery', '我的厂牌');
  assert.equal(r.input('brewery').dataset.userEdited, '1', '手工编辑必须打上 userEdited 标记');
  r.searchBtn.fire('click');
  await r.flush(120);

  assert.equal(r.input('brewery').value, '我的厂牌', '用户改过的厂牌不能被自动填充覆盖');
  assert.equal(r.input('style').value, 'IPA - American', '未编辑字段应被自动填充');
  assert.equal(r.input('abv').value, '5.2', 'ABV 应被自动填充');
  assert.equal(r.input('style').dataset.autofilled, '1', '自动填充字段应标记 autofilled');
}

// 2. P2-28：编辑模式下，已有内容且非自动填充的字段保持原值，空字段仍补齐。
{
  const r = boot({
    mode: 'edit',
    onFetch: url => (
      url.startsWith('/admin/beers/lookup?')
        ? candidates
        : { source: 'catalog', brewery: '新厂', style: '新风格', abv: '6.6' }
    ),
  });
  r.input('brewery').value = '老厂';
  r.input('style').value = '老风格';
  r.typeInto('name', '候选酒');
  r.searchBtn.fire('click');
  await r.flush(120);

  assert.equal(r.input('brewery').value, '老厂', '编辑模式不能覆盖已填厂牌');
  assert.equal(r.input('style').value, '老风格', '编辑模式不能覆盖已填风格');
  assert.equal(r.input('abv').value, '6.6', '空字段应补齐');
}

// 3. P3-31：选定候选时清掉防抖并中止在途搜索，迟到的响应不能再把下拉弹开。
{
  const r = boot({
    onFetch: url => {
      if (url.startsWith('/admin/beers/lookup?')) return candidates;
      return { source: 'catalog', source_label: '本店历史酒单', brewery: '详情厂', style: 'IPA' };
    },
    // 第一次搜索立刻返回，第二次（在途时被中止）故意拖到 500ms 后才回。
    lookupDelay: n => (n === 1 ? 0 : 500),
  });

  // 非自动选择的搜索：拿到候选并展开下拉。
  r.typeInto('name', '候选');
  await r.flush(340);
  assert.equal(r.menu.hidden, false, '首次搜索应展开候选下拉');
  const items = r.menu.querySelectorAll('.lookup-item');
  assert.equal(items.length, 2, '应渲染两个候选');

  // 再次输入触发防抖搜索，等它真正发出请求后再选候选。
  r.typeInto('name', '候选X');
  await r.flush(340);
  assert.equal(r.lookupCount(), 2, '第二次搜索应已发出');

  items[0].fire('click');
  await r.flush(80);
  assert.equal(r.aborted.filter(u => u.includes('q=%E5%80%99%E9%80%89X')).length, 1, '选定候选必须中止在途的第二次搜索');
  assert.equal(r.menu.hidden, true, '选定后下拉应关闭');
  assert.ok(r.calls.some(u => u.includes('/admin/beers/lookup/detail?source=catalog&id=1')), '应拉取所选候选详情');

  // 等过第二次搜索原本的返回时间，确认它没有迟到重开下拉或覆盖状态。
  await r.flush(550);
  assert.equal(r.menu.hidden, true, '迟到响应不能重新弹开下拉');
  assert.equal(r.lookupCount(), 2, '中止的搜索不应再发请求');
  assert.ok(!r.statusEl.textContent.includes('未搜到'), '迟到响应不能覆盖当前状态');
}

// 4. P3-31：检索关键词占着 #name 的 userEdited 标记，选定候选后酒名仍须被详情覆盖；
//    用户自己改过的其他字段（如厂牌）继续保留。
{
  const r = boot({
    onFetch: url => (
      url.startsWith('/admin/beers/lookup?')
        ? candidates
        : { source: 'catalog', source_label: '本店历史酒单', name: '候选酒', brewery: '详情厂', style: 'IPA' }
    ),
  });
  r.typeInto('name', '候选');
  r.typeInto('brewery', '自填厂');
  await r.flush(340);
  const items = r.menu.querySelectorAll('.lookup-item');
  assert.equal(r.menu.hidden, false, '键入关键词应展开候选下拉');
  items[0].fire('click');
  await r.flush(80);

  assert.equal(r.input('name').value, '候选酒', '选定候选后酒名应被详情覆盖');
  assert.equal(r.input('brewery').value, '自填厂', '用户手填的厂牌不能被覆盖');
}

console.log('web/static/js/beer-lookup.js: 4 组回归通过');
