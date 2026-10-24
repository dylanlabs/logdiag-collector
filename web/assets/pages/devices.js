// SPDX-License-Identifier: GPL-3.0-or-later

// 设备列表：添加、批量添加、导入导出、凭据模板、检查连通、识别型号、发起收集。

import {
  h, icon, button, tag, notice, api, bus, dialog, confirm, toast, showError, download, fmtTime,
  field, optional, input, select, checkbox, withList, frame, empty, pageHead, menuButton, keyedRows, setChildren,
  customerFilter, matchCustomer,
} from '../lib.js';
import {
  store, loadDevices, collectingIDs, credentialName, deviceStatus, typeChoices, deviceLabel, brands, brandOf,
} from '../store.js';
import { startCollect, showSkipped, openPath } from '../actions.js';

// 切换页面后保留搜索词、筛选、排序和选中的设备。
const view = { search: '', customer: '', sort: 'mgmt_ip', dir: 1, selected: new Set(), anchor: -1 };

// 按连通状态排序时，有问题的排前面。
const statusRank = { 认证失败: 0, 识别失败: 1, 不可达: 2, 网络可达: 3, 服务可用: 4, 登录成功: 5, 收集中: 6 };

// 客户名称不单独占一列，用工具栏的客户筛选。
const columns = [
  { key: 'vendor', title: '品牌' },
  { key: 'model', title: '型号' },
  { key: 'serial', title: '序列号' },
  { key: 'hostname', title: '主机名' },
  { key: 'host_ip', title: '主机 IP', ip: true },
  { key: 'os', title: '系统' },
  { key: 'mgmt_ip', title: '管理口 IP', ip: true },
  { key: 'account', title: '管理口账号', value: accountText, render: accountCell },
  { key: 'status', title: '连通状态', value: (d, c) => statusRank[deviceStatus(d, c).text] ?? 9, render: statusCell },
  { key: 'last', title: '最后收集', value: (d) => d.last_collect?.at || '', render: lastCell },
];

// ---------- 单元格 ----------

function accountText(d) {
  return d.credential_id ? credentialName(d.credential_id) : d.user || '';
}

function accountCell(d) {
  if (d.credential_id) {
    const name = credentialName(d.credential_id);
    return h('span', { class: 'cell-icon', title: '使用凭据模板' }, icon('key'), name || '（模板已删除）');
  }
  if (!d.user && !d.has_password) return h('span', { class: 'faint', text: '未填' });
  return h('span', {}, d.user || h('span', { class: 'faint', text: '无账号' }),
    !d.has_password && h('span', { class: 'faint', text: ' · 未填密码' }));
}

function statusCell(d, collecting) {
  const st = deviceStatus(d, collecting);
  const s = d.status || {};
  const tip = [s.reach_message, s.login_message, s.checked_at && `检查于 ${fmtTime(s.checked_at, true)}`].filter(Boolean).join('\n');
  return tag(st.text, st.tone, tip || null);
}

function lastCell(d) {
  const l = d.last_collect;
  if (!l) return '';
  return h('span', { class: l.ok ? '' : 'text-danger', title: l.message }, fmtTime(l.at), l.ok ? '' : ' 失败');
}

// ---------- 排序与筛选 ----------

const collator = new Intl.Collator('zh-CN', { numeric: true, sensitivity: 'base' });

function ipKey(s) {
  const m = /^(\d+)\.(\d+)\.(\d+)\.(\d+)/.exec(s || '');
  return m ? m.slice(1).map((n) => n.padStart(3, '0')).join('.') + s.slice(m[0].length) : s || '';
}

function sortValue(col, d, collecting) {
  if (col.value) return col.value(d, collecting.has(d.id));
  const v = d[col.key] || '';
  return col.ip ? ipKey(v) : v;
}

function compare(a, b) {
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  return collator.compare(String(a), String(b));
}

function visibleDevices() {
  const q = view.search.trim().toLowerCase();
  const collecting = collectingIDs();
  const list = store.devices.filter((d) => {
    if (!matchCustomer(d.customer, view.customer)) return false;
    if (!q) return true;
    return [d.customer, d.vendor, d.model, d.serial, d.hostname, d.host_ip, d.os, d.mgmt_ip, d.user,
      d.location, d.notes, d.firmware, credentialName(d.credential_id)].some((v) => v && v.toLowerCase().includes(q));
  });
  const byIP = columns.find((c) => c.key === 'mgmt_ip');
  const col = columns.find((c) => c.key === view.sort) || byIP;
  const byCustomer = { key: 'customer' }; // 同一列的值相同时，同一客户的设备排在一起
  list.sort((a, b) => {
    const va = sortValue(col, a, collecting);
    const vb = sortValue(col, b, collecting);
    // 空值总排在最后。
    if (va === '' && vb !== '') return 1;
    if (vb === '' && va !== '') return -1;
    return compare(va, vb) * view.dir ||
      compare(sortValue(byCustomer, a, collecting), sortValue(byCustomer, b, collecting)) ||
      compare(sortValue(byIP, a, collecting), sortValue(byIP, b, collecting));
  });
  return { list, collecting };
}

