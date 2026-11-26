(() => {
  const money = c => '¥ ' + (c % 100 ? (c / 100).toFixed(2) : c / 100);
  const newIdemKey = () => 'pos-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10);

  function buildMemberMeta(m) {
    if (!m) return '';
    const parts = [];
    if (m.tier_name) {
      parts.push(m.tier_discount_text && m.tier_discount_text !== '无折扣'
        ? `${m.tier_name} (${m.tier_discount_text})`
        : m.tier_name);
    }
    parts.push(`余额 ${m.balance_text || money(m.balance_cents || 0)}`);
    parts.push(`${m.points || 0} 积分`);
    if (m.active_pass) {
      const p = m.active_pass;
      if (p.benefit_type === 'discount') {
        parts.push(`${p.plan_name} (${p.discount_text})`);
      } else {
        parts.push(`${p.plan_name} · 今日剩 ${p.remaining_today} 杯`);
      }
    }
    return parts.join(' · ');
  }

  function buildPreviewSummary(pricing, method) {
    if (!pricing) return [];
    const tags = [];
    if (pricing.discount_label) {
      tags.push(pricing.discount_label);
    }
    if (pricing.discount_cents > 0) {
      tags.push(`共省 ${money(pricing.discount_cents)}`);
    }
    if (pricing.balance_paid_cents > 0 && pricing.external_paid_cents > 0) {
      const extName = method === 'cash' ? '现金' : (method === 'card' ? '刷卡' : (method === 'other' ? '其他' : '扫码'));
      tags.push(`储值扣 ${money(pricing.balance_paid_cents)} + ${extName}补差 ${money(pricing.external_paid_cents)}`);
    } else if (pricing.balance_paid_cents > 0) {
      tags.push(`储值扣款 ${money(pricing.balance_paid_cents)}`);
    }
    if (pricing.points_earned > 0) {
      tags.push(`返 ${pricing.points_earned} 积分`);
    }
    return tags;
  }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { money, buildMemberMeta, buildPreviewSummary };
  }
  if (typeof document === 'undefined') return;

  function init() {
    const form = document.getElementById('pay-form');
    const side = document.querySelector('.side[data-order-id]');
    const orderID = side ? side.dataset.orderId : '';
    if (!form || !orderID) return;

    const memberIdInput = document.getElementById('member-id-input');
    const passIdInput = document.getElementById('pass-id-input');
    const ruleIdInput = document.getElementById('discount-rule-input');
    const searchInput = document.getElementById('member-search-input');
    const searchMenu = document.getElementById('member-search-results');
    const memberBoundBox = document.getElementById('member-bound-box');
    const memberSearchBox = document.getElementById('member-search-box');
    const boundNameEl = document.getElementById('member-bound-name');
    const boundMetaEl = document.getElementById('member-bound-meta');
    const unbindBtn = document.getElementById('member-unbind-btn');

    const passRow = document.getElementById('loyalty-pass-row');
    const usePassCheck = document.getElementById('use-pass-check');
    const passMetaEl = document.getElementById('loyalty-pass-meta');
    const pointsRow = document.getElementById('loyalty-points-row');
    const usePointsCheck = document.getElementById('use-points-check');
    const pointsMetaEl = document.getElementById('loyalty-points-meta');
    const balanceRow = document.getElementById('loyalty-balance-row');
    const useBalanceCheck = document.getElementById('use-balance-check');
    const balanceMetaEl = document.getElementById('loyalty-balance-meta');

    const customWrap = document.getElementById('custom-discount-wrap');
    const customKind = document.getElementById('custom-discount-kind');
    const customValue = document.getElementById('custom-discount-value');
    const customScope = document.getElementById('custom-discount-scope');
    const discountNote = document.getElementById('discount-note');

    const togglePanelBtn = document.getElementById('toggle-loyalty-panel');
    const loyaltyPanel = document.getElementById('loyalty-panel');

    const selTotal = document.getElementById('sel-total');
    const selGross = document.getElementById('sel-gross');
    const previewBadges = document.getElementById('preview-badges');
    const previewErr = document.getElementById('preview-err');
    const expInput = document.getElementById('expected-total-cents');
    const payBtn = document.getElementById('pay-btn');

    let currentMember = null;
    try {
      const seedEl = document.getElementById('bound-member-json');
      if (seedEl && seedEl.textContent.trim()) {
        currentMember = JSON.parse(seedEl.textContent);
      }
    } catch (e) {}

    let previewSeq = 0;
    let previewCtrl = null;
    let previewTimer = null;
    let searchSeq = 0;
    let searchCtrl = null;
    let searchTimer = null;

    function selectedMethod() {
      const checked = form.querySelector('input[name="method"]:checked');
      return checked ? checked.value : 'qr';
    }

    function syncMemberUI(autoCheckPass) {
      const hasMember = Boolean(currentMember && currentMember.id);
      if (memberIdInput) memberIdInput.value = hasMember ? String(currentMember.id) : '';
      if (passIdInput) passIdInput.value = (hasMember && currentMember.active_pass) ? String(currentMember.active_pass.id) : '';
      if (memberBoundBox) memberBoundBox.hidden = !hasMember;
      if (memberSearchBox) memberSearchBox.hidden = hasMember;

      if (hasMember) {
        if (boundNameEl) boundNameEl.textContent = `${currentMember.name} · ${currentMember.phone}`;
        if (boundMetaEl) boundMetaEl.textContent = buildMemberMeta(currentMember);

        const pass = currentMember.active_pass;
        if (passRow && usePassCheck) {
          const usable = Boolean(pass && (pass.remaining_today > 0 || pass.benefit_type === 'discount' || pass.benefit_type === 'hybrid'));
          passRow.hidden = !pass;
          usePassCheck.disabled = !usable;
          if (autoCheckPass) usePassCheck.checked = usable;
          if (!usable) usePassCheck.checked = false;
          if (passMetaEl && pass) {
            passMetaEl.textContent = pass.benefit_type === 'discount'
              ? `${pass.plan_name}（${pass.discount_text}）`
              : `${pass.plan_name} · 今日剩 ${pass.remaining_today} 杯`;
          }
        }
        if (pointsRow && usePointsCheck) {
          const pts = currentMember.points || 0;
          pointsRow.hidden = pts <= 0;
          if (pts <= 0) usePointsCheck.checked = false;
          if (pointsMetaEl) pointsMetaEl.textContent = `可用 ${pts} 积分`;
        }
        if (balanceRow && useBalanceCheck) {
          const bal = currentMember.balance_cents || 0;
          balanceRow.hidden = bal <= 0;
          if (bal <= 0) useBalanceCheck.checked = false;
          if (balanceMetaEl) balanceMetaEl.textContent = `当前余额 ${money(bal)}`;
        }
      } else {
        if (passRow) passRow.hidden = true;
        if (usePassCheck) usePassCheck.checked = false;
        if (pointsRow) pointsRow.hidden = true;
        if (usePointsCheck) usePointsCheck.checked = false;
        if (balanceRow) balanceRow.hidden = true;
        if (useBalanceCheck) useBalanceCheck.checked = false;
        if (selectedMethod() === 'balance') {
          const qrRadio = form.querySelector('input[name="method"][value="qr"]');
          if (qrRadio) qrRadio.checked = true;
        }
      }

      const balRadio = form.querySelector('input[name="method"][value="balance"]');
      if (balRadio) balRadio.disabled = !hasMember;
    }

    function renderPreview(pricing) {
      if (!pricing) return;
      if (previewErr) {
        previewErr.hidden = true;
        previewErr.textContent = '';
      }
      if (selTotal) selTotal.textContent = money(pricing.amount_cents);
      if (expInput) expInput.value = String(pricing.amount_cents);
      if (selGross) {
        if (pricing.discount_cents > 0) {
          selGross.hidden = false;
          selGross.textContent = money(pricing.gross_cents);
        } else {
          selGross.hidden = true;
          selGross.textContent = '';
        }
      }
      if (previewBadges) {
        previewBadges.replaceChildren();
        buildPreviewSummary(pricing, selectedMethod()).forEach(text => {
          const span = document.createElement('span');
          span.className = 'preview-chip';
          span.textContent = text;
          previewBadges.appendChild(span);
        });
      }
      if (payBtn) {
        payBtn.disabled = false;
        payBtn.textContent = pricing.amount_cents === 0
          ? '确认 0 元核销'
          : '结账所选 · ' + money(pricing.amount_cents);
      }
    }

    function renderPreviewError(msg) {
      if (previewErr) {
        previewErr.hidden = !msg;
        previewErr.textContent = msg || '';
      }
      if (payBtn && msg) payBtn.disabled = true;
    }

    async function runPreview() {
      const checkedBoxes = Array.from(form.querySelectorAll('input[name="items"]:checked'));
      if (checkedBoxes.length === 0) {
        if (previewCtrl) previewCtrl.abort();
        if (selGross) selGross.hidden = true;
        if (previewBadges) previewBadges.replaceChildren();
        renderPreviewError('');
        return;
      }

      const seq = ++previewSeq;
      if (previewCtrl) previewCtrl.abort();
      previewCtrl = typeof AbortController !== 'undefined' ? new AbortController() : null;

      const body = new URLSearchParams();
      checkedBoxes.forEach(b => body.append('items', b.value));
      body.set('member_id', memberIdInput ? memberIdInput.value : '');
      if (passIdInput && passIdInput.value) body.set('pass_id', passIdInput.value);
      if (usePassCheck && usePassCheck.checked) body.set('use_pass', '1');
      if (usePointsCheck && usePointsCheck.checked) body.set('use_points', '1');
      if (useBalanceCheck && useBalanceCheck.checked) body.set('use_balance', '1');
      if (ruleIdInput && ruleIdInput.value) body.set('discount_rule_id', ruleIdInput.value);
      if (customKind && customKind.value) body.set('custom_discount_kind', customKind.value);
      if (customValue && customValue.value) body.set('custom_discount_value', customValue.value);
      if (customScope && customScope.value) body.set('custom_discount_scope', customScope.value);
      if (discountNote && discountNote.value) body.set('discount_note', discountNote.value);
      body.set('method', selectedMethod());

      try {
        const resp = await fetch(`/pos/order/${orderID}/preview`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Requested-With': 'fetch' },
          body: body.toString(),
          signal: previewCtrl ? previewCtrl.signal : undefined,
        });
        const data = await resp.json();
        if (seq !== previewSeq) return;
        if (!resp.ok) {
          renderPreviewError(data.error || '算价失败，请核对优惠设置');
          return;
        }
        if (data.member) {
          currentMember = data.member;
          syncMemberUI(false);
        }
        renderPreview(data.pricing);
      } catch (err) {
        if (err && err.name === 'AbortError') return;
      }
    }

    function schedulePreview(delay = 50) {
      if (previewTimer) clearTimeout(previewTimer);
      previewTimer = setTimeout(runPreview, delay);
    }

    // 会员检索与绑定
    function closeSearchMenu() {
      if (searchMenu) {
        searchMenu.hidden = true;
        searchMenu.replaceChildren();
      }
    }

    async function bindMember(memberID) {
      const body = new URLSearchParams();
      body.set('member_id', memberID ? String(memberID) : '0');
      const resp = await fetch(`/pos/order/${orderID}/member`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/x-www-form-urlencoded',
          'X-Requested-With': 'fetch',
          'Accept': 'application/json',
        },
        body: body.toString(),
      });
      const data = await resp.json();
      if (!resp.ok) {
        renderPreviewError(data.error || '绑定会员失败');
        return;
      }
      currentMember = data.member || null;
      syncMemberUI(true);
      closeSearchMenu();
      if (searchInput) searchInput.value = '';
      schedulePreview(0);
    }

    function renderSearchResults(list) {
      if (!searchMenu) return;
      searchMenu.replaceChildren();
      if (!list || list.length === 0) {
        const empty = document.createElement('div');
        empty.className = 'member-result-empty';
        empty.textContent = '没有匹配的会员，点右侧「+ 建档/办卡」快速开通';
        searchMenu.appendChild(empty);
        searchMenu.hidden = false;
        return;
      }
      list.forEach(m => {
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'member-result-item';
        const title = document.createElement('strong');
        title.textContent = `${m.name} · ${m.phone}`;
        const sub = document.createElement('span');
        sub.textContent = buildMemberMeta(m);
        btn.append(title, sub);
        btn.addEventListener('click', () => bindMember(m.id));
        searchMenu.appendChild(btn);
      });
      searchMenu.hidden = false;
    }

    if (searchInput) {
      searchInput.addEventListener('input', () => {
        const q = searchInput.value.trim();
        if (searchTimer) clearTimeout(searchTimer);
        if (!q) {
          closeSearchMenu();
          return;
        }
        searchTimer = setTimeout(async () => {
          const seq = ++searchSeq;
          if (searchCtrl) searchCtrl.abort();
          searchCtrl = typeof AbortController !== 'undefined' ? new AbortController() : null;
          try {
            const resp = await fetch(`/pos/members/search?q=${encodeURIComponent(q)}`, {
              signal: searchCtrl ? searchCtrl.signal : undefined,
            });
            const data = await resp.json();
            if (seq !== searchSeq) return;
            renderSearchResults(data.members || []);
          } catch (err) {
            if (err && err.name === 'AbortError') return;
          }
        }, 180);
      });
      searchInput.addEventListener('keydown', ev => {
        if (ev.key === 'Escape') closeSearchMenu();
      });
    }

    if (unbindBtn) {
      unbindBtn.addEventListener('click', () => bindMember(0));
    }

    // 优惠面板折叠
    if (togglePanelBtn && loyaltyPanel) {
      togglePanelBtn.addEventListener('click', () => {
        const open = loyaltyPanel.hidden;
        loyaltyPanel.hidden = !open;
        togglePanelBtn.setAttribute('aria-expanded', String(open));
      });
    }

    // 预设折扣胶囊与自定义切换
    form.querySelectorAll('[data-rule-pick]').forEach(btn => {
      btn.addEventListener('click', () => {
        const rid = btn.dataset.rulePick || '';
        form.querySelectorAll('[data-rule-pick]').forEach(b => {
          b.classList.toggle('is-active', b === btn);
          b.setAttribute('aria-pressed', String(b === btn));
        });
        if (rid === 'custom') {
          if (ruleIdInput) ruleIdInput.value = '';
          if (customWrap) customWrap.hidden = false;
          if (customKind && !customKind.value) customKind.value = 'percent';
        } else {
          if (ruleIdInput) ruleIdInput.value = rid;
          if (customWrap) customWrap.hidden = true;
          if (customKind) customKind.value = '';
          if (customValue) customValue.value = '';
        }
        schedulePreview(0);
      });
    });

    [usePassCheck, usePointsCheck, useBalanceCheck, customKind, customScope].forEach(el => {
      if (el) el.addEventListener('change', () => schedulePreview(0));
    });
    [customValue, discountNote].forEach(el => {
      if (el) el.addEventListener('input', () => schedulePreview(160));
    });
    form.querySelectorAll('input[name="method"]').forEach(el => {
      el.addEventListener('change', () => {
        if (el.value === 'balance' && useBalanceCheck) {
          useBalanceCheck.checked = false;
        }
        schedulePreview(0);
      });
    });

    // 快捷建档 / 充值 / 办月卡 Modal
    const modal = document.getElementById('member-modal');
    if (modal) {
      const modalTitle = document.getElementById('member-modal-title');
      const modalErr = document.getElementById('member-modal-err');
      const modalForm = document.getElementById('member-modal-form');
      const modalModeInput = document.getElementById('modal-mode-input');
      const modalIdemInput = document.getElementById('modal-idem-input');
      const quickFields = document.getElementById('modal-quick-fields');
      const rechargeFields = document.getElementById('modal-recharge-fields');
      const passFields = document.getElementById('modal-pass-fields');
      const payAmtInput = document.getElementById('modal-pay-amount');
      const bonusAmtInput = document.getElementById('modal-bonus-amount');
      const passPlanSelect = document.getElementById('modal-pass-plan');
      const passMethodSelect = document.getElementById('modal-pass-method');
      const modalSubmitBtn = document.getElementById('member-modal-submit');

      function openModal(mode) {
        if (modalErr) {
          modalErr.hidden = true;
          modalErr.textContent = '';
        }
        if (modalModeInput) modalModeInput.value = mode;
        if (modalIdemInput) modalIdemInput.value = newIdemKey();
        if (quickFields) quickFields.hidden = mode !== 'quick';
        if (rechargeFields) rechargeFields.hidden = mode === 'pass';
        if (passFields) passFields.hidden = mode === 'recharge';

        const balOpt = passMethodSelect ? passMethodSelect.querySelector('option[value="balance"]') : null;
        if (balOpt) balOpt.disabled = mode === 'quick' || !currentMember;

        if (mode === 'quick') {
          if (modalTitle) modalTitle.textContent = '快捷会员建档 / 开卡';
          if (modalSubmitBtn) modalSubmitBtn.textContent = '创建并绑定本单';
          if (payAmtInput) payAmtInput.value = '';
          if (bonusAmtInput) bonusAmtInput.value = '';
          if (passPlanSelect) passPlanSelect.value = '';
        } else if (mode === 'recharge') {
          if (!currentMember) return;
          if (modalTitle) modalTitle.textContent = `储值充值 · ${currentMember.name}`;
          if (modalSubmitBtn) modalSubmitBtn.textContent = '确认充值';
          if (payAmtInput) payAmtInput.value = '';
          if (bonusAmtInput) bonusAmtInput.value = '';
        } else {
          if (!currentMember) return;
          if (modalTitle) modalTitle.textContent = `办理/续费月卡 · ${currentMember.name}`;
          if (modalSubmitBtn) modalSubmitBtn.textContent = '确认办理月卡';
        }
        if (typeof modal.showModal === 'function') modal.showModal();
        else modal.setAttribute('open', '');
      }

      document.querySelectorAll('[data-open-member-modal]').forEach(btn => {
        btn.addEventListener('click', () => openModal(btn.dataset.openMemberModal));
      });
      document.querySelectorAll('[data-close-member-modal]').forEach(btn => {
        btn.addEventListener('click', () => {
          if (typeof modal.close === 'function') modal.close();
          else modal.removeAttribute('open');
        });
      });
      modal.querySelectorAll('[data-preset-pay]').forEach(btn => {
        btn.addEventListener('click', () => {
          if (payAmtInput) payAmtInput.value = btn.dataset.presetPay || '';
          if (bonusAmtInput) bonusAmtInput.value = btn.dataset.presetBonus || '0';
        });
      });

      if (modalForm) {
        modalForm.addEventListener('submit', async ev => {
          ev.preventDefault();
          const mode = modalModeInput ? modalModeInput.value : 'quick';
          if (modalErr) modalErr.hidden = true;
          const fd = new FormData(modalForm);
          const body = new URLSearchParams();
          let url = '/pos/members/quick';

          if (mode === 'quick') {
            body.set('name', fd.get('name') || '');
            body.set('phone', fd.get('phone') || '');
            body.set('recharge_pay_amount', fd.get('pay_amount') || '');
            body.set('recharge_bonus_amount', fd.get('bonus_amount') || '');
            body.set('recharge_method', fd.get('recharge_method') || 'qr');
            body.set('pass_plan_id', fd.get('pass_plan_id') || '');
            body.set('pass_method', fd.get('pass_method') || 'qr');
            body.set('order_id', orderID);
            body.set('idempotency_key', fd.get('idempotency_key') || '');
          } else if (mode === 'recharge') {
            url = `/pos/members/${currentMember.id}/recharge`;
            body.set('pay_amount', fd.get('pay_amount') || '');
            body.set('bonus_amount', fd.get('bonus_amount') || '');
            body.set('pay_method', fd.get('recharge_method') || 'qr');
            body.set('note', fd.get('note') || '');
            body.set('idempotency_key', fd.get('idempotency_key') || '');
          } else {
            url = `/pos/members/${currentMember.id}/pass`;
            body.set('plan_id', fd.get('pass_plan_id') || '');
            body.set('pay_method', fd.get('pass_method') || 'qr');
            body.set('note', fd.get('note') || '');
            body.set('idempotency_key', fd.get('idempotency_key') || '');
          }

          if (modalSubmitBtn) modalSubmitBtn.disabled = true;
          try {
            const resp = await fetch(url, {
              method: 'POST',
              headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Requested-With': 'fetch' },
              body: body.toString(),
            });
            const data = await resp.json();
            if (!resp.ok) {
              if (modalErr) {
                modalErr.hidden = false;
                modalErr.textContent = data.error || '操作失败，请重试';
              }
              return;
            }
            currentMember = data.member || currentMember;
            syncMemberUI(true);
            modalForm.reset();
            if (typeof modal.close === 'function') modal.close();
            else modal.removeAttribute('open');
            schedulePreview(0);
          } catch (err) {
            if (modalErr) {
              modalErr.hidden = false;
              modalErr.textContent = '网络错误，请重试';
            }
          } finally {
            if (modalSubmitBtn) modalSubmitBtn.disabled = false;
          }
        });
      }
    }

    syncMemberUI(true);
    schedulePreview(0);

    window.PourOpsPosLoyalty = {
      refresh: () => schedulePreview(0),
      getMember: () => currentMember,
    };
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
