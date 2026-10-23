// SPDX-License-Identifier: GPL-3.0-or-later

// 本机界面外壳：导航、页面切换、事件流、状态栏和左下角的按钮。
// 每个页面是 pages/ 下的一个模块，导出 mount(root, query)，返回离开页面时调用的清理函数。

import { h, $, icon, bus, dialog, notice, button, openMenu, soon, frame } from './lib.js';
import { store, loadState, loadDevices, loadTasks } from './store.js';
import { links, openLink } from './actions.js';
import * as devicesPage from './pages/devices.js';
import * as collectPage from './pages/collect.js';
import * as healthPage from './pages/health.js';
import * as resultsPage from './pages/results.js';
import * as osPage from './pages/os.js';
import * as settingsPage from './pages/settings.js';

const pages = [
  { id: 'devices', title: '设备列表', icon: 'devices', mod: devicesPage },
  { id: 'collect', title: '日志收集', icon: 'collect', mod: collectPage },
  { id: 'health', title: '健康与告警', icon: 'health', mod: healthPage },
  { id: 'results', title: '收集结果', icon: 'results', mod: resultsPage },
  { id: 'os', title: 'OS 巡检', icon: 'os', mod: osPage },
  { id: 'settings', title: '设置', icon: 'settings', mod: settingsPage, sep: true },
];

function buildNav() {
  $('nav').replaceChildren(...pages.flatMap((p) => [
    p.sep ? h('div', { class: 'nav-sep' }) : null,
    h('a', { href: `#/${p.id}`, dataset: { page: p.id } },
      icon(p.icon), h('span', { class: 'nav-text', text: p.title }), h('span', { class: 'nav-badge', hidden: true })),
  ].filter(Boolean)));
}

// ---------- 页面切换 ----------

let current = null;