// ---------- 页面 ----------

export function mount(root) {
  let shown = [];
  const rowCache = new Map();
  const selectedIDs = () => shown.filter((d) => view.selected.has(d.id)).map((d) => d.id);

  const search = input({
    type: 'search', placeholder: '搜索 IP、型号、序列号、客户…', value: view.search, 'aria-label': '搜索设备',
    oninput: () => { view.search = search.value; render(); },
  });
  const customer = customerFilter((v) => { view.customer = v; render(); });
  const selInfo = h('span', { class: 'selection-info' });
  const act = (fn) => async () => {
    const ids = selectedIDs();
    if (ids.length) {
      try {
        await fn(ids);
      } catch (err) {
        showError(err);
      }
    }
  };
  const btnCheck = button('检查连通', { icon: 'scan', title: '不登录，只检查管理口能不能连上、服务是否可用', onclick: act(doCheck) });
  const btnIdentify = button('识别型号', { icon: 'identify', title: '用填写的账号登录管理口，读取品牌、型号、序列号、主机名并回填', onclick: act(doIdentify) });
  const btnMore = menuButton('更多', {}, () => [
    { text: '批量设置…', icon: 'edit', onclick: act(batchEditDialog) },
    { text: '导出选中的设备（Excel）', icon: 'export', onclick: act((ids) => download(`/api/devices/export?format=xlsx&ids=${ids.join(',')}`)) },
    '-',
    { text: '删除…', icon: 'trash', onclick: act(doDelete) },
  ]);
  const btnCollect = button('收集日志', { kind: 'primary', icon: 'collect', onclick: act(startCollect) });
  const headCheck = h('input', { type: 'checkbox', 'aria-label': '全选', onchange: () => {
    const all = shown.length > 0 && shown.every((d) => view.selected.has(d.id));
    for (const d of shown) {
      if (all) view.selected.delete(d.id);
      else view.selected.add(d.id);
    }
    render();
  } });
  const thead = h('thead');
  const tbody = h('tbody');
  const foot = h('div', { class: 'table-foot' });
  const tableCard = h('section', { class: 'card card-flush' },
    h('div', { class: 'toolbar' },
      h('div', { class: 'search' }, icon('search'), search), customer.el,
      h('div', { class: 'spacer' }), selInfo, btnCheck, btnIdentify, btnMore, btnCollect),
    h('div', { class: 'table-wrap' }, h('table', { class: 'grid' }, thead, tbody)),
    foot);
  const emptyCard = h('section', { class: 'card' }, empty('devices', '还没有设备',
    '填写管理口 IP 和账号就能一键收集日志。也可以导入 Excel、CSV 设备表，或 BMCKit 导出的发现结果。',
    button('添加设备', { kind: 'primary', icon: 'plus', onclick: () => editDialog(null) }),
    button('批量添加', { icon: 'list', onclick: batchAddDialog }),
    button('导入', { icon: 'import', onclick: importDialog })));

  root.append(
    pageHead('设备列表', '填写管理口 IP 和账号即可；品牌、型号、序列号在识别或收集后自动回填。',
      button('凭据模板', { icon: 'key', onclick: credentialsDialog }),
      button('导入', { icon: 'import', onclick: importDialog }),
      menuButton('导出', { icon: 'export', title: '导出的表格不含密码' }, [
        { text: '导出全部设备（Excel）', icon: 'export', onclick: () => download('/api/devices/export?format=xlsx') },
        { text: '导出全部设备（CSV）', icon: 'export', onclick: () => download('/api/devices/export?format=csv') },
        '-',
        { text: '下载导入模板（Excel）', icon: 'record', onclick: () => download('/api/devices/template?format=xlsx') },
        { text: '下载导入模板（CSV）', icon: 'record', onclick: () => download('/api/devices/template?format=csv') },
      ]),
      button('批量添加', { icon: 'list', onclick: batchAddDialog }),
      button('添加设备', { kind: 'primary', icon: 'plus', onclick: () => editDialog(null) })),
    tableCard, emptyCard);

  thead.addEventListener('click', (e) => {
    const th = e.target.closest('th[data-key]');
    if (!th) return;
    if (view.sort === th.dataset.key) view.dir = -view.dir;
    else Object.assign(view, { sort: th.dataset.key, dir: 1 });
    render();
  });
  // 按住 Shift 连续选择时不要选中文字。
  tbody.addEventListener('mousedown', (e) => { if (e.shiftKey) e.preventDefault(); });
  tbody.addEventListener('click', (e) => {
    const tr = e.target.closest('tr[data-id]');
    if (!tr) return;
    if (e.target.closest('[data-edit]')) return editDialog(store.devices.find((d) => d.id === tr.dataset.id));
    if (e.target.closest('button, a')) return;
    const i = Number(tr.dataset.i);
    const on = !view.selected.has(tr.dataset.id);
    if (e.shiftKey && view.anchor >= 0) {
      for (const d of shown.slice(Math.min(view.anchor, i), Math.max(view.anchor, i) + 1)) {
        if (on) view.selected.add(d.id);
        else view.selected.delete(d.id);
      }
    } else if (on) view.selected.add(tr.dataset.id);
    else view.selected.delete(tr.dataset.id);
    view.anchor = i;
    render();
  });
  tbody.addEventListener('dblclick', (e) => {
    const tr = e.target.closest('tr[data-id]');
    if (tr && !e.target.closest('input, button')) editDialog(store.devices.find((d) => d.id === tr.dataset.id));
  });

  const render = frame(() => {
    const hasDevices = store.devices.length > 0;
    tableCard.hidden = !hasDevices;
    emptyCard.hidden = hasDevices;
    if (!hasDevices) return;

    view.customer = customer.update(store.devices.map((d) => d.customer), view.customer);

    const ids = new Set(store.devices.map((d) => d.id));
    for (const id of view.selected) if (!ids.has(id)) view.selected.delete(id);
    const { list, collecting } = visibleDevices();
    shown = list;

    const nSel = list.filter((d) => view.selected.has(d.id)).length;
    selInfo.textContent = nSel ? `已选 ${nSel} 台` : '勾选设备后可检查、识别、收集';
    for (const b of [btnCheck, btnIdentify, btnMore, btnCollect]) b.disabled = nSel === 0;
    headCheck.checked = nSel > 0 && nSel === list.length;
    headCheck.indeterminate = nSel > 0 && nSel < list.length;

    thead.replaceChildren(h('tr', {},
      h('th', { class: 'cell-check' }, headCheck),
      columns.map((c) => h('th', { class: 'sortable', dataset: { key: c.key }, title: '点击排序' }, c.title,
        view.sort === c.key && h('span', { class: 'sort', text: view.dir > 0 ? '↑' : '↓' }))),
      h('th', { class: 'cell-actions' }, h('span', { class: 'sr-only', text: '操作' }))));

    const row = (d, i) => {
      const sel = view.selected.has(d.id);
      return h('tr', { class: sel ? 'clickable selected' : 'clickable', dataset: { id: d.id, i } },
        h('td', { class: 'cell-check' }, h('input', { type: 'checkbox', checked: sel, 'aria-label': `选择 ${deviceLabel(d)}` })),
        columns.map((c) => h('td', {}, c.render ? c.render(d, collecting.has(d.id)) : d[c.key] || '')),
        h('td', { class: 'cell-actions' },
          button('', { kind: 'ghost', size: 'small', icon: 'edit', title: '编辑', 'aria-label': '编辑', dataset: { edit: d.id } })));
    };
    const sig = (d, i) => JSON.stringify([d, i, view.selected.has(d.id), collecting.has(d.id), credentialName(d.credential_id)]);
    setChildren(tbody, list.length
      ? keyedRows(rowCache, list, (d) => d.id, sig, row)
      : [h('tr', {}, h('td', { class: 'cell-empty', colspan: columns.length + 2, text: '没有符合条件的设备' }))]);

    const filtered = list.length !== store.devices.length;
    foot.replaceChildren(
      h('span', { text: `共 ${store.devices.length} 台${filtered ? `，当前显示 ${list.length} 台` : ''}` }),
      h('span', { class: 'spacer' }),
      h('span', { class: 'faint', text: '按住 Shift 点击可连续选择；双击一行编辑' }));
  });

  const offs = [bus.on('changed:devices', render), bus.on('changed:tasks', render)];
  render();
  return () => offs.forEach((off) => off());
}

