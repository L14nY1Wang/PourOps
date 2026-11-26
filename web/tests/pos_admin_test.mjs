// POS 结账表单勾选与 Admin JS 交互回归：node web/tests/pos_admin_test.mjs
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const posHTML = readFileSync(join(here, '../templates/pos/pos.html'), 'utf8');
const posScript = posHTML.match(/<script>([\s\S]*?)<\/script>/)[1];
const adminScript = readFileSync(join(here, '../static/js/admin.js'), 'utf8');

function mkClassList(init = []) {
  const s = new Set(init);
  return {
    add: (...cs) => cs.forEach(c => s.add(c)),
    remove: (...cs) => cs.forEach(c => s.delete(c)),
    contains: c => s.has(c),
    toggle: (c, on) => {
      const want = on === undefined ? !s.has(c) : !!on;
      want ? s.add(c) : s.delete(c);
      return want;
    },
  };
}

function mkEl(tag = 'div', attrs = {}) {
  const listeners = new Map();
  const children = [];
  const el = {
    tagName: tag.toUpperCase(),
    children,
    className: attrs.className || '',
    classList: mkClassList((attrs.className || '').split(/\s+/).filter(Boolean)),
    dataset: { ...(attrs.dataset || {}) },
    style: {},
    attributes: new Map(),
    value: attrs.value ?? '',
    name: attrs.name ?? '',
    type: attrs.type ?? '',
    method: attrs.method ?? '',
    checked: Boolean(attrs.checked),
    disabled: Boolean(attrs.disabled),
    hidden: Boolean(attrs.hidden),
    inert: false,
    textContent: attrs.textContent ?? '',
    innerHTML: '',
    noValidate: false,
    labels: attrs.labels || [],
    placeholder: attrs.placeholder || '',
    _focused: false,
    _queries: new Map(),
    _queryAlls: new Map(),
    focus() { this._focused = true; },
    checkValidity() { return attrs.valid !== false; },
    reportValidity() {},
    closest(sel) {
      if (sel === 'a' && this.tagName === 'A') return this;
      return null;
    },
    before() {},
    append() {},
    setAttribute(k, v) {
      this.attributes.set(k, String(v));
      if (k.startsWith('data-')) {
        const key = k.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase());
        this.dataset[key] = String(v);
      }
    },
    getAttribute(k) { return this.attributes.has(k) ? this.attributes.get(k) : null; },
    hasAttribute(k) { return this.attributes.has(k) || (k === 'data-allow-multi' && Boolean(attrs.allowMulti)); },
    removeAttribute(k) { this.attributes.delete(k); },
    addEventListener(ev, fn) {
      if (!listeners.has(ev)) listeners.set(ev, []);
      listeners.get(ev).push(fn);
    },
    dispatch(ev, extra = {}) {
      let defaultPrevented = Boolean(extra.defaultPrevented);
      const eventObj = {
        type: ev,
        target: extra.target || el,
        key: extra.key || '',
        shiftKey: Boolean(extra.shiftKey),
        get defaultPrevented() { return defaultPrevented; },
        preventDefault() { defaultPrevented = true; },
      };
      for (const fn of listeners.get(ev) || []) fn(eventObj);
      return eventObj;
    },
    querySelector(sel) { return el._queries.get(sel) || null; },
    querySelectorAll(sel) { return el._queryAlls.get(sel) || []; },
  };
  return el;
}

