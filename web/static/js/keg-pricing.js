(() => {
  const DEFAULT_SELLABLE_PCT = 80;
  const DEFAULT_MARGIN_PCT = 50;
  const SELLABLE_MIN = 50;
  const SELLABLE_MAX = 100;
  const MARGIN_MIN = 10;
  const MARGIN_MAX = 85;

  const clampInt = (raw, min, max, fallback) => {
    const n = Math.round(Number(raw));
    if (!Number.isFinite(n)) return fallback;
    return Math.min(max, Math.max(min, n));
  };

  const parseCents = raw => {
    const cleaned = String(raw || '').replace(/[¥￥,\s]/g, '');
    if (!cleaned) return 0;
    const val = Number(cleaned);
    if (!Number.isFinite(val) || val < 0) return -1;
    return Math.round(val * 100);
  };

  const fmtYuan = cents => {
    if (cents % 100 === 0) return '¥' + Math.round(cents / 100);
    return '¥' + (cents / 100).toFixed(2).replace(/0$/, '');
  };

  const fmtPriceText = cents => {
    if (cents % 100 === 0) return '¥ ' + Math.round(cents / 100);
    return '¥ ' + (cents / 100).toFixed(2);
  };

  const flash = el => {
    if (!el) return;
    el.classList.add('is-autofilled');
    window.setTimeout(() => el.classList.remove('is-autofilled'), 1500);
  };

  // 与后端 server/keg_economics.go 在默认 80% / 50% 时保持完全一致的整数运算与整元向上取整。
  const calcPricing = (capML, costCents, sellablePct, marginPct, cupRows) => {
    const sPct = clampInt(sellablePct, SELLABLE_MIN, SELLABLE_MAX, DEFAULT_SELLABLE_PCT);
    const mPct = clampInt(marginPct, MARGIN_MIN, MARGIN_MAX, DEFAULT_MARGIN_PCT);
    if (capML <= 0 || costCents <= 0) {
      return { ready: false, capML, costCents, sellablePct: sPct, marginPct: mPct };
    }

    const sellableML = Math.floor((capML * sPct + 50) / 100);
    const lossML = Math.max(capML - sellableML, 0);
    const costRatio = 100 - mPct;
    const targetRevCents = Math.floor((costCents * 100 + costRatio - 1) / costRatio);
    const theoBreakEvenML = Math.floor((sellableML * costRatio) / 100);
    const denom = sellableML * costRatio;

    const cups = cupRows.map(spec => {
      const ml = spec.ml;
      const actualCents = typeof spec.actualCents === 'number'
        ? spec.actualCents
        : (spec.input ? parseCents(spec.input.value) : 0);
      const effCostCents = sellableML > 0
        ? Math.floor((costCents * ml + Math.floor(sellableML / 2)) / sellableML)
        : 0;
      const recYuan = denom > 0 ? Math.floor((costCents * ml + denom - 1) / denom) : 0;
      const recCents = recYuan * 100;
      const refCents = actualCents > 0 ? actualCents : recCents;
      const beCups = refCents > 0 ? Math.floor((costCents + refCents - 1) / refCents) : 0;
      const sellCups = ml > 0 ? Math.floor(sellableML / ml) : 0;

      let marginText = '未定价';
      let state = 'empty';
      if (actualCents > 0) {
        const margin = ((actualCents - effCostCents) * 100) / actualCents;
        marginText = margin.toFixed(1) + '%';
        state = actualCents >= recCents ? 'ok' : 'low';
      }

      return {
        ...spec,
        effCostCents,
        recYuan,
        recCents,
        actualCents,
        beCups,
        sellCups,
        marginText,
        state,
      };
    });

    return {
      ready: true,
      capML,
      sellablePct: sPct,
      marginPct: mPct,
      sellableML,
      lossML,
      costCents,
      targetRevCents,
      theoBreakEvenML,
      cups,
    };
  };

  const createSliderControls = ({
    idPrefix,
    inline = false,
    defaultSellable = DEFAULT_SELLABLE_PCT,
    defaultMargin = DEFAULT_MARGIN_PCT,
    onChange,
  }) => {
    const defS = clampInt(defaultSellable, SELLABLE_MIN, SELLABLE_MAX, DEFAULT_SELLABLE_PCT);
    const defM = clampInt(defaultMargin, MARGIN_MIN, MARGIN_MAX, DEFAULT_MARGIN_PCT);
    const el = document.createElement('div');
    el.className = 'keg-pricing-controls' + (inline ? ' keg-pricing-controls--inline' : '');

    const makeGroup = ({ id, labelText, min, max, value, scaleLabels }) => {
      const group = document.createElement('div');
      group.className = 'keg-pricing-slider-group';

      const head = document.createElement('div');
      head.className = 'keg-pricing-slider-head';

      const label = document.createElement('label');
      label.className = 'keg-pricing-slider-label';
      label.setAttribute('for', id);
      label.textContent = labelText;

      const readout = document.createElement('strong');
      readout.className = 'keg-pricing-slider-val';

      head.append(label, readout);

      const input = document.createElement('input');
      input.type = 'range';
      input.className = 'keg-pricing-range';
      input.id = id;
      input.min = String(min);
      input.max = String(max);
      input.step = '1';
      input.value = String(value);

      const scale = document.createElement('div');
      scale.className = 'keg-pricing-slider-scale';
      scaleLabels.forEach(text => {
        const span = document.createElement('span');
        span.textContent = text;
        scale.appendChild(span);
      });

      group.append(head, input, scale);
      return { group, input, readout };
    };

    const sellable = makeGroup({
      id: idPrefix + '-sellable',
      labelText: '预计可售比例',
      min: SELLABLE_MIN,
      max: SELLABLE_MAX,
      value: defS,
      scaleLabels: [`${SELLABLE_MIN}%`, `默认 ${defS}%`, `${SELLABLE_MAX}%`],
    });
    sellable.readout.dataset.role = 'sellable-val';

    const margin = makeGroup({
      id: idPrefix + '-margin',
      labelText: '目标毛利率',
      min: MARGIN_MIN,
      max: MARGIN_MAX,
      value: defM,
      scaleLabels: [`${MARGIN_MIN}%`, `默认 ${defM}%`, `${MARGIN_MAX}%`],
    });
    margin.readout.dataset.role = 'margin-val';

    const foot = document.createElement('div');
    foot.className = 'keg-pricing-controls-foot';
    const hint = document.createElement('span');
    hint.className = 'keg-pricing-controls-hint';
    hint.textContent = '拖动滑块实时试算不同出酒率与毛利率下的单杯定价';

    const resetBtn = document.createElement('button');
    resetBtn.type = 'button';
    resetBtn.className = 'lookup-switch-btn keg-pricing-reset';
    resetBtn.textContent = `恢复默认 (${defS}% / ${defM}%)`;
    resetBtn.hidden = true;

    foot.append(hint, resetBtn);
    el.append(sellable.group, margin.group, foot);

    const getValues = () => ({
      sellablePct: clampInt(sellable.input.value, SELLABLE_MIN, SELLABLE_MAX, defS),
      marginPct: clampInt(margin.input.value, MARGIN_MIN, MARGIN_MAX, defM),
    });

    const syncDisplay = capML => {
      const { sellablePct, marginPct } = getValues();
      if (capML > 0) {
        const sml = Math.floor((capML * sellablePct + 50) / 100);
        sellable.readout.textContent = `${sml}ml（${sellablePct}%）`;
      } else {
        sellable.readout.textContent = `${sellablePct}%`;
      }
      margin.readout.textContent = `${marginPct}%`;
      resetBtn.hidden = sellablePct === defS && marginPct === defM;
    };

    const handleChange = () => {
      if (typeof onChange === 'function') onChange();
    };
    [sellable.input, margin.input].forEach(input => {
      input.addEventListener('input', handleChange);
      input.addEventListener('change', handleChange);
    });
    resetBtn.addEventListener('click', () => {
      sellable.input.value = String(defS);
      margin.input.value = String(defM);
      handleChange();
    });

    return { el, getValues, syncDisplay, sellableInput: sellable.input, marginInput: margin.input, resetBtn };
  };

  const initFormGuide = () => {
    const guide = document.getElementById('keg-pricing-guide');
    const capEl = document.getElementById('capacity_ml');
    const costEl = document.getElementById('cost');
    if (!guide || !capEl || !costEl) return;

    const cupSpecs = [
      { ml: 150, input: document.getElementById('price_150') },
      { ml: 330, input: document.getElementById('price_330') },
      { ml: 500, input: document.getElementById('price_500') },
    ];

    const output = document.createElement('div');
    output.className = 'keg-pricing-output';

    let controls = null;
    const applyPrices = items => {
      items.forEach(({ input, recYuan }) => {
        if (!input || recYuan <= 0) return;
        input.value = String(recYuan);
        flash(input);
      });
      render();
    };

    const render = () => {
      const capML = parseInt(String(capEl.value || '').trim(), 10) || 0;
      const costCents = parseCents(costEl.value);
      const { sellablePct, marginPct } = controls.getValues();
      controls.syncDisplay(capML);

      const data = calcPricing(capML, costCents, sellablePct, marginPct, cupSpecs);
      output.replaceChildren();

      if (!data.ready) {
        const empty = document.createElement('div');
        empty.className = 'keg-pricing-empty';
        const icon = document.createElement('i');
        icon.className = 'bi bi-calculator';
        icon.setAttribute('aria-hidden', 'true');
        const txt = document.createElement('span');
        txt.textContent = data.costCents < 0
          ? '每桶进价格式不正确，请输入非负数字'
          : `填入桶容量与每桶进价后，将按当前 ${data.sellablePct}% 可售量与 ${data.marginPct}% 目标毛利率自动测算回本线和推荐杯价。`;
        empty.append(icon, txt);
        output.appendChild(empty);
        return;
      }

      const head = document.createElement('div');
      head.className = 'keg-pricing-head';

      const meta = document.createElement('div');
      meta.className = 'keg-pricing-meta';
      const items = [
        [`${data.sellablePct}% 可售容量`, `${data.sellableML}ml（预留损耗 ${data.lossML}ml）`],
        ['金额回本线', `${fmtYuan(data.costCents)}（${data.marginPct}% 价约售 ${data.theoBreakEvenML}ml）`],
        [`${data.marginPct}% 毛利目标营收`, fmtYuan(data.targetRevCents)],
      ];
      items.forEach(([k, v]) => {
        const pill = document.createElement('span');
        pill.className = 'keg-pricing-pill';
        const label = document.createElement('small');
        label.textContent = k;
        const val = document.createElement('strong');
        val.textContent = v;
        pill.append(label, val);
        meta.appendChild(pill);
      });

      const applyAll = document.createElement('button');
      applyAll.type = 'button';
      applyAll.className = 'button button--secondary button--sm';
      const magicIcon = document.createElement('i');
      magicIcon.className = 'bi bi-magic';
      magicIcon.setAttribute('aria-hidden', 'true');
      applyAll.append(magicIcon, document.createTextNode(`一键套用 ${data.marginPct}% 推荐价`));
      applyAll.addEventListener('click', () => applyPrices(data.cups));

      head.append(meta, applyAll);

      const grid = document.createElement('div');
      grid.className = 'keg-pricing-cups';

      data.cups.forEach(cup => {
        const card = document.createElement('div');
        card.className = 'keg-pricing-cup';

        const top = document.createElement('div');
        top.className = 'keg-pricing-cup-top';
        const title = document.createElement('strong');
        title.textContent = cup.ml + 'ml';
        const chip = document.createElement('span');
        chip.className = 'keg-margin-chip is-' + cup.state;
        if (cup.state === 'ok') chip.textContent = '达标 ' + cup.marginText;
        else if (cup.state === 'low') chip.textContent = '偏低 ' + cup.marginText;
        else chip.textContent = '未定价';
        top.append(title, chip);

        const kv = document.createElement('div');
        kv.className = 'keg-pricing-cup-rows';
        const rowData = [
          [`${data.sellablePct}% 有效成本`, fmtYuan(cup.effCostCents)],
          [`${data.marginPct}% 推荐售价`, '¥' + cup.recYuan],
          ['回本 / 可打杯数', `${cup.beCups} / ${cup.sellCups} 杯`],
        ];
        rowData.forEach(([k, v], idx) => {
          const line = document.createElement('div');
          line.className = 'keg-pricing-cup-row';
          const l = document.createElement('span');
          l.textContent = k;
          const r = document.createElement(idx === 1 ? 'strong' : 'span');
          r.textContent = v;
          line.append(l, r);
          kv.appendChild(line);
        });

        const foot = document.createElement('div');
        foot.className = 'keg-pricing-cup-foot';
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'lookup-switch-btn';
        btn.textContent = '采用 ¥' + cup.recYuan;
        btn.addEventListener('click', () => applyPrices([cup]));
        foot.appendChild(btn);

        card.append(top, kv, foot);
        grid.appendChild(card);
      });

      output.append(head, grid);
    };

    const defaultSellable = clampInt(guide.dataset.defaultSellable || DEFAULT_SELLABLE_PCT, SELLABLE_MIN, SELLABLE_MAX, DEFAULT_SELLABLE_PCT);
    const defaultMargin = clampInt(guide.dataset.defaultMargin || DEFAULT_MARGIN_PCT, MARGIN_MIN, MARGIN_MAX, DEFAULT_MARGIN_PCT);
    controls = createSliderControls({
      idPrefix: 'keg-pricing',
      inline: false,
      defaultSellable,
      defaultMargin,
      onChange: render,
    });
    guide.replaceChildren(controls.el, output);

    [capEl, costEl, ...cupSpecs.map(c => c.input).filter(Boolean)].forEach(el => {
      el.addEventListener('input', render);
      el.addEventListener('change', render);
    });
    document.addEventListener('keg:pricing-sync', render);

    render();
  };

  const initDetailTable = () => {
    const section = document.getElementById('keg-detail-pricing');
    const controlsHost = document.getElementById('keg-detail-pricing-controls');
    if (!section || !controlsHost) return;

    const capML = parseInt(section.dataset.capacityMl || '', 10) || 0;
    const costCents = parseInt(section.dataset.costCents || '', 10) || 0;
    if (capML <= 0 || costCents <= 0) return;

    const subtitleEl = section.querySelector('[data-role="pricing-subtitle"]');
    const costHeadEl = section.querySelector('[data-role="cost-head"]');
    const recHeadEl = section.querySelector('[data-role="rec-head"]');
    const cupsHeadEl = section.querySelector('[data-role="cups-head"]');

    const rows = Array.from(section.querySelectorAll('tbody tr[data-ml]')).map(tr => ({
      ml: parseInt(tr.dataset.ml || '', 10) || 0,
      actualCents: parseInt(tr.dataset.actualCents || '', 10) || 0,
      effCostEl: tr.querySelector('[data-role="eff-cost"]'),
      recPriceEl: tr.querySelector('[data-role="rec-price"]'),
      marginEl: tr.querySelector('[data-role="margin"]'),
      breakevenEl: tr.querySelector('[data-role="breakeven"]'),
      sellCupsEl: tr.querySelector('[data-role="sellable-cups"]'),
      statusEl: tr.querySelector('[data-role="status"]'),
    })).filter(r => r.ml > 0);

    if (!rows.length) return;

    let controls = null;
    const renderDetail = () => {
      const { sellablePct, marginPct } = controls.getValues();
      controls.syncDisplay(capML);

      const data = calcPricing(capML, costCents, sellablePct, marginPct, rows);
      if (!data.ready) return;

      if (subtitleEl) {
        subtitleEl.textContent = `按整桶 ${data.sellablePct}% 可售（${data.sellableML}ml / ${capML}ml）摊算单杯成本，以 ${data.marginPct}% 毛利率向上取整到整元`;
      }
      if (costHeadEl) costHeadEl.textContent = `${data.sellablePct}% 有效成本`;
      if (recHeadEl) recHeadEl.textContent = `${data.marginPct}% 推荐售价`;
      if (cupsHeadEl) cupsHeadEl.textContent = `${data.sellablePct}% 预计可打`;

      data.cups.forEach(cup => {
        if (cup.effCostEl) {
          cup.effCostEl.textContent = cup.effCostCents > 0 ? fmtPriceText(cup.effCostCents) : '—';
        }
        if (cup.recPriceEl) {
          const target = cup.recPriceEl.querySelector('strong') || cup.recPriceEl;
          target.textContent = cup.recCents > 0 ? fmtPriceText(cup.recCents) : '—';
        }
        if (cup.marginEl) {
          if (cup.actualCents > 0) {
            cup.marginEl.textContent = cup.marginText;
            cup.marginEl.className = 'num ' + (cup.state === 'ok' ? 'text-success' : 'text-danger');
          } else {
            cup.marginEl.textContent = '—';
            cup.marginEl.className = 'num text-muted';
          }
        }
        if (cup.breakevenEl) {
          cup.breakevenEl.textContent = cup.beCups > 0 ? cup.beCups + ' 杯' : '—';
        }
        if (cup.sellCupsEl) {
          cup.sellCupsEl.textContent = cup.sellCups > 0 ? cup.sellCups + ' 杯' : '—';
        }
        if (cup.statusEl) {
          const pill = cup.statusEl.querySelector('.verdict-pill') || cup.statusEl;
          if (cup.actualCents <= 0) {
            pill.className = 'verdict-pill verdict-pill--muted';
            pill.textContent = '待定价';
          } else if (cup.actualCents >= cup.recCents) {
            pill.className = 'verdict-pill verdict-pill--success';
            pill.textContent = '达标';
          } else {
            pill.className = 'verdict-pill verdict-pill--warn';
            pill.textContent = '差 ' + fmtPriceText(cup.recCents - cup.actualCents);
          }
        }
      });
    };

    const defaultSellable = clampInt(section.dataset.defaultSellable || DEFAULT_SELLABLE_PCT, SELLABLE_MIN, SELLABLE_MAX, DEFAULT_SELLABLE_PCT);
    const defaultMargin = clampInt(section.dataset.defaultMargin || DEFAULT_MARGIN_PCT, MARGIN_MIN, MARGIN_MAX, DEFAULT_MARGIN_PCT);
    controls = createSliderControls({
      idPrefix: 'keg-detail-pricing',
      inline: true,
      defaultSellable,
      defaultMargin,
      onChange: renderDetail,
    });
    controlsHost.replaceChildren(controls.el);
    renderDetail();
  };

  initFormGuide();
  initDetailTable();
})();
