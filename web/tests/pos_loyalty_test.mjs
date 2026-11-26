import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const scriptSrc = fs.readFileSync(path.join(__dirname, '../static/js/pos-loyalty.js'), 'utf8');

// 1. 纯辅助函数回归测试
const mod = { exports: {} };
vm.runInNewContext(scriptSrc, { module: mod, exports: mod.exports });
const { money, buildMemberMeta, buildPreviewSummary } = mod.exports;

assert.equal(money(4500), '¥ 45');
assert.equal(money(4550), '¥ 45.50');

const meta = buildMemberMeta({
  tier_name: '黑金会员',
  tier_discount_text: '9折',
  balance_text: '¥ 280',
  points: 350,
  active_pass: {
    plan_name: '31天畅饮月卡',
    benefit_type: 'free_quota',
    remaining_today: 1,
  },
});
assert.match(meta, /黑金会员 \(9折\)/);
assert.match(meta, /余额 ¥ 280/);
assert.match(meta, /350 积分/);
assert.match(meta, /今日剩 1 杯/);

const summary = buildPreviewSummary({
  discount_label: '31天畅饮月卡免1杯 + 黑金会员(9折)',
  discount_cents: 6200,
  balance_paid_cents: 2000,
  external_paid_cents: 1800,
  points_earned: 38,
}, 'cash');
assert.deepEqual(Array.from(summary), [
  '31天畅饮月卡免1杯 + 黑金会员(9折)',
  '共省 ¥ 62',
  '储值扣 ¥ 20 + 现金补差 ¥ 18',
  '返 38 积分',
]);

// 2. 最小 DOM 垫片验证防抖预览与过期响应丢弃
class FakeClassList {
  constructor(el) { this.el = el; }
  _arr() { return (this.el.className || '').split(/\s+/).filter(Boolean); }
  add(c) { const a = this._arr(); if (!a.includes(c)) { a.push(c); this.el.className = a.join(' '); } }
  remove(c) { this.el.className = this._arr().filter(x => x !== c).join(' '); }
  contains(c) { return this._arr().includes(c); }
  toggle(c, on) { const want = on === undefined ? !this.contains(c) : !!on; want ? this.add(c) : this.remove(c); return want; }
}

class FakeNode {
  constructor(tag, id = '') {
    this.tagName = tag.toUpperCase();
    this.id = id;
    this.className = '';
    this.dataset = {};
    this.value = '';
    this.checked = false;
    this.disabled = false;
    this.hidden = false;
    this.children = [];
    this.listeners = {};
    this._text = '';
    this.classList = new FakeClassList(this);
  }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  setAttribute() {}
  appendChild(n) { this.children.push(n); return n; }
  append(...ns) { ns.forEach(n => this.appendChild(n)); }
  replaceChildren(...ns) { this.children = [...ns]; }
  querySelectorAll(sel) {
    if (sel === 'input[name="items"]:checked') return this.checkedItems || [];
    if (sel === 'input[name="method"]:checked') return this.checkedMethod ? [this.checkedMethod] : [];
    if (sel === 'input[name="method"]') return this.methods || [];
    if (sel === 'input[name="method"][value="qr"]') return [];
    if (sel === 'input[name="method"][value="balance"]') return [];
    return [];
  }
  querySelector(sel) {
    const arr = this.querySelectorAll(sel);
    return arr[0] || null;
  }
  get textContent() { return this._text + this.children.map(c => c.textContent).join(''); }
  set textContent(v) { this._text = String(v); this.children = []; }
}

const byId = new Map();
const mk = (tag, id) => {
  const n = new FakeNode(tag, id);
  if (id) byId.set(id, n);
  return n;
};

const side = mk('aside');
side.dataset.orderId = '88';
const form = mk('form', 'pay-form');
const itemBox = mk('input');
itemBox.value = '101';
itemBox.checked = true;
form.checkedItems = [itemBox];
const qrMethod = mk('input');
qrMethod.value = 'qr';
qrMethod.checked = true;
form.checkedMethod = qrMethod;
form.methods = [qrMethod];

const selTotal = mk('strong', 'sel-total');
const selGross = mk('span', 'sel-gross');
const expInput = mk('input', 'expected-total-cents');
const payBtn = mk('button', 'pay-btn');
const previewBadges = mk('div', 'preview-badges');
const previewErr = mk('p', 'preview-err');
const memberIdInput = mk('input', 'member-id-input');
const seedEl = mk('script', 'bound-member-json');
seedEl.textContent = 'null';

let resolvers = [];
const fakeFetch = (url) => new Promise((resolve, reject) => {
  resolvers.push({ url, resolve, reject });
});

const win = {
  PourOpsPosLoyalty: null,
};
const doc = {
  readyState: 'complete',
  getElementById: id => byId.get(id) || null,
  querySelector: sel => (sel === '.side[data-order-id]' ? side : null),
  querySelectorAll: () => [],
  createElement: tag => new FakeNode(tag),
  addEventListener: () => {},
};

vm.runInNewContext(scriptSrc, {
  window: win,
  document: doc,
  fetch: fakeFetch,
  AbortController,
  URLSearchParams,
  setTimeout,
  clearTimeout,
});

assert.ok(win.PourOpsPosLoyalty && typeof win.PourOpsPosLoyalty.refresh === 'function');

// 触发两次连续预览请求，让第 2 次先返回（3800），第 1 次后返回（9999）；过期响应必须被丢弃！
await new Promise(r => setTimeout(r, 80));
assert.equal(resolvers.length, 1);
win.PourOpsPosLoyalty.refresh();
await new Promise(r => setTimeout(r, 80));
assert.equal(resolvers.length, 2);

resolvers[1].resolve({
  ok: true,
  json: async () => ({ pricing: { amount_cents: 3800, gross_cents: 4500, discount_cents: 700, discount_label: '85折' } }),
});
await new Promise(r => setTimeout(r, 10));
assert.equal(expInput.value, '3800');
assert.equal(selTotal.textContent, '¥ 38');

resolvers[0].resolve({
  ok: true,
  json: async () => ({ pricing: { amount_cents: 9999, gross_cents: 9999, discount_cents: 0 } }),
});
await new Promise(r => setTimeout(r, 10));
assert.equal(expInput.value, '3800', 'stale preview response must be ignored');
assert.equal(selTotal.textContent, '¥ 38');

console.log('pos_loyalty_test.mjs passed');