// ---------- 批量操作 ----------

async function doCheck(ids) {
  const res = await api.post('/api/devices/check', { ids });
  if (res.started) toast(`正在检查 ${res.started} 台设备的连通状态`);
  else toast('选中的设备都没有填写管理口 IP', 'error');
}

async function doIdentify(ids) {
  const res = await api.post('/api/devices/identify', { ids });
  if (res.started) toast(`正在登录识别 ${res.started} 台设备`);
  if (res.skipped.length) showSkipped(res.started ? `${res.skipped.length} 台设备没有识别` : '没有开始识别', res.skipped);
}

async function doDelete(ids) {
  const ok = await confirm({
    title: `删除 ${ids.length} 台设备？`,
    message: '只从设备列表里删除，已收集的日志文件不会删除。',
    ok: '删除',
    danger: true,
  });
  if (!ok) return false;
  const res = await api.post('/api/devices/delete', { ids });
  for (const id of ids) view.selected.delete(id);
  toast(`已删除 ${res.deleted} 台设备`, 'ok');
  loadDevices();
  return true;
}

function selectNew(ids) {
  view.selected = new Set(ids || []);
  view.anchor = -1;
  view.search = '';
  view.customer = '';
  loadDevices();
}

// ---------- 账号、品牌（添加、编辑、批量添加、批量设置共用） ----------

