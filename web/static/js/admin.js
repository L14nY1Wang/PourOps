(() => {
  const root = document.documentElement;
  const themeButton = document.querySelector('.theme-toggle');
  const syncTheme = () => {
    const dark = root.dataset.theme === 'dark';
    themeButton?.setAttribute('aria-pressed', String(dark));
    const icon = themeButton?.querySelector('i');
    if (icon) icon.className = `bi bi-${dark ? 'sun' : 'moon'}`;
  };
  syncTheme();
  themeButton?.addEventListener('click', () => {
    root.dataset.theme = root.dataset.theme === 'dark' ? 'light' : 'dark';
    try { localStorage.setItem('pourops-theme', root.dataset.theme); } catch (_) {}
    syncTheme();
    document.dispatchEvent(new Event('pourops:theme-change'));
  });

  document.querySelector('.sidebar-link.active')?.setAttribute('aria-current', 'page');

  const sidebar = document.querySelector('.app-sidebar');
  const toggle = document.querySelector('.mobile-menu-toggle');
  const backdrop = document.querySelector('.sidebar-backdrop');
  const workspace = document.querySelector('.app-workspace');
  const mobile = window.matchMedia('(max-width: 991px)');
  let returnFocus = null;

  function setMenu(open) {
    document.body.classList.toggle('menu-open', open);
    toggle?.setAttribute('aria-expanded', String(open));
    if (backdrop) backdrop.hidden = !open;
    if (sidebar) sidebar.inert = mobile.matches && !open;
    if (workspace) workspace.inert = mobile.matches && open;
    if (open) {
      returnFocus = document.activeElement;
      sidebar?.querySelector('a')?.focus();
    } else if (returnFocus) {
      returnFocus.focus();
      returnFocus = null;
    }
  }
  setMenu(false);
  toggle?.addEventListener('click', () => setMenu(!document.body.classList.contains('menu-open')));
  backdrop?.addEventListener('click', () => setMenu(false));
  sidebar?.addEventListener('click', event => {
    if (event.target.closest('a') && mobile.matches) setMenu(false);
  });
  mobile.addEventListener('change', () => setMenu(false));

  document.addEventListener('keydown', event => {
    if (event.key === 'Escape' && document.body.classList.contains('menu-open')) {
      setMenu(false);
    }
    if (event.key === 'Tab' && document.body.classList.contains('menu-open') && sidebar) {
      const links = [...sidebar.querySelectorAll('a, button')];
      if (!links.length) return;
      const first = links[0];
      const last = links[links.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    }
  });

  document.querySelectorAll('form').forEach(form => {
    form.addEventListener('submit', event => {
      if (form.noValidate) return;
      if (!form.checkValidity()) {
        event.preventDefault();
        form.reportValidity();
      }
      form.classList.add('was-validated');
    });
  });

  document.querySelectorAll('input[type="search"]').forEach(input => {
    if (input.closest('.po-search')) return;
    const wrap = document.createElement('div');
    wrap.className = 'po-search';
    const icon = document.createElement('i');
    icon.className = 'po-search__icon bi bi-search';
    icon.setAttribute('aria-hidden', 'true');
    const clear = document.createElement('button');
    clear.type = 'button';
    clear.className = 'po-search__clear';
    clear.setAttribute('aria-label', '清空搜索');
    clear.innerHTML = '<i class="bi bi-x" aria-hidden="true"></i>';
    if (!input.getAttribute('aria-label') && !input.labels?.length) {
      input.setAttribute('aria-label', input.placeholder || '搜索');
    }
    input.style.width = '';
    input.before(wrap);
    wrap.append(icon, input, clear);
    input.classList.add('po-search__input');

    const sync = () => {
      clear.hidden = !input.value || input.disabled || input.readOnly;
    };
    input.addEventListener('input', sync);
    input.addEventListener('change', sync);
    input.form?.addEventListener('reset', () => setTimeout(sync));
    clear.addEventListener('click', () => {
      input.value = '';
      sync();
      input.dispatchEvent(new Event('input', { bubbles: true }));
      input.dispatchEvent(new Event('change', { bubbles: true }));
      input.focus();
    });
    sync();
  });
})();
