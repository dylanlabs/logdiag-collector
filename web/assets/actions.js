// SPDX-License-Identifier: GPL-3.0-or-later

// 几个页面共用的操作：开始收集、显示跳过的设备、在资源管理器里打开、打开外部网页。

import { h, api, dialog, toast, showError } from './lib.js';
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