let radioSeq = 0;

// accountFields 是管理口账号：用户名、密码两格，或者用凭据模板代替。keep 为 true 时（批量设置）多一个“不修改”。
// fields 放进表单和管理口 IP 同一排；options 是下面一排的“使用凭据模板”等选项。
function accountFields({ device, keep = false }) {
  const d = device || {};
  const creds = store.credentials;
  let mode = keep ? 'keep' : d.credential_id ? 'template' : 'manual';
  const user = input({ value: d.user || '', placeholder: '例如 Administrator', maxlength: 100 });
  const password = input({
    type: 'password', autocomplete: 'new-password', maxlength: 256,
    placeholder: d.has_password ? '已保存，留空表示不修改' : keep ? '留空表示不修改' : '',
  });
  const cred = select(creds.map((c) => [c.id, `${c.name}（${c.user}）`]), d.credential_id || creds[0]?.id || '');
  const userField = field('用户名', user);
  const passField = field('密码', password);
  const credField = field('凭据模板', cred, '改模板里的账号密码，用这个模板的设备都会跟着改。', 'cols-2');
  const clear = d.has_password ? checkbox('清除已保存的密码', false) : null;
  const noTemplates = !creds.length && h('span', { class: 'faint', text: '还没有凭据模板，可以在“凭据模板”里新建' });
  let options;
  if (keep) {
    const name = `account-${++radioSeq}`;
    options = h('div', { class: 'radio-row span-2' },
      h('span', { class: 'field-label', text: '管理口账号' }),
      [['keep', '不修改'], ['manual', '改为手动填写'], ['template', '改用凭据模板']].map(([v, text]) => h('label', { class: 'check' },
        h('input', {
          type: 'radio', name, value: v, checked: v === mode, disabled: v === 'template' && !creds.length,
          onchange: () => { mode = v; sync(); },
        }),
        h('span', { text }))),
      noTemplates);
  } else {
    options = h('div', { class: 'radio-row span-2' },
      checkbox('使用凭据模板', mode === 'template', {
        disabled: !creds.length,
        onchange: (e) => { mode = e.target.checked ? 'template' : 'manual'; sync(); },
      }),
      noTemplates, clear);
  }
  function sync() {
    userField.hidden = mode === 'template';
    passField.hidden = mode === 'template';
    credField.hidden = mode !== 'template';
    user.disabled = password.disabled = mode === 'keep';
    if (clear) clear.hidden = mode !== 'manual';
  }
  sync();
  return {
    fields: [userField, passField, credField],
    options,
    // check 检查必填项：手动填写时要有用户名和密码（编辑时密码已保存就不用再填）。
    check() {
      if (mode !== 'manual') return;
      if (!user.value.trim()) throw new Error('请填写用户名');
      if (!password.value && !d.has_password && !keep) throw new Error('请填写密码');
    },
    patch(p) {
      if (mode === 'template') p.credential_id = cred.value;
      else if (mode === 'manual') {
        p.credential_id = '';
        p.user = user.value.trim();
        if (password.value) p.password = password.value;
        else if (clear?.querySelector('input').checked) p.password = '';
      }
      return p;
    },
  };
}

const AUTO = '';
const KEEP = '\x00keep';

// brandSelect 是品牌下拉框，默认“自动识别”。收集时会自动判断品牌和系列，一般不用选；
// 只有关闭了 Redfish 的浪潮设备要选“浪潮”。value() 在用户没改动时返回设备表里原来的写法。
function brandSelect(current, { keep = false } = {}) {
  const known = brandOf(current);
  const options = [
    ...(keep ? [[KEEP, '不修改']] : []),
    [AUTO, keep ? '改为自动识别' : '自动识别（推荐）'],
    ...brands.map((b) => [b.vendor, b.status === 'supported' ? b.name : `${b.name}（收集计划 ${b.status}）`]),
  ];
  // 认不出的写法（例如识别到的其他厂商）单独列出来，保存时原样保留。
  if (current && !known) options.push([current, current]);
  const initial = keep ? KEEP : known ? known.vendor : current || AUTO;
  const el = select(options, initial);
  return {
    el,
    value: () => (el.value === initial && !keep ? current || '' : el.value),
    changed: () => el.value !== KEEP,
  };
}

// ---------- 添加、编辑单台设备 ----------