function route() {
  const [path, query] = location.hash.replace(/^#\/?/, '').split('?');
  const p = pages.find((x) => x.id === path) || pages[0];
  current?.cleanup?.();
  document.querySelector('.menu')?.remove();
  for (const a of $('nav').querySelectorAll('a')) {
    const on = a.dataset.page === p.id;
    a.classList.toggle('active', on);
    if (on) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  document.title = `${p.title} - LogDiag Collector`;
  // 每次换一个新的容器，旧页面挂在元素上的事件随元素一起丢掉。
  const root = h('div', { class: 'page', id: 'page' });
  $('page').replaceWith(root);
  $('main').scrollTop = 0;
  let cleanup;
  try {
    cleanup = p.mod.mount(root, new URLSearchParams(query || ''));
  } catch (err) {
    console.error(err);
    root.append(notice('error', h('p', { text: `页面出错：${err.message}。请重新打开程序；问题持续出现请反馈给我们。` })));
  }
  current = { id: p.id, cleanup };
}

// ---------- 状态栏、导航角标、模块开关 ----------

const renderStatus = frame(() => {
  const n = store.devices.length;
  const out = store.state?.settings?.output_dir;
  $('status').textContent = [n ? `设备 ${n} 台` : '还没有设备', out && `日志保存到 ${out}`].filter(Boolean).join(' · ');
  const running = store.tasks.filter((t) => t.state === 'running').length;
  const queued = store.tasks.filter((t) => t.state === 'queued').length;
  const link = $('status-tasks');
  link.hidden = running + queued === 0;
  link.textContent = `收集中 ${running} 台${queued ? `，排队 ${queued} 台` : ''}`;
  const badge = $('nav').querySelector('[data-page="collect"] .nav-badge');
  badge.hidden = running + queued === 0;
  badge.textContent = String(running + queued);
  badge.title = link.textContent;
});

function applyModules() {
  const os = $('nav').querySelector('[data-page="os"]');
  os.hidden = store.state?.settings?.os_module === false;
}

function renderVersion() {
  const info = store.state?.info;
  if (!info) return;
  $('version').textContent = `v${info.version}`;
  $('version').title = info.commit ? `提交 ${info.commit.slice(0, 7)} · ${info.go}` : info.go;
}

// 启动时发现的问题（例如设备库文件损坏、已备份）显示在页面顶部，可以关闭。
function renderBanners() {
  const box = $('banners');
  for (const w of store.state?.warnings || []) {
    const n = notice('warn', h('p', { text: w }));
    n.append(button('', { kind: 'ghost', size: 'small', icon: 'close', title: '关闭', 'aria-label': '关闭', onclick: () => n.remove() }));
    box.append(n);
  }
}

// ---------- 与本机服务的连接 ----------

let offline = null;

function setConnected(ok, closed) {
  const tag = $('conn');
  tag.textContent = ok ? '已连接' : '未连接';
  tag.className = `tag ${ok ? 'tag-green' : 'tag-red'}`;
  if (ok) {
    offline?.remove();
    offline = null;
    return;
  }
  const text = closed
    ? '与本机服务的连接已断开。程序可能已经退出，请重新运行 LogDiag Collector。'
    : '与本机服务的连接已断开，正在重试。如果程序已经退出，请重新运行 LogDiag Collector。';
  offline?.remove();
  offline = notice('error', h('p', { text }));
  $('banners').prepend(offline);
}

function connect() {
  // 断开后浏览器会自动重连；重连上会收到 hello，store 据此重新读取全部数据。
  const es = new EventSource('/api/events');
  for (const name of ['hello', 'device', 'devices', 'task']) {
    es.addEventListener(name, (e) => {
      let data = null;
      try {
        data = e.data ? JSON.parse(e.data) : null;
      } catch { /* 忽略 */ }
      bus.emit(`event:${name}`, data);
    });
  }
  es.addEventListener('hello', () => setConnected(true));
  es.addEventListener('error', () => setConnected(false, es.readyState === EventSource.CLOSED));
}

// ---------- 左下角：检查更新、语言、反馈 ----------

function showUpdate() {
  const info = store.state?.info || {};
  dialog({
    title: '检查更新',
    body: [
      h('dl', { class: 'kv' },
        h('dt', { text: '当前版本' }), h('dd', { text: `v${info.version || ''}` }),
        info.commit ? [h('dt', { text: '提交' }), h('dd', { class: 'mono', text: info.commit.slice(0, 12) })] : null),
      notice('soon',
        h('p', {}, h('strong', { text: '一键检查更新即将支持' }), '，正式版 v0.1 提供，只在你点击时联网。'),
        h('p', { text: '现在可以打开发布页，对照上面的版本号下载新版本。' })),
    ],
    actions: [
      { text: '关闭' },
      { text: '打开发布页', kind: 'primary', icon: 'open', onclick: () => openLink(links.releases) },
    ],
  });
}

function showLanguage(e) {
  openMenu(e.currentTarget, [
    { text: '简体中文', checked: true },
    { text: 'English', disabled: true, badge: soon('v0.2') },
  ]);
}

function showFeedback() {
  dialog({
    title: '问题反馈',
    body: [
      h('p', { class: 'dialog-text', text: '遇到问题或有建议，请到 GitHub 提交，写清楚设备品牌、型号、固件版本和操作步骤。' }),
      notice('info', h('p', {}, h('strong', { text: '收集失败时' }),
        '：在“日志收集”页点该任务的“诊断记录”导出文件，附在反馈里。诊断记录已去掉密码、令牌、Cookie 和设备 IP，但还有型号、序列号、主机名，附上前请看一遍。')),
      notice('soon', h('p', {}, h('strong', { text: '在程序里直接提交反馈即将支持' }), '。')),
    ],
    actions: [
      { text: '关闭' },
      { text: '打开 GitHub 反馈页', kind: 'primary', icon: 'open', onclick: () => openLink(links.issues) },
    ],
  });
}

function buildSideIcons() {
  const items = [
    ['update', '检查更新', showUpdate],
    ['language', '语言', showLanguage],
    ['feedback', '问题反馈', showFeedback],
  ];
  $('side-icons').replaceChildren(...items.map(([ic, title, onclick]) =>
    h('button', { type: 'button', class: 'icon-btn', title, 'aria-label': title, onclick }, icon(ic))));
  $('analyze').prepend(icon('open'));
}

// ---------- 启动 ----------

async function start() {
  buildNav();
  buildSideIcons();
  try {
    await Promise.all([loadState(), loadDevices(), loadTasks()]);
  } catch (err) {
    $('page').append(notice('error', h('p', { text: `读取数据失败：${err.message}` })));
    return;
  }
  renderVersion();
  renderBanners();
  applyModules();
  renderStatus();
  bus.on('changed:state', () => { applyModules(); renderStatus(); });
  bus.on('changed:devices', renderStatus);
  bus.on('changed:tasks', renderStatus);
  window.addEventListener('hashchange', route);
  route();
  connect();
}

start();
