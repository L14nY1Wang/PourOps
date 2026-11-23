// 首屏绘制前同步主题状态，避免亮暗切换闪烁。
(() => {
  let theme = 'light';
  try { theme = localStorage.getItem('pourops-theme') || theme; } catch (_) {}
  document.documentElement.dataset.theme = theme === 'dark' ? 'dark' : 'light';
})();