function editDialog(dev) {
  const d = dev || {};
  const f = {
    mgmt_ip: input({ value: d.mgmt_ip || '', placeholder: '例如 10.0.0.10' }),
    customer: input({ value: d.customer || '', maxlength: 100 }),
    type: select(typeChoices, d.type || 'server'),
    model: input({ value: d.model || '', maxlength: 100 }),
    serial: input({ value: d.serial || '', maxlength: 100 }),
    location: input({ value: d.location || '', placeholder: '机房、机柜、U 位', maxlength: 100 }),
    hostname: input({ value: d.hostname || '', maxlength: 100 }),
    host_ip: input({ value: d.host_ip || '', placeholder: 'OS 巡检用' }),
    os: input({ value: d.os || '', placeholder: '例如 CentOS 7.9', maxlength: 100 }),
    notes: h('textarea', { class: 'input', rows: 2, value: d.notes || '', maxlength: 2000 }),
  };
  const brand = brandSelect(d.vendor);
  const legacy = checkbox('允许旧版 TLS', !!d.legacy_tls);
  legacy.title = '只在老设备提示“TLS 握手失败”连不上时勾选';
  const account = accountFields({ device: dev });
  account.options.append(legacy);
  const status = dev ? statusSection(dev) : null;

  // 必填的管理口 IP、用户名、密码排第一排；其余都可以不填，品牌、型号等会在识别或收集时自动判断并回填。
  const form = h('div', { class: 'form form-3' },
    field('管理口 IP', f.mgmt_ip, '可以带端口，例如 10.0.0.10:8443'),
    ...account.fields,
    account.options,
    field('设备类型', f.type),
    field(optional('品牌'), brand.el, '收集时自动判断品牌和系列，一般不用选'),
    field(optional('客户名称'), withList(f.customer, store.customers)),
    h('div', { class: 'form-section' }, '设备信息', h('span', { class: 'form-section-hint', text: '识别型号或收集后自动填写' })),
    field(optional('型号'), f.model), field(optional('序列号'), f.serial), field(optional('位置'), f.location),
    field(optional('主机名'), f.hostname), field(optional('主机 IP'), f.host_ip), field(optional('系统'), f.os),
    field(optional('备注'), f.notes, null, 'span-2'));

  const actions = [{ text: '取消' }, {
    text: dev ? '保存' : '添加',
    kind: 'primary',
    onclick: async () => {
      const p = { legacy_tls: legacy.querySelector('input').checked, vendor: brand.value() };
      for (const [k, el] of Object.entries(f)) p[k] = el.value.trim();
      if (!p.mgmt_ip) throw new Error('请填写管理口 IP');
      if (!dev) account.check();
      account.patch(p);
      if (dev) {
        await api.post('/api/devices/update', { ids: [dev.id], patch: p });
        toast('已保存', 'ok');
        loadDevices();
      } else {
        const created = await api.post('/api/devices', p);
        toast('已添加设备', 'ok');
        selectNew([created.id]);
      }
    },
  }];
  if (dev) {
    actions.unshift({
      text: '删除', kind: 'danger', icon: 'trash', left: true,
      onclick: async () => (await doDelete([dev.id])) || false,
    });
  }
  const dlg = dialog({ title: dev ? `编辑设备 ${deviceLabel(dev)}` : '添加设备', wide: true, body: [status, form], actions });
  if (!dev) f.mgmt_ip.focus();
  return dlg;
}

// statusSection 是编辑对话框顶部的设备状态：连通、固件、最后一次收集。
function statusSection(d) {
  const st = deviceStatus(d, collectingIDs().has(d.id));
  const s = d.status || {};
  const l = d.last_collect;
  return h('dl', { class: 'kv kv-box' },
    h('dt', { text: '连通状态' }),
    h('dd', {}, tag(st.text, st.tone), ' ', h('span', { class: 'muted', text: [s.login_message, s.reach_message].filter(Boolean)[0] || '' })),
    h('dt', { text: '管理口固件' }), h('dd', { text: d.firmware || '—' }),
    h('dt', { text: '最后收集' }),
    h('dd', {}, l ? [
      `${fmtTime(l.at)}，${l.ok ? '' : '失败：'}${l.message}`,
      l.ok && l.dir ? [' ', button('打开文件夹', { kind: 'ghost', size: 'small', icon: 'folder', onclick: () => openPath('path', l.dir) })] : null,
    ] : '还没有收集过'));
}

// ---------- 批量添加 ----------

