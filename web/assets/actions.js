// SPDX-License-Identifier: GPL-3.0-or-later

// 几个页面共用的操作：开始收集、显示跳过的设备、在资源管理器里打开、打开外部网页、检查更新。

import { h, api, dialog, notice, toast, showError } from './lib.js';
import { store, support, deviceLabel } from './store.js';

export const links = {
  logdiag: 'https://logdiag.com/',
  repo: 'https://github.com/dylanlabs/logdiag-collector',
  releases: 'https://github.com/dylanlabs/logdiag-collector/releases',
  issues: 'https://github.com/dylanlabs/logdiag-collector/issues',
};

// openLink 在系统默认浏览器里打开外部网页（只在用户点击时调用）。
// 界面窗口用的是单独的 Edge 用户数据文件夹，直接 window.open 会开在那里，而不是用户自己的浏览器。
export function openLink(url) {
  api.post('/api/open', { target: 'link', url }).catch(showError);
}

// openPath 在资源管理器里打开保存位置（output）、数据文件夹（data）或保存位置里的某个文件夹/文件（path）。
export async function openPath(target, path) {
  try {
    await api.post('/api/open', { target, path });
  } catch (err) {
    showError(err);
  }
}

// startCollect 对设备开始收集。界面先挡掉还不支持的品牌和类型，其余交给后台（后台还会跳过缺账号、
// 正在收集、上次认证失败的设备）。有任务开始时切到日志收集页。
export async function startCollect(ids) {
  const skipped = [];
  const ok = [];
  for (const id of ids) {
    const d = store.devices.find((x) => x.id === id);
    if (!d) continue;
    const s = support(d);
    if (s.blocked) skipped.push({ label: deviceLabel(d), reason: s.reason });
    else ok.push(id);
  }
  let started = [];
  if (ok.length) {
    try {
      const res = await api.post('/api/tasks', { device_ids: ok });
      started = res.started;
      skipped.push(...res.skipped);
    } catch (err) {
      return showError(err);
    }
  }
  if (started.length) {
    toast(`已开始收集 ${started.length} 台设备`, 'ok');
    location.hash = '#/collect';
  }
  if (skipped.length) {
    showSkipped(started.length ? `${skipped.length} 台设备没有开始收集` : '没有开始收集', skipped);
  }
  return started;
}

// showSkipped 列出被跳过的设备和原因。
export function showSkipped(title, list) {
  dialog({
    title,
    wide: true,
    body: [
      h('p', { class: 'dialog-text', text: '下面这些设备已跳过，处理后可以再试一次：' }),
      h('div', { class: 'table-wrap short bordered' },
        h('table', { class: 'grid' },
          h('thead', {}, h('tr', {}, h('th', { text: '设备' }), h('th', { text: '原因' }))),
          h('tbody', {}, list.map((s) => h('tr', {}, h('td', { text: s.label }), h('td', { class: 'wrap', text: s.reason })))))),
    ],
    actions: [{ text: '知道了', kind: 'primary' }],
  });
}

// checkUpdate 检查更新，只在用户点击时调用：后台先读 logdiag.com 的最新版本信息，连不上时读 GitHub。
// 有新版本时由用户决定是否打开下载页，程序自己不下载、不安装。
export function checkUpdate() {
  const version = store.state?.info?.version || '';
  let page = '';
  const d = dialog({
    title: '检查更新',
    body: [
      h('p', { class: 'dialog-text', text: `当前版本 v${version}，正在读取最新版本信息……` }),
      h('div', { class: 'progress indeterminate update-progress' }, h('div', { class: 'progress-bar' })),
    ],
    actions: [
      { text: '关闭' },
      { text: '打开下载页', kind: 'primary', icon: 'open', onclick: () => openLink(page) },
    ],
  });
  const [closeBtn, openBtn] = d.buttons;
  openBtn.hidden = true;
  // show 显示结果；link 非空时显示打开网页的按钮，later 为 true 时关闭按钮改叫“以后再说”。
  const show = (link, label, later, ...content) => {
    d.body.replaceChildren(...content.filter(Boolean));
    page = link;
    openBtn.hidden = !link;
    if (link) openBtn.querySelector('span').textContent = label;
    if (later) closeBtn.querySelector('span').textContent = '以后再说';
  };
  api.post('/api/update/check').then((r) => {
    const date = r.released_at ? `，${r.released_at.slice(0, 10)} 发布` : '';
    const notes = r.notes ? h('div', { class: 'update-notes', text: r.notes }) : null;
    const via = r.source === 'GitHub' ? h('p', { class: 'muted update-via', text: 'logdiag.com 暂时连不上，以上信息来自 GitHub。' }) : null;
    // 下载页不在允许的网站上时（page 为空）改为打开本项目的发布页。
    const target = r.page ? [r.page, '打开下载页'] : [links.releases, '打开发布页'];
    if (r.required) {
      show(...target, true, notice('warn', h('p', {}, h('strong', { text: `当前版本 v${r.current} 已停止支持` }),
        `，请升级到 v${r.latest}${date}。旧版本可能有影响使用或安全的问题。`)), notes, via);
    } else if (r.dev) {
      show(...target, true, notice('info', h('p', { text: `这是开发版本 v${r.current}。最新的正式版本是 v${r.latest}${date}。` })), notes, via);
    } else if (r.newer) {
      show(...target, true, notice('info', h('p', {}, h('strong', { text: `发现新版本 v${r.latest}` }), `（当前 v${r.current}）${date}。`)), notes, via);
    } else {
      show('', '', false, notice('ok', h('p', { text: `已是最新版本（v${r.current}）。` })), via);
    }
  }).catch((err) => {
    show(links.releases, '打开发布页', false, notice('error', h('p', { text: err.message || String(err) })));
  });
}
