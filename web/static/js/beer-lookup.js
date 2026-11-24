(() => {
  const posterInput = document.getElementById('poster_url');
  const posterFileInput = document.getElementById('poster_file');
  const posterImg = document.getElementById('poster-preview-img');
  const posterFb = document.getElementById('poster-preview-fallback');
  const fbTitle = document.getElementById('poster-placeholder-title');
  const fbNote = document.getElementById('poster-placeholder-note');
  let localPreviewUrl = '';

  const revokeLocalPreview = () => {
    if (!localPreviewUrl) return;
    if (window.URL && typeof URL.revokeObjectURL === 'function') {
      URL.revokeObjectURL(localPreviewUrl);
    }
    localPreviewUrl = '';
  };

  const syncPoster = () => {
    if (!posterInput || !posterImg || !posterFb) return;
    revokeLocalPreview();
    if (posterFileInput && posterFileInput.files && posterFileInput.files[0]) {
      if (window.URL && typeof URL.createObjectURL === 'function') {
        localPreviewUrl = URL.createObjectURL(posterFileInput.files[0]);
        posterImg.src = localPreviewUrl;
        posterImg.hidden = false;
        posterFb.hidden = true;
        return;
      }
    }
    const v = posterInput.value.trim();
    if (!v) {
      posterImg.hidden = true;
      posterImg.removeAttribute('src');
      posterFb.hidden = false;
      if (fbTitle) fbTitle.textContent = '暂无酒标';
      if (fbNote) fbNote.textContent = '搜索酒名自动填充、上传图片或粘贴图片地址';
      return;
    }
    posterImg.src = v;
    posterImg.hidden = false;
    posterFb.hidden = true;
  };
  if (posterImg) {
    posterImg.addEventListener('error', () => {
      posterImg.hidden = true;
      posterFb.hidden = false;
      if (fbTitle) fbTitle.textContent = '酒标加载失败';
      if (fbNote) fbNote.textContent = '可重新搜索、上传图片或替换图片地址';
    });
  }
  if (posterInput) {
    posterInput.addEventListener('input', () => {
      if (posterFileInput && posterFileInput.value) posterFileInput.value = '';
      syncPoster();
    });
  }
  if (posterFileInput) {
    posterFileInput.addEventListener('change', syncPoster);
  }

  const wrap = document.getElementById('beer-lookup-wrap');
  const nameInput = document.getElementById('name');
  const searchBtn = document.getElementById('beer-lookup-btn');
  const menu = document.getElementById('beer-lookup-menu');
  const statusEl = document.getElementById('beer-lookup-status');
  if (!wrap || !nameInput || !searchBtn || !menu || !statusEl) return;

  const mode = wrap.dataset.mode || 'purchase';
  const isPurchase = mode === 'purchase';
  const isEdit = mode === 'edit';
  const fieldKeys = {
    brewery: 'brewery',
    name: 'name',
    style: 'style',
    category: 'category',
    abv: 'abv',
    ibu: 'ibu',
    ut_rating: 'ut_rating',
    ut_count: 'ut_count',
    tags: 'tags',
    poster_url: 'poster_url',
    capacity_ml: 'capacity_ml',
    cost: 'cost',
    price_150: 'price_150',
    price_330: 'price_330',
    price_500: 'price_500',
  };
  const fieldEls = {};
  Object.entries(fieldKeys).forEach(([jsonKey, id]) => {
    const el = document.getElementById(id);
    if (!el) return;
    fieldEls[jsonKey] = el;
    const onManual = ev => {
      if (ev && ev.isTrusted === false) return;
      el.dataset.userEdited = '1';
      delete el.dataset.autofilled;
      el.classList.remove('is-autofilled');
    };
    el.addEventListener('input', onManual);
    el.addEventListener('change', onManual);
  });

  let candidates = [];
  let activeIndex = -1;
  let debounceTimer = 0;
  let searchAbort = null;
  let detailAbort = null;

  const closeMenu = () => {
    menu.hidden = true;
    activeIndex = -1;
  };

  const setStatus = (kind, text, opts = {}) => {
    statusEl.className = 'lookup-status' + (kind ? ' is-' + kind : '');
    statusEl.replaceChildren();
    const span = document.createElement('span');
    span.textContent = text;
    statusEl.appendChild(span);
    if (opts.showSwitch && candidates.length > 1) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'lookup-switch-btn';
      btn.textContent = '切换候选 (' + candidates.length + ')';
      btn.addEventListener('click', () => {
        renderMenu(candidates);
        menu.hidden = false;
      });
      statusEl.appendChild(btn);
    }
    statusEl.hidden = false;
  };

  const highlightActive = idx => {
    const items = Array.from(menu.querySelectorAll('.lookup-item'));
    activeIndex = idx;
    items.forEach((el, i) => {
      el.dataset.active = i === activeIndex ? 'true' : 'false';
      if (i === activeIndex) el.scrollIntoView({ block: 'nearest' });
    });
  };

  const renderMenu = list => {
    menu.replaceChildren();
    if (!list.length) {
      const empty = document.createElement('div');
      empty.className = 'lookup-empty';
      empty.textContent = '未找到匹配酒款，请换个关键词或直接手动填写';
      menu.appendChild(empty);
      return;
    }
    list.forEach((item, idx) => {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'lookup-item';
      btn.setAttribute('role', 'option');

      const thumb = document.createElement('span');
      thumb.className = 'lookup-thumb';
      if (item.poster_url) {
        const img = document.createElement('img');
        img.src = item.poster_url;
        img.alt = '';
        img.referrerPolicy = 'no-referrer';
        img.addEventListener('error', () => {
          thumb.innerHTML = '<i class="bi bi-cup-straw" aria-hidden="true"></i>';
        });
        thumb.appendChild(img);
      } else {
        thumb.innerHTML = '<i class="bi bi-cup-straw" aria-hidden="true"></i>';
      }

      const main = document.createElement('div');
      main.className = 'lookup-main';
      const title = document.createElement('div');
      title.className = 'lookup-title';
      title.textContent = (item.brewery ? item.brewery + ' · ' : '') + item.name;
      const sub = document.createElement('div');
      sub.className = 'lookup-sub';
      sub.textContent = [
        item.name_en,
        item.style,
        item.is_draft ? '生啤' : '',
        item.has_pricing ? '含参考定价' : '',
      ].filter(Boolean).join(' · ') || '点击自动填充全部资料';
      main.append(title, sub);

      const aside = document.createElement('div');
      aside.className = 'lookup-aside';
      const isJiuhe = item.source === 'jiuhe';
      const badge = document.createElement('span');
      badge.className = 'lookup-badge' + (isJiuhe ? '' : ' lookup-badge--db');
      badge.textContent = isJiuhe ? '酒核 · UT' : '历史酒单';
      aside.appendChild(badge);
      if (item.rating) {
        const rate = document.createElement('span');
        rate.className = 'lookup-rating';
        rate.textContent = '评分 ' + item.rating;
        aside.appendChild(rate);
      }

      btn.append(thumb, main, aside);
      btn.addEventListener('click', () => selectCandidate(item));
      btn.addEventListener('mouseenter', () => highlightActive(idx));
      menu.appendChild(btn);
    });
    activeIndex = -1;
  };

  const flashField = el => {
    el.classList.add('is-autofilled');
    window.setTimeout(() => el.classList.remove('is-autofilled'), 1800);
  };

  const applyDetail = detail => {
    let filled = 0;
    Object.entries(fieldEls).forEach(([key, el]) => {
      if (isEdit && (key === 'capacity_ml' || key === 'cost') && el.value.trim() !== '' && el.dataset.autofilled !== '1') {
        return;
      }
      const val = (detail[key] || '').trim();
      if (val) {
        if (el.tagName === 'SELECT' && !Array.from(el.options).some(opt => opt.value === val)) {
          if (el.dataset.autofilled === '1') {
            el.value = '';
            delete el.dataset.autofilled;
            if (key === 'category') el.dispatchEvent(new Event('change', { bubbles: true }));
          }
          return;
        }
        el.value = val;
        el.dataset.autofilled = '1';
        delete el.dataset.userEdited;
        if (key === 'category') el.dispatchEvent(new Event('change', { bubbles: true }));
        if (key === 'poster_url') {
          if (posterFileInput && posterFileInput.value) posterFileInput.value = '';
          syncPoster();
        }
        flashField(el);
        filled++;
        return;
      }
      if (el.dataset.autofilled === '1') {
        el.value = key === 'capacity_ml' ? '20000' : '';
        delete el.dataset.autofilled;
        if (key === 'category') el.dispatchEvent(new Event('change', { bubbles: true }));
        if (key === 'poster_url') {
          if (posterFileInput && posterFileInput.value) posterFileInput.value = '';
          syncPoster();
        }
      }
    });

    const missing = [];
    if (!fieldEls.poster_url?.value.trim()) missing.push('酒标');
    if (!fieldEls.brewery?.value.trim()) missing.push('厂牌');
    if (!fieldEls.style?.value.trim()) missing.push('风格');
    if (!fieldEls.abv?.value.trim()) missing.push('ABV');
    if (!fieldEls.ibu?.value.trim()) missing.push('IBU');
    if (isPurchase && !fieldEls.cost?.value.trim()) missing.push('每桶进价');
    if (isPurchase || isEdit) {
      const hasPrice = ['price_150', 'price_330', 'price_500'].some(k => fieldEls[k]?.value.trim());
      if (!hasPrice) missing.push('杯型售价');
    }

    const srcName = detail.source_label || (detail.source === 'jiuhe' ? '酒核 · UT 同步库' : '本店历史酒单');
    let summary = '已从「' + srcName + '」填充 ' + filled + ' 项信息';
    summary += missing.length
      ? '；仍需确认或补充：' + missing.join('、')
      : (isPurchase ? '；资料已完整，核对后即可入库' : '；资料已就绪，核对后可保存');
    setStatus('ok', summary, { showSwitch: true });
    document.dispatchEvent(new Event('keg:pricing-sync'));
  };

  const selectCandidate = async item => {
    closeMenu();
    if (detailAbort) detailAbort.abort();
    detailAbort = new AbortController();
    setStatus('loading', '正在拉取「' + (item.brewery ? item.brewery + ' · ' : '') + item.name + '」的完整资料…');
    try {
      const u = '/admin/beers/lookup/detail?source=' + encodeURIComponent(item.source || 'catalog') + '&id=' + encodeURIComponent(item.id);
      const res = await fetch(u, { signal: detailAbort.signal, headers: { Accept: 'application/json' } });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const detail = await res.json();
      applyDetail(detail);
    } catch (err) {
      if (err && err.name === 'AbortError') return;
      setStatus('warn', '拉取该酒款详情失败，请换一个候选或手动填写', { showSwitch: candidates.length > 1 });
    }
  };

  const runSearch = async ({ autoFillFirst = false, allowEmpty = false } = {}) => {
    const q = nameInput.value.trim();
    if (!q && !allowEmpty) {
      closeMenu();
      return;
    }
    if (searchAbort) searchAbort.abort();
    searchAbort = new AbortController();

    if (autoFillFirst || allowEmpty) {
      searchBtn.disabled = true;
      setStatus('loading', q ? '正在历史酒单与酒核 UT 库搜索「' + q + '」…' : '正在加载最近的历史酒单…');
    }

    try {
      const res = await fetch('/admin/beers/lookup?q=' + encodeURIComponent(q), {
        signal: searchAbort.signal,
        headers: { Accept: 'application/json' },
      });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const data = await res.json();
      candidates = Array.isArray(data.items) ? data.items : [];

      if (autoFillFirst && q) {
        if (candidates.length > 0) {
          await selectCandidate(candidates[0]);
        } else {
          closeMenu();
          setStatus(
            'warn',
            data.warning || (isPurchase ? '未搜到「' + q + '」，手动填写入库后会自动存入历史酒单' : '未搜到「' + q + '」，请换个关键词或直接手动填写')
          );
        }
        return;
      }

      renderMenu(candidates);
      menu.hidden = false;
      if (data.warning) {
        setStatus('warn', data.warning);
      } else if (!candidates.length) {
        setStatus(
          'warn',
          q ? '未搜到匹配酒款，请换个关键词或手动填写' : '历史酒单暂无记录，请手动填写'
        );
      } else if (!q) {
        setStatus('ok', '已列出最近 ' + candidates.length + ' 款历史酒单，请从下拉列表选择一款');
      } else {
        statusEl.hidden = true;
      }
    } catch (err) {
      if (err && err.name === 'AbortError') return;
      closeMenu();
      setStatus('warn', '搜索请求失败，可直接手动填写酒款信息');
    } finally {
      searchBtn.disabled = false;
    }
  };

  searchBtn.addEventListener('click', () => {
    const hasQ = nameInput.value.trim().length > 0;
    runSearch({ autoFillFirst: hasQ, allowEmpty: true });
  });
  nameInput.addEventListener('input', () => {
    window.clearTimeout(debounceTimer);
    const q = nameInput.value.trim();
    if (q.length < 2) {
      if (searchAbort) searchAbort.abort();
      closeMenu();
      return;
    }
    debounceTimer = window.setTimeout(() => runSearch({ autoFillFirst: false }), 300);
  });

  nameInput.addEventListener('keydown', ev => {
    if (ev.isComposing || ev.keyCode === 229) return;
    if (ev.key === 'Escape') {
      closeMenu();
      return;
    }
    if (ev.key === 'Enter') {
      ev.preventDefault();
      if (!menu.hidden && activeIndex >= 0 && candidates[activeIndex]) {
        selectCandidate(candidates[activeIndex]);
      } else if (nameInput.value.trim().length > 0) {
        window.clearTimeout(debounceTimer);
        runSearch({ autoFillFirst: true });
      }
      return;
    }
    if (menu.hidden || !candidates.length) return;
    if (ev.key === 'ArrowDown') {
      ev.preventDefault();
      highlightActive((activeIndex + 1) % candidates.length);
    } else if (ev.key === 'ArrowUp') {
      ev.preventDefault();
      highlightActive((activeIndex - 1 + candidates.length) % candidates.length);
    }
  });

  document.addEventListener('click', ev => {
    if (!wrap.contains(ev.target)) closeMenu();
  });
})();
