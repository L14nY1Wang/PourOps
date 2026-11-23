import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const scriptSrc = fs.readFileSync(path.join(__dirname, '../static/js/keg-pricing.js'), 'utf8');

function toCamel(attr) {
  return attr.replace(/^data-/, '').replace(/-([a-z])/g, (_, ch) => ch.toUpperCase());
}

function createFakeDOM() {
  const docListeners = new Map();

  class FakeClassList {
    constructor(el) {
      this.el = el;
    }
    _list() {
      return this.el.className.split(/\s+/).filter(Boolean);
    }
    add(...names) {
      const set = new Set(this._list());
      names.forEach(n => set.add(n));
      this.el.className = Array.from(set).join(' ');
    }
    remove(...names) {
      const drop = new Set(names);
      this.el.className = this._list().filter(n => !drop.has(n)).join(' ');
    }
    contains(name) {
      return this._list().includes(name);
    }
  }

  class FakeText {
    constructor(text) {
      this.nodeType = 3;
      this._text = String(text);
    }
    get textContent() {
      return this._text;
    }
    set textContent(v) {
      this._text = String(v);
    }
  }

  class FakeElement {
    constructor(tagName) {
      this.nodeType = 1;
      this.tagName = tagName.toUpperCase();
      this.id = '';
      this.className = '';
      this.value = '';
      this.type = '';
      this.min = '';
      this.max = '';
      this.step = '';
      this.hidden = false;
      this.attrs = {};
      this.childNodes = [];
      this.listeners = new Map();
      this.classList = new FakeClassList(this);
      this.dataset = new Proxy({}, {
        get: (_, prop) => {
          const attr = 'data-' + String(prop).replace(/[A-Z]/g, m => '-' + m.toLowerCase());
          return this.attrs[attr];
        },
        set: (_, prop, val) => {
          const attr = 'data-' + String(prop).replace(/[A-Z]/g, m => '-' + m.toLowerCase());
          this.attrs[attr] = String(val);
          return true;
        },
      });
    }

    get children() {
      return this.childNodes.filter(n => n.nodeType === 1);
    }

    get textContent() {
      return this.childNodes.map(n => n.textContent).join('');
    }

    set textContent(v) {
      this.childNodes = v === '' ? [] : [new FakeText(v)];
    }

    setAttribute(name, value) {
      const val = String(value);
      this.attrs[name] = val;
      if (name === 'id') this.id = val;
      if (name === 'class') this.className = val;
    }

    getAttribute(name) {
      if (name === 'id') return this.id || null;
      if (name === 'class') return this.className || null;
      return Object.prototype.hasOwnProperty.call(this.attrs, name) ? this.attrs[name] : null;
    }

    appendChild(node) {
      this.append(node);
      return node;
    }

    append(...nodes) {
      nodes.forEach(n => {
        if (typeof n === 'string') {
          this.childNodes.push(new FakeText(n));
        } else if (n) {
          this.childNodes.push(n);
        }
      });
    }

    replaceChildren(...nodes) {
      this.childNodes = [];
      this.append(...nodes);
    }

    addEventListener(type, fn) {
      if (!this.listeners.has(type)) this.listeners.set(type, []);
      this.listeners.get(type).push(fn);
    }

    dispatchEvent(ev) {
      const list = this.listeners.get(ev.type) || [];
      list.forEach(fn => fn.call(this, ev));
      return true;
    }

    click() {
      this.dispatchEvent({ type: 'click', target: this });
    }

    _matchesSimple(sel) {
      let rest = sel.trim();
      if (!rest) return false;
      const tagMatch = rest.match(/^[a-zA-Z0-9_-]+/);
      if (tagMatch) {
        if (this.tagName !== tagMatch[0].toUpperCase()) return false;
        rest = rest.slice(tagMatch[0].length);
      }
      while (rest.length > 0) {
        if (rest.startsWith('#')) {
          const m = rest.match(/^#([a-zA-Z0-9_-]+)/);
          if (!m || this.id !== m[1]) return false;
          rest = rest.slice(m[0].length);
        } else if (rest.startsWith('.')) {
          const m = rest.match(/^\.([a-zA-Z0-9_-]+)/);
          if (!m || !this.classList.contains(m[1])) return false;
          rest = rest.slice(m[0].length);
        } else if (rest.startsWith('[')) {
          const m = rest.match(/^\[([a-zA-Z0-9_-]+)(?:="([^"]*)")?\]/);
          if (!m) return false;
          const attrName = m[1];
          const hasAttr = attrName.startsWith('data-')
            ? Object.prototype.hasOwnProperty.call(this.attrs, attrName) || Object.prototype.hasOwnProperty.call(this.dataset, toCamel(attrName))
            : this.getAttribute(attrName) !== null;
          if (!hasAttr) return false;
          if (m[2] !== undefined && this.getAttribute(attrName) !== m[2]) return false;
          rest = rest.slice(m[0].length);
        } else {
          return false;
        }
      }
      return true;
    }

    querySelectorAll(selector) {
      const parts = selector.trim().split(/\s+/);
      const walk = (node, depth) => {
        const out = [];
        for (const child of node.children) {
          if (child._matchesSimple(parts[depth])) {
            if (depth === parts.length - 1) {
              out.push(child);
            } else {
              out.push(...walk(child, depth + 1));
            }
          }
          out.push(...walk(child, depth));
        }
        return out;
      };
      return Array.from(new Set(walk(this, 0)));
    }

    querySelector(selector) {
      return this.querySelectorAll(selector)[0] || null;
    }
  }

  const root = new FakeElement('body');
  const document = {
    body: root,
    createElement(tag) {
      return new FakeElement(tag);
    },
    createTextNode(text) {
      return new FakeText(text);
    },
    getElementById(id) {
      const find = node => {
        for (const child of node.children) {
          if (child.id === id) return child;
          const nested = find(child);
          if (nested) return nested;
        }
        return null;
      };
      return find(root);
    },
    addEventListener(type, fn) {
      if (!docListeners.has(type)) docListeners.set(type, []);
      docListeners.get(type).push(fn);
    },
    dispatchEvent(ev) {
      const list = docListeners.get(ev.type) || [];
      list.forEach(fn => fn.call(document, ev));
      return true;
    },
  };

  return { document, FakeElement };
}

function el(doc, tag, opts = {}, children = []) {
  const node = doc.createElement(tag);
  if (opts.id) node.id = opts.id;
  if (opts.className) node.className = opts.className;
  if (opts.value !== undefined) node.value = String(opts.value);
  if (opts.attrs) {
    Object.entries(opts.attrs).forEach(([k, v]) => node.setAttribute(k, v));
  }
  if (opts.text !== undefined) node.textContent = opts.text;
  if (children.length) node.append(...children);
  return node;
}

function runScript(doc) {
  const window = {
    setTimeout: () => 1,
    document: doc,
  };
  const Event = function Event(type) {
    this.type = type;
  };
  vm.runInNewContext(scriptSrc, {
    document: doc,
    window,
    Event,
    Number,
    String,
    Math,
    Array,
    Object,
    parseInt,
    isFinite: Number.isFinite,
  });
}

// 场景 1-4：表单页 #keg-pricing-guide（purchase / form / catalog_form 共用）
{
  const { document } = createFakeDOM();
  const capEl = el(document, 'input', { id: 'capacity_ml', value: '20000' });
  const costEl = el(document, 'input', { id: 'cost', value: '1200' });
  const p150 = el(document, 'input', { id: 'price_150', value: '' });
  const p330 = el(document, 'input', { id: 'price_330', value: '50' });
  const p500 = el(document, 'input', { id: 'price_500', value: '' });
  const guide = el(document, 'div', { id: 'keg-pricing-guide' });
  document.body.append(capEl, costEl, p150, p330, p500, guide);

  runScript(document);

  const sellSlider = document.getElementById('keg-pricing-sellable');
  const marginSlider = document.getElementById('keg-pricing-margin');
  const resetBtn = guide.querySelector('.keg-pricing-reset');
  const sellVal = guide.querySelector('[data-role="sellable-val"]');
  const marginVal = guide.querySelector('[data-role="margin-val"]');
  const output = guide.querySelector('.keg-pricing-output');

  assert.ok(sellSlider && marginSlider && resetBtn && sellVal && marginVal && output);
  assert.equal(sellSlider.value, '80');
  assert.equal(marginSlider.value, '50');
  assert.equal(sellVal.textContent, '16000ml（80%）');
  assert.equal(marginVal.textContent, '50%');
  assert.equal(resetBtn.hidden, true);
  assert.match(output.textContent, /16000ml（预留损耗 4000ml）/);
  assert.match(output.textContent, /¥2400/);
  assert.match(output.textContent, /¥23/);
  assert.match(output.textContent, /¥50/);
  assert.match(output.textContent, /¥75/);

  // 2. 拖动可售比例至 90%，验证滑块 DOM 未被销毁重建且实时重算
  sellSlider.value = '90';
  sellSlider.dispatchEvent({ type: 'input', target: sellSlider });
  assert.equal(document.getElementById('keg-pricing-sellable'), sellSlider, '滑块节点不应在 render 时被重建');
  assert.equal(sellVal.textContent, '18000ml（90%）');
  assert.equal(resetBtn.hidden, false);
  assert.match(output.textContent, /18000ml（预留损耗 2000ml）/);
  assert.match(output.textContent, /¥20/);
  assert.match(output.textContent, /¥44/);
  assert.match(output.textContent, /¥67/);

  // 搜索填充触发 keg:pricing-sync 时，不应重置用户拖动中的滑块状态
  capEl.value = '30000';
  document.dispatchEvent({ type: 'keg:pricing-sync' });
  assert.equal(sellSlider.value, '90');
  assert.equal(sellVal.textContent, '27000ml（90%）');
  capEl.value = '20000';
  document.dispatchEvent({ type: 'keg:pricing-sync' });

  // 3. 恢复可售 80% 并拖动毛利率至 60%，验证目标营收、回本线、一键套用
  sellSlider.value = '80';
  sellSlider.dispatchEvent({ type: 'input', target: sellSlider });
  marginSlider.value = '60';
  marginSlider.dispatchEvent({ type: 'input', target: marginSlider });
  assert.equal(document.getElementById('keg-pricing-margin'), marginSlider);
  assert.equal(marginVal.textContent, '60%');
  assert.match(output.textContent, /¥3000/);
  assert.match(output.textContent, /约售 6400ml/);
  assert.match(output.textContent, /¥29/);
  assert.match(output.textContent, /¥62/);
  assert.match(output.textContent, /¥94/);

  const applyAllBtn = output.querySelector('button.button');
  assert.ok(applyAllBtn && applyAllBtn.textContent.includes('一键套用 60% 推荐价'));
  applyAllBtn.click();
  assert.equal(p150.value, '29');
  assert.equal(p330.value, '62');
  assert.equal(p500.value, '94');

  // 4. 点击恢复默认按钮
  resetBtn.click();
  assert.equal(sellSlider.value, '80');
  assert.equal(marginSlider.value, '50');
  assert.equal(resetBtn.hidden, true);
  assert.match(output.textContent, /¥23/);
  assert.match(output.textContent, /¥50/);
  assert.match(output.textContent, /¥75/);

  // 奇数 sellableML（19999 * 80% = 15999ml）下理论回本量应向下取整为 7999ml，与 Go sml/2 一致
  capEl.value = '19999';
  capEl.dispatchEvent({ type: 'input', target: capEl });
  assert.match(output.textContent, /约售 7999ml/);
}

// 场景 5：酒桶详情页 #keg-detail-pricing 表格实时试算
{
  const { document } = createFakeDOM();
  const makeRow = (ml, actualCents, initialRec, initialStatus, statusCls) =>
    el(document, 'tr', { attrs: { 'data-ml': String(ml), 'data-actual-cents': String(actualCents) } }, [
      el(document, 'td', {}, [el(document, 'strong', { text: `${ml}ml` })]),
      el(document, 'td', { attrs: { 'data-role': 'eff-cost' }, text: '—' }),
      el(document, 'td', { attrs: { 'data-role': 'rec-price' } }, [el(document, 'strong', { className: 'text-primary', text: initialRec })]),
      el(document, 'td', {}, [el(document, 'strong', { text: `¥ ${actualCents / 100}` })]),
      el(document, 'td', { className: 'num text-success', attrs: { 'data-role': 'margin' }, text: '50.0%' }),
      el(document, 'td', { attrs: { 'data-role': 'breakeven' }, text: '24 杯' }),
      el(document, 'td', { attrs: { 'data-role': 'sellable-cups' }, text: '48 杯' }),
      el(document, 'td', { attrs: { 'data-role': 'status' } }, [el(document, 'span', { className: `verdict-pill ${statusCls}`, text: initialStatus })]),
    ]);

  const controlsHost = el(document, 'div', { id: 'keg-detail-pricing-controls' });
  const section = el(document, 'section', {
    id: 'keg-detail-pricing',
    attrs: { 'data-capacity-ml': '20000', 'data-cost-cents': '120000' },
  }, [
    el(document, 'div', { className: 'panel-heading' }, [
      el(document, 'div', {}, [
        el(document, 'h2', { text: '单杯成本、回本杯数与 50% 毛利率推荐售价' }),
        el(document, 'p', { attrs: { 'data-role': 'pricing-subtitle' }, text: '初始副标题' }),
      ]),
    ]),
    controlsHost,
    el(document, 'table', {}, [
      el(document, 'thead', {}, [
        el(document, 'tr', {}, [
          el(document, 'th', { text: '杯型' }),
          el(document, 'th', { attrs: { 'data-role': 'cost-head' }, text: '80% 有效成本' }),
          el(document, 'th', { attrs: { 'data-role': 'rec-head' }, text: '50% 推荐售价' }),
          el(document, 'th', { text: '当前设定售价' }),
          el(document, 'th', { text: '当前毛利率' }),
          el(document, 'th', { text: '回本所需杯数' }),
          el(document, 'th', { attrs: { 'data-role': 'cups-head' }, text: '80% 预计可打' }),
          el(document, 'th', { text: '达标状态' }),
        ]),
      ]),
      el(document, 'tbody', {}, [
        makeRow(150, 2500, '¥ 23', '达标', 'verdict-pill--success'),
        makeRow(330, 5000, '¥ 50', '达标', 'verdict-pill--success'),
        makeRow(500, 7500, '¥ 75', '达标', 'verdict-pill--success'),
      ]),
    ]),
  ]);

  document.body.append(section);
  runScript(document);

  const marginSlider = document.getElementById('keg-detail-pricing-margin');
  assert.ok(marginSlider);
  marginSlider.value = '60';
  marginSlider.dispatchEvent({ type: 'input', target: marginSlider });

  assert.equal(section.querySelector('[data-role="rec-head"]').textContent, '60% 推荐售价');
  assert.match(section.querySelector('[data-role="pricing-subtitle"]').textContent, /以 60% 毛利率/);

  const row330 = section.querySelector('tbody tr[data-ml="330"]');
  assert.equal(row330.querySelector('[data-role="rec-price"]').textContent, '¥ 62');
  const pill330 = row330.querySelector('[data-role="status"] .verdict-pill');
  assert.equal(pill330.textContent, '差 ¥ 12');
  assert.ok(pill330.classList.contains('verdict-pill--warn'));
  assert.ok(row330.querySelector('[data-role="margin"]').classList.contains('text-danger'));
}

// 场景 6：系统设置下发自定义默认比例 (85% / 55%) 初始化与恢复默认
{
  const { document } = createFakeDOM();
  const capEl = el(document, 'input', { id: 'capacity_ml', value: '20000' });
  const costEl = el(document, 'input', { id: 'cost', value: '1200' });
  const guide = el(document, 'div', {
    id: 'keg-pricing-guide',
    attrs: { 'data-default-sellable': '85', 'data-default-margin': '55' },
  });
  document.body.append(capEl, costEl, guide);

  runScript(document);

  const sellSlider = document.getElementById('keg-pricing-sellable');
  const marginSlider = document.getElementById('keg-pricing-margin');
  const resetBtn = guide.querySelector('.keg-pricing-reset');
  const output = guide.querySelector('.keg-pricing-output');
  assert.equal(sellSlider.value, '85');
  assert.equal(marginSlider.value, '55');
  assert.equal(resetBtn.hidden, true);
  assert.equal(resetBtn.textContent, '恢复默认 (85% / 55%)');
  assert.match(output.textContent, /17000ml（预留损耗 3000ml）/);
  assert.match(output.textContent, /¥52/);

  sellSlider.value = '90';
  sellSlider.dispatchEvent({ type: 'input', target: sellSlider });
  assert.equal(resetBtn.hidden, false);

  resetBtn.click();
  assert.equal(sellSlider.value, '85');
  assert.equal(marginSlider.value, '55');
  assert.equal(resetBtn.hidden, true);
}

console.log('web/static/js/keg-pricing.js: 6 组可调滑块交互回归通过');