function batchAddDialog() {
  const addresses = h('textarea', {
    class: 'input', rows: 5, spellcheck: 'false',
    placeholder: '每行一个，也可以用逗号分隔，例如：\n10.0.0.10\n10.0.0.11-20\n10.0.1.0/24',
  });
  const customer = input({ maxlength: 100 });
  const type = select(typeChoices, 'server');
  const brand = brandSelect('');
  const account = accountFields({});
  dialog({
    title: '批量添加设备',
    wide: true,
    body: h('div', { class: 'form form-3' },
      field('管理口 IP', addresses, '支持单个地址、范围（10.0.0.11-20）和网段（/22 到 /32），一次最多 1024 个；客户下已有的地址会跳过。', 'span-2'),
      ...account.fields,
      h('div', { class: 'field-note' }, '这批设备用同一个账号；账号不同的请用“导入”。'),
      account.options,
      field('设备类型', type),
      field(optional('品牌'), brand.el, '收集时自动判断品牌和系列，一般不用选'),
      field(optional('客户名称'), withList(customer, store.customers))),
    actions: [{ text: '取消' }, {
      text: '添加', kind: 'primary', icon: 'plus',
      onclick: async () => {
        if (!addresses.value.trim()) throw new Error('请填写管理口 IP');
        account.check();
        const patch = account.patch({ customer: customer.value.trim(), type: type.value, vendor: brand.value() });
        const res = await api.post('/api/devices/batch', { addresses: addresses.value, patch });
        toast(`已添加 ${res.added} 台设备${res.skipped ? `，${res.skipped} 个地址已存在、已跳过` : ''}`, 'ok');
        selectNew(res.ids);
      },
    }],
  });
  addresses.focus();
}

// ---------- 批量设置 ----------

function batchEditDialog(ids) {
  const keep = '不修改';
  const customer = input({ placeholder: keep, maxlength: 100 });
  const type = select([['', keep], ...typeChoices], '');
  const brand = brandSelect('', { keep: true });
  const os = input({ placeholder: keep, maxlength: 100 });
  const location = input({ placeholder: keep, maxlength: 100 });
  const tls = select([['', keep], ['on', '允许'], ['off', '不允许']], '');
  const account = accountFields({ keep: true });
  dialog({
    title: `批量设置 ${ids.length} 台设备`,
    wide: true,
    body: [
      h('p', { class: 'dialog-text muted', text: '只修改填写了的项，留空的保持原样。' }),
      h('div', { class: 'form form-3' },
        account.options,
        ...account.fields,
        h('div'),
        field('设备类型', type),
        field('品牌', brand.el),
        field('客户名称', withList(customer, store.customers)),
        field('系统', os),
        field('位置', location),
        field('允许旧版 TLS', tls)),
    ],
    actions: [{ text: '取消' }, {
      text: '保存', kind: 'primary',
      onclick: async () => {
        const p = {};
        for (const [k, el] of Object.entries({ customer, os, location })) {
          if (el.value.trim()) p[k] = el.value.trim();
        }
        if (type.value) p.type = type.value;
        if (brand.changed()) p.vendor = brand.value();
        if (tls.value) p.legacy_tls = tls.value === 'on';
        account.check();
        account.patch(p);
        if (!Object.keys(p).length) throw new Error('没有要修改的项');
        await api.post('/api/devices/update', { ids, patch: p });
        toast(`已修改 ${ids.length} 台设备`, 'ok');
        loadDevices();
      },
    }],
  });
}

// ---------- 导入 ----------

const formatNames = { table: '设备表', bmckit: 'BMCKit 发现结果', list: '地址列表' };

function importDialog() {
  const fileInput = h('input', { type: 'file', accept: '.xlsx,.csv,.tsv,.txt', hidden: true });
  const drop = h('div', { class: 'drop', tabindex: '0', role: 'button', 'aria-label': '选择要导入的文件' },
    icon('import'), h('strong', { text: '把文件拖到这里，或点击选择文件' }), h('span', { text: 'Excel（.xlsx）、CSV、TXT，最大 20 MB' }));
  const paste = h('textarea', {
    class: 'input', rows: 4, spellcheck: 'false',
    placeholder: '也可以直接粘贴：从 Excel 复制的整张表（含表头），或每行一个管理口 IP',
  });
  const dlg = dialog({
    title: '导入设备',
    wide: true,
    body: [
      notice('info',
        h('p', { text: '认得本工具导出的表格和模板、自己整理的设备表（“管理口 IP”“BMC IP”“带外 IP”等常见列名都认得），以及 BMCKit 导出的发现结果 CSV。' }),
        h('p', { text: '旧版 Excel（.xls）请先另存为 .xlsx。导入前会先预览，确认后才写入。' })),
      drop, fileInput,
      field('粘贴内容', paste),
      h('p', { class: 'dialog-text muted' }, '没有现成的表？下载',
        h('a', { href: '/api/devices/template?format=xlsx', download: '' }, '导入模板（Excel）'), '或',
        h('a', { href: '/api/devices/template?format=csv', download: '' }, 'CSV 模板'), '，填好再导入。'),
    ],
    actions: [{ text: '取消' }, {
      text: '识别粘贴的内容', kind: 'primary',
      onclick: async () => {
        if (!paste.value.trim()) throw new Error('请先粘贴内容，或选择文件');
        const res = await api.post('/api/devices/import', { text: paste.value });
        dlg.close();
        previewDialog(res, '粘贴的内容');
        return false;
      },
    }],
  });
  const upload = async (file) => {
    const form = new FormData();
    form.append('file', file);
    try {
      const res = await api.upload('/api/devices/import', form);
      dlg.close();
      previewDialog(res, file.name);
    } catch (err) {
      dlg.error(err);
    }
  };
  fileInput.addEventListener('change', () => fileInput.files[0] && upload(fileInput.files[0]));
  drop.addEventListener('click', () => fileInput.click());
  drop.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      fileInput.click();
    }
  });
  drop.addEventListener('dragover', (e) => { e.preventDefault(); drop.classList.add('over'); });
  drop.addEventListener('dragleave', () => drop.classList.remove('over'));
  drop.addEventListener('drop', (e) => {
    e.preventDefault();
    drop.classList.remove('over');
    const file = e.dataTransfer.files[0];
    if (file) upload(file);
  });
}