// 1. 验证 POS 结账页 items 勾选 persistence、金额重算与 pageshow 解锁。
{
  const ss = new Map([['pos:unchecked:9', JSON.stringify(['102'])]]);
  const box1 = mkEl('input', { name: 'items', value: '101', checked: true });
  const box2 = mkEl('input', { name: 'items', value: '102', checked: true });
  const line1 = mkEl('div', { dataset: { amount: '4500' } });
  line1._queries.set('input[name="items"]', box1);
  const line2 = mkEl('div', { dataset: { amount: '3000' } });
  line2._queries.set('input[name="items"]', box2);

  const pickNone = mkEl('button', { dataset: { pick: 'none' } });
  const pickAll = mkEl('button', { dataset: { pick: 'all' } });
  const form = mkEl('form', { className: 'tile-form' });
  form._queryAlls.set('input[name="items"]', [box1, box2]);
  form._queryAlls.set('.line', [line1, line2]);
  form._queryAlls.set('[data-pick]', [pickNone, pickAll]);
  const submitBtn = mkEl('button', { type: 'submit', disabled: true });
  form._queryAlls.set('button[type="submit"], input[type="submit"]', [submitBtn]);

  const side = mkEl('aside', { dataset: { orderId: '9' } });
  const selTotal = mkEl('span');
  const selCount = mkEl('span');
  const payBtn = mkEl('button');
  const expInput = mkEl('input');
  const winListeners = new Map();
  const byId = new Map([
    ['pay-form', form],
    ['sel-total', selTotal],
    ['sel-count', selCount],
    ['pay-btn', payBtn],
    ['expected-total-cents', expInput],
  ]);

  const sandbox = {
    document: {
      getElementById: id => byId.get(id) || null,
      querySelector: sel => (sel === '.side[data-order-id]' ? side : null),
      querySelectorAll: sel => {
        if (sel.includes('#pay-form')) return [form];
        if (sel === 'form[data-pos-busy]') return form.dataset.posBusy ? [form] : [];
        return [];
      },
    },
    window: {
      addEventListener(ev, fn) { winListeners.set(ev, fn); },
      setTimeout: fn => { fn(); return 1; },
    },
    sessionStorage: {
      getItem: k => ss.get(k) || null,
      setItem: (k, v) => ss.set(k, v),
      removeItem: k => ss.delete(k),
    },
  };
  vm.createContext(sandbox);
  new vm.Script(posScript).runInContext(sandbox);

  assert.equal(box1.checked, true);
  assert.equal(box2.checked, false, '应从 sessionStorage 恢复未勾选状态');
  assert.equal(selTotal.textContent, '¥ 45');
  assert.equal(expInput.value, '4500');
  assert.equal(payBtn.disabled, false);

  pickNone.dispatch('click');
  assert.equal(payBtn.disabled, true, '全不选时禁用结账按钮');
  assert.equal(expInput.value, '0');

  pickAll.dispatch('click');
  assert.equal(selTotal.textContent, '¥ 75');
  assert.equal(expInput.value, '7500');

  const ev1 = form.dispatch('submit');
  assert.equal(ev1.defaultPrevented, false);
  assert.equal(submitBtn.disabled, true, '防重复提交应在下次提交前禁用按钮');
  const ev2 = form.dispatch('submit');
  assert.equal(ev2.defaultPrevented, true, '重复提交应被拦截');

  winListeners.get('pageshow')();
  assert.equal(submitBtn.disabled, false, 'pageshow 应恢复提交按钮');
  assert.equal(form.dataset.posBusy, undefined);
}

// 2. 验证 Admin JS：移动端侧边栏开关与 inert、confirm cancel 不锁按钮。
{
  const body = mkEl('body');
  const firstLink = mkEl('a');
  const sidebar = mkEl('aside');
  sidebar._queries.set('a', firstLink);
  sidebar._queryAlls.set('a, button', [firstLink]);
  const toggle = mkEl('button');
  const backdrop = mkEl('div');
  const workspace = mkEl('div');
  const formCancel = mkEl('form', { method: 'post' });
  const cancelBtn = mkEl('button', { type: 'submit' });
  formCancel._queryAlls.set('button[type="submit"], button:not([type]), input[type="submit"]', [cancelBtn]);

  const docListeners = new Map();
  const winListeners = new Map();
  const sandbox = {
    document: {
      documentElement: { dataset: { theme: 'light' } },
      body,
      activeElement: toggle,
      querySelector: sel => ({
        '.app-sidebar': sidebar,
        '.mobile-menu-toggle': toggle,
        '.sidebar-backdrop': backdrop,
        '.app-workspace': workspace,
      }[sel] || null),
      querySelectorAll: sel => (sel === 'form' ? [formCancel] : []),
      addEventListener(ev, fn) { docListeners.set(ev, fn); },
      dispatchEvent() {},
    },
    window: {
      matchMedia: () => ({ matches: true, addEventListener() {} }),
      addEventListener(ev, fn) { winListeners.set(ev, fn); },
      setTimeout: fn => { fn(); return 1; },
    },
    localStorage: { getItem: () => null, setItem() {} },
    Event: class { constructor(t) { this.type = t; } },
  };
  vm.createContext(sandbox);
  new vm.Script(adminScript).runInContext(sandbox);

  assert.equal(sidebar.inert, true, '初始闭合状态下移动端侧栏应 inert');
  toggle.dispatch('click');
  assert.equal(body.classList.contains('menu-open'), true);
  assert.equal(sidebar.inert, false);
  assert.equal(workspace.inert, true);
  assert.equal(backdrop.hidden, false);

  docListeners.get('keydown')({ key: 'Escape', preventDefault() {} });
  assert.equal(body.classList.contains('menu-open'), false, 'Escape 应关闭抽屉');
  assert.equal(workspace.inert, false);

  formCancel.dispatch('submit', { defaultPrevented: true });
  assert.equal(cancelBtn.disabled, false, 'onsubmit confirm 取消时不应禁用提交按钮');
  assert.equal(formCancel.dataset.submitting, undefined);
}

console.log('web/tests/pos_admin_test.mjs: POS 与后台交互回归通过');
