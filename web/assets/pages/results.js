// SPDX-License-Identifier: GPL-3.0-or-later

// 收集结果：列出保存位置里的日志包，按客户打成离线上传包。

import {
  h, icon, button, tag, notice, api, bus, dialog, showError, fmtTime, fmtSize, input, frame, empty, pageHead, soon,
  keyedRows, setChildren,
} from '../lib.js';
import { logTypes } from '../store.js';
import { openPath, openLink, links } from '../actions.js';

const view = { search: '', customer: '', selected: new Set() };
const NO_CUSTOMER = '\x00';

export function mount(root) {
  let entries = [];
  let outputDir = '';
  let loaded = false;
  let shown = [];
  const rowCache = new Map();

  const search = input({
    type: 'search', placeholder: '搜索客户、IP、型号、序列号…', value: view.search, 'aria-label': '搜索结果',
    oninput: () => { view.search = search.value; render(); },
  });
  const customer = h('select', { class: 'input', 'aria-label': '按客户筛选', onchange: () => { view.customer = customer.value; render(); } });
  const selInfo = h('span', { class: 'selection-info' });
  const btnPackage = button('生成离线上传包', { kind: 'primary', icon: 'package', onclick: () => buildPackage() });
  const btnUpload = button('上传到 LogDiag', { icon: 'upload', disabled: true, title: '即将支持（1.1）' });
  const headCheck = h('input', {
    type: 'checkbox', 'aria-label': '全选',
    onchange: () => {
      const all = shown.length > 0 && shown.every((e) => view.selected.has(e.dir));
      for (const e of shown) {
        if (all) view.selected.delete(e.dir);
        else view.selected.add(e.dir);
      }
      render();
    },
  });
  const tbody = h('tbody');
  const foot = h('div', { class: 'table-foot' });
  const tableCard = h('section', { class: 'card card-flush' },
    h('div', { class: 'toolbar' },
      h('div', { class: 'search' }, icon('search'), search), customer,
      h('div', { class: 'spacer' }), selInfo, h('span', { class: 'btn-with-tag' }, btnUpload, soon('1.1')), btnPackage),
    h('div', { class: 'table-wrap' },
      h('table', { class: 'grid' },
        h('thead', {}, h('tr', {}, h('th', { class: 'cell-check' }, headCheck),
          ['收集时间', '客户名称', '管理口 IP', '品牌', '型号', '序列号', '日志类型', '大小', '文件', ''].map((t) => h('th', { text: t })))),
        tbody)),
    foot);
  const emptyCard = h('section', { class: 'card' });

  tbody.addEventListener('click', (e) => {
    const tr = e.target.closest('tr[data-dir]');
    if (!tr || e.target.closest('button, a')) return;
    const dir = tr.dataset.dir;
    if (view.selected.has(dir)) view.selected.delete(dir);
    else view.selected.add(dir);
    render();
  });

  root.append(
    pageHead('收集结果', '保存位置里已收集的日志包。选中同一客户的结果打成离线上传包，再到 LogDiag 网站上传分析。',
      button('刷新', { icon: 'refresh', onclick: () => load() }),
      button('打开保存位置', { icon: 'folder', onclick: () => openPath('output') })),
    notice('soon',
      h('p', {}, h('strong', { text: '在程序里直接上传即将支持（1.1）。' }), 'LogDiag 网站识别离线上传包的功能也在开发中，在此之前可以把包里的日志文件直接上传到网站分析。')),
    tableCard, emptyCard);

  const row = (e) => {
    const sel = view.selected.has(e.dir);
    const d = e.device || {};
    const missing = (e.files || []).filter((f) => f.missing || !f.size_ok).length;
    return h('tr', { class: sel ? 'clickable selected' : 'clickable', dataset: { dir: e.dir }, title: e.path },
      h('td', { class: 'cell-check' }, h('input', { type: 'checkbox', checked: sel, 'aria-label': '选择' })),
      h('td', { text: fmtTime(e.collected_at) }),
      h('td', { text: e.customer || '' }),
      h('td', { text: d.mgmt_ip || '' }),
      h('td', { text: d.vendor || '' }),
      h('td', { text: d.model || '' }),
      h('td', { text: d.serial || '' }),
      h('td', { text: logTypes[e.log?.type] || e.log?.type || '' }),
      h('td', { class: 'num', text: fmtSize(e.size) }),
      h('td', {}, e.problem ? tag(missing ? `缺 ${missing} 个文件` : '有问题', 'red', e.problem) : tag(`${(e.files || []).length} 个文件`, 'green')),
      h('td', { class: 'cell-actions' },
        button('', { kind: 'ghost', size: 'small', icon: 'folder', title: '在资源管理器里打开', 'aria-label': '打开文件夹', onclick: () => openPath('path', e.path) })));
  };

  const render = frame(() => {
    if (!loaded) return;
    tableCard.hidden = entries.length === 0;
    emptyCard.hidden = entries.length > 0;
    if (!entries.length) {
      emptyCard.replaceChildren(empty('results', '还没有收集结果',
        h('span', {}, '收集成功的日志包保存在 ', h('span', { class: 'path', text: outputDir }), '，会显示在这里。'),
        button('去收集日志', { kind: 'primary', icon: 'collect', onclick: () => { location.hash = '#/collect'; } }),
        button('打开保存位置', { icon: 'folder', onclick: () => openPath('output') })));
      return;
    }
    const names = [...new Set(entries.map((e) => e.customer || NO_CUSTOMER))].sort((a, b) => a.localeCompare(b, 'zh-CN'));
    if (view.customer && !names.includes(view.customer)) view.customer = '';
    customer.replaceChildren(h('option', { value: '', text: '全部客户' }),
      ...names.map((n) => h('option', { value: n, selected: n === view.customer, text: n === NO_CUSTOMER ? '（未填客户）' : n })));

    const dirs = new Set(entries.map((e) => e.dir));
    for (const d of view.selected) if (!dirs.has(d)) view.selected.delete(d);
    const q = view.search.trim().toLowerCase();
    shown = entries.filter((e) => {
      if (view.customer && (e.customer || NO_CUSTOMER) !== view.customer) return false;
      const d = e.device || {};
      return !q || [e.customer, d.mgmt_ip, d.vendor, d.model, d.serial, d.hostname, e.log?.type, logTypes[e.log?.type]]
        .some((v) => v && v.toLowerCase().includes(q));
    });
    setChildren(tbody, shown.length
      ? keyedRows(rowCache, shown, (e) => e.dir, (e) => JSON.stringify([e, view.selected.has(e.dir)]), row)
      : [h('tr', {}, h('td', { class: 'cell-empty', colspan: 11, text: '没有符合条件的结果' }))]);

    const picked = shown.filter((e) => view.selected.has(e.dir));
    const customers = new Set(picked.map((e) => e.customer || ''));
    const broken = picked.filter((e) => e.problem).length;
    headCheck.checked = picked.length > 0 && picked.length === shown.length;
    headCheck.indeterminate = picked.length > 0 && picked.length < shown.length;
    let reason = '';
    if (!picked.length) reason = '勾选同一客户的结果后打包';
    else if (customers.size > 1) reason = '一个离线上传包只能包含一个客户的结果';
    else if (broken) reason = `有 ${broken} 个结果文件不完整，请取消勾选或重新收集`;
    selInfo.textContent = picked.length ? `已选 ${picked.length} 个${reason ? `，${reason}` : ''}` : reason;
    selInfo.classList.toggle('text-danger', picked.length > 0 && !!reason);
    btnPackage.disabled = !!reason;

    const total = entries.reduce((n, e) => n + (e.size || 0), 0);
    foot.replaceChildren(
      h('span', { text: `共 ${entries.length} 个结果，${fmtSize(total)}` }),
      h('span', { class: 'spacer' }),
      h('span', { class: 'muted' }, '保存位置：', h('span', { class: 'path', text: outputDir })));
  });

  async function load() {
    try {
      const res = await api.get('/api/results');
      entries = res.entries || [];
      outputDir = res.output_dir;
      loaded = true;
      render();
    } catch (err) {
      showError(err);
    }
  }

  async function buildPackage() {
    const dirs = shown.filter((e) => view.selected.has(e.dir)).map((e) => e.dir);
    btnPackage.disabled = true;
    const label = btnPackage.querySelector('span');
    label.textContent = '正在打包…';
    try {
      const res = await api.post('/api/results/package', { dirs });
      view.selected.clear();
      render();
      dialog({
        title: '离线上传包已生成',
        body: [
          h('dl', { class: 'kv' },
            h('dt', { text: '文件' }), h('dd', { class: 'path', text: res.path }),
            h('dt', { text: '大小' }), h('dd', { text: fmtSize(res.size) }),
            h('dt', { text: '设备' }), h('dd', { text: `${res.devices} 台` })),
          notice('info', h('p', { text: '打包前已逐个核对文件的校验值。包里带有设备清单和校验值，供 LogDiag 网站识别。' })),
        ],
        actions: [
          { text: '打开所在文件夹', icon: 'folder', left: true, onclick: () => { openPath('path', res.path); return false; } },
          { text: '关闭' },
          { text: '打开 LogDiag', kind: 'primary', icon: 'open', onclick: () => openLink(links.logdiag) },
        ],
      });
    } catch (err) {
      showError(err);
    } finally {
      label.textContent = '生成离线上传包';
      render();
    }
  }

  const offs = [bus.on('changed:results', load)];
  load();
  return () => offs.forEach((off) => off());
}