const MAX_PREVIEW = 300;

function previewDialog(res, source) {
  const p = res.preview;
  const dup = p.rows.filter((r) => r.duplicate && !r.error).length;
  const bad = p.rows.filter((r) => r.error).length;
  const needAccount = p.rows.some((r) => !r.error && !r.duplicate && !r.device.user && !r.credential);
  const needCustomer = p.rows.some((r) => !r.error && !r.duplicate && !r.device.customer);
  const customer = input({ maxlength: 100, placeholder: needCustomer ? '表格里没写客户的设备用这个' : '表格里都写了客户' });
  const cred = select([['', '不使用'], ...store.credentials.map((c) => [c.id, `${c.name}（${c.user}）`])], '');

  const accountOf = (r) => {
    if (r.credential) return `模板：${r.credential}`;
    const d = r.device;
    if (!d.user) return '';
    return d.user + (d.has_password ? ' · 有密码' : ' · 无密码');
  };
  const result = (r) => {
    if (r.error) return [tag('有问题', 'red'), ' ', h('span', { class: 'text-danger', text: r.error })];
    if (r.duplicate) return tag('已存在，跳过', 'gray');
    return tag('可导入', 'green');
  };
  // 客户名称不单独占一列，表格里写了客户时可以按客户筛选。
  const rowBody = h('tbody');
  let filterBy = '';
  const filter = customerFilter((v) => { filterBy = v; renderRows(); });
  const tableCustomers = p.rows.map((r) => r.device.customer);
  filter.update(tableCustomers, '');
  const shownInfo = h('span', { class: 'muted' });
  function renderRows() {
    const matched = p.rows.filter((r) => matchCustomer(r.device.customer, filterBy));
    const rows = matched.slice(0, MAX_PREVIEW);
    rowBody.replaceChildren(...(rows.length ? rows.map((r) => h('tr', {},
      h('td', { class: 'faint', text: r.line }),
      h('td', { text: r.device.mgmt_ip || '' }),
      h('td', { text: r.device.host_ip || '' }),
      h('td', { text: r.device.vendor || '' }),
      h('td', { text: r.device.model || '' }),
      h('td', { text: r.device.serial || '' }),
      h('td', { text: accountOf(r) }),
      h('td', {}, result(r)))) : [h('tr', {}, h('td', { class: 'cell-empty', colspan: 8, text: '没有这个客户的设备' }))]));
    shownInfo.textContent = matched.length > MAX_PREVIEW
      ? `只显示前 ${MAX_PREVIEW} 行，共 ${matched.length} 行`
      : filterBy ? `显示 ${matched.length} 行，共 ${p.rows.length} 行` : `共 ${p.rows.length} 行`;
  }
  renderRows();

  dialog({
    title: '确认导入',
    wide: true,
    body: [
      h('dl', { class: 'kv' },
        h('dt', { text: '来源' }), h('dd', { text: `${source}（${formatNames[p.format] || p.format}）` }),
        h('dt', { text: '认出的列' }), h('dd', { text: p.columns.join('、') || '—' }),
        p.ignored?.length ? [h('dt', { text: '忽略的列' }), h('dd', { class: 'muted', text: p.ignored.join('、') })] : null),
      ...(p.warnings || []).map((w) => notice('warn', h('p', { text: w }))),
      h('div', { class: 'stats' },
        stat(res.ready, '台可导入'), stat(dup, '台已存在（跳过）'), stat(bad, '行有问题（跳过）')),
      h('div', { class: 'toolbar' },
        tableCustomers.some(Boolean) && filter.el, shownInfo),
      h('div', { class: 'table-wrap short bordered' },
        h('table', { class: 'grid' },
          h('thead', {}, h('tr', {}, ['行', '管理口 IP', '主机 IP', '品牌', '型号', '序列号', '账号', '结果'].map((t) => h('th', { text: t })))),
          rowBody)),
      h('div', { class: 'form' },
        field('默认客户名称', withList(customer, store.customers), '只用于表格里没写客户的设备。'),
        field('默认凭据模板', cred, needAccount
          ? '有设备没写账号，可以选一个模板；不选的话，收集前要逐台补上账号密码。'
          : '只用于表格里没写账号的设备。')),
    ],
    actions: [
      { text: '重新选择', icon: 'import', left: true, onclick: () => { setTimeout(importDialog); } },
      { text: '取消' },
      {
        text: res.ready ? `导入 ${res.ready} 台` : '没有可导入的设备', kind: 'primary', disabled: !res.ready,
        onclick: async () => {
          const out = await api.post('/api/devices/import/commit', { token: res.token, default_customer: customer.value.trim(), credential_id: cred.value });
          toast(`已导入 ${out.added} 台设备`, 'ok');
          selectNew(out.ids);
        },
      },
    ],
  });
}

function stat(value, label) {
  return h('div', { class: 'stat' }, h('div', { class: 'stat-value', text: value }), h('div', { class: 'stat-label', text: label }));
}

// ---------- 凭据模板 ----------

function credentialsDialog() {
  const wrap = h('div');
  const render = () => {
    if (!store.credentials.length) {
      wrap.replaceChildren(empty('key', '还没有凭据模板', '一批设备账号密码相同时，建一个模板，添加或导入设备时选它就行。'));
      return;
    }
    wrap.replaceChildren(h('div', { class: 'table-wrap short bordered' },
      h('table', { class: 'grid' },
        h('thead', {}, h('tr', {}, ['名称', '账号', '密码', '使用的设备', '备注', ''].map((t) => h('th', { text: t })))),
        h('tbody', {}, store.credentials.map((c) => h('tr', {},
          h('td', { text: c.name }),
          h('td', { text: c.user }),
          h('td', {}, c.has_password ? '已保存' : h('span', { class: 'faint', text: '未填' })),
          h('td', { text: `${c.used_by} 台` }),
          h('td', {}, h('span', { class: 'ellipsis', title: c.notes, text: c.notes })),
          h('td', { class: 'cell-actions' },
            button('', { kind: 'ghost', size: 'small', icon: 'edit', title: '编辑', 'aria-label': '编辑', onclick: () => credentialEdit(c) }),
            button('', { kind: 'ghost', size: 'small', icon: 'trash', title: '删除', 'aria-label': '删除', onclick: () => credentialDelete(c) }))))))));
  };
  const off = bus.on('changed:devices', render);
  const dlg = dialog({
    title: '凭据模板',
    wide: true,
    body: [
      notice('info', h('p', { text: `一组账号密码存成模板，多台设备共用；改模板里的密码，用它的设备都会跟着改。密码用${store.state?.secret_method || '本机密钥'}加密保存在这台电脑上。` })),
      wrap,
    ],
    actions: [
      { text: '新建模板', icon: 'plus', left: true, onclick: () => { credentialEdit(null); return false; } },
      { text: '关闭', kind: 'primary' },
    ],
  });
  dlg.done.then(off);
  render();
}

function credentialEdit(c) {
  const name = input({ value: c?.name || '', placeholder: '例如 华为默认账号', maxlength: 100 });
  const user = input({ value: c?.user || '', placeholder: '例如 Administrator', maxlength: 100 });
  const password = input({ type: 'password', autocomplete: 'new-password', maxlength: 256, placeholder: c?.has_password ? '已保存，留空表示不修改' : '' });
  const notes = h('textarea', { class: 'input', rows: 2, value: c?.notes || '', maxlength: 2000 });
  dialog({
    title: c ? `编辑模板“${c.name}”` : '新建凭据模板',
    body: [
      c?.used_by ? notice('warn', h('p', { text: `有 ${c.used_by} 台设备在用这个模板，修改账号或密码后它们都会改用新的。` })) : null,
      h('div', { class: 'form' },
        field('名称', name, null, 'span-2'),
        field('账号', user), field('密码', password),
        field('备注', notes, null, 'span-2')),
    ],
    actions: [{ text: '取消' }, {
      text: c ? '保存' : '新建', kind: 'primary',
      onclick: async () => {
        const p = { name: name.value.trim(), user: user.value.trim(), notes: notes.value.trim() };
        if (password.value) p.password = password.value;
        if (c) await api.post(`/api/credentials/${c.id}`, p);
        else await api.post('/api/credentials', p);
        toast(c ? '已保存模板' : '已新建模板', 'ok');
        loadDevices();
      },
    }],
  });
  name.focus();
}

async function credentialDelete(c) {
  const ok = await confirm({
    title: `删除模板“${c.name}”？`,
    message: c.used_by ? `有 ${c.used_by} 台设备在用这个模板，删除后这些设备要重新设置账号才能收集。` : '没有设备在用这个模板。',
    ok: '删除',
    danger: true,
  });
  if (!ok) return;
  try {
    const res = await api.post(`/api/credentials/${c.id}/delete`);
    toast(res.unlinked ? `已删除模板，${res.unlinked} 台设备需要重新设置账号` : '已删除模板', 'ok');
    loadDevices();
  } catch (err) {
    showError(err);
  }
}
