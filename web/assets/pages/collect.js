// SPDX-License-Identifier: GPL-3.0-or-later

// 日志收集：选设备开始收集，查看每台设备的进度、结果和失败原因。

import {
  h, icon, button, tag, notice, api, bus, confirm, toast, showError, download, fmtTime, fmtDuration, fmtSize,
  input, select, frame, empty, pageHead, keyedRows, setChildren, customerFilter, matchCustomer,
} from '../lib.js';
import { store, loadTasks, collectingIDs, deviceStatus, support, deviceLabel, logTypes } from '../store.js';
import { startCollect, openPath } from '../actions.js';

const view = { search: '', customer: '', selected: new Set(), filter: 'all' };

const filters = [['all', '全部任务'], ['active', '进行中和排队'], ['failed', '失败和取消'], ['done', '已完成']];

// ---------- 任务显示 ----------

function stateTag(t) {
  switch (t.state) {
    case 'queued': return tag('排队中', 'gray');
    case 'running': return tag('收集中', 'blue');
    case 'done': return tag('完成', 'green');
    case 'failed': return tag(t.kind || '失败', 'red');
    default: return tag('已取消', 'gray');
  }
}

// 各阶段在总进度条上占的区间：探测、触发、设备生成、下载、校验。
const spans = [[0, 0], [2, 8], [8, 15], [15, 75], [75, 95], [95, 99], [100, 100]];

function progress(t) {
  const [lo, hi] = spans[t.stage_index] || [0, 0];
  const generating = t.state === 'running' && t.stage_index === 3 && t.percent < 0;
  const value = t.state === 'done' ? 100 : t.percent >= 0 ? lo + ((hi - lo) * t.percent) / 100 : lo;
  const cls = ['progress', generating && 'indeterminate', t.state === 'failed' && 'failed', t.state === 'canceled' && 'canceled'];
  const bar = h('div', { class: 'progress-bar' });
  if (!generating) bar.style.width = `${Math.max(value, t.state === 'running' ? 2 : 0)}%`;
  const label = t.state === 'running' || t.state === 'queued'
    ? `${t.stage}${t.percent >= 0 ? ` ${t.percent}%` : ''}`
    : t.state === 'done' ? '完成' : `停在“${t.stage}”`;
  return h('div', { class: 'stage' },
    h('div', { class: 'stage-label', text: label }),
    h('div', { class: cls.filter(Boolean).join(' '), role: 'progressbar', 'aria-valuemin': 0, 'aria-valuemax': 100, 'aria-valuenow': generating ? null : Math.round(value) }, bar));
}

function describe(t) {
  if (t.state === 'done' && t.result) {
    const r = t.result;
    return h('div', {},
      h('div', { text: logTypes[r.log_type] || r.log_type }),
      h('div', { class: 'muted small' }, `${r.file} · ${fmtSize(r.size)}`));
  }
  if (t.state === 'failed') {
    // 先说该怎么做；设备或接口返回的原始错误收在“详细信息”里，反馈问题时用得上。
    if (!t.hint) return h('div', { text: t.error || t.message });
    return h('div', {},
      h('div', { class: 'task-hint' }, icon('info'), h('span', { text: t.hint })),
      t.error && h('details', { class: 'task-detail' }, h('summary', { text: '详细信息' }), h('div', { text: t.error })));
  }
  return h('div', { class: t.state === 'canceled' ? 'muted' : '', text: t.message });
}

function elapsed(t) {
  if (!t.started_at) return '';
  const end = t.finished_at ? new Date(t.finished_at) : new Date();
  return fmtDuration(end - new Date(t.started_at));
}

function taskActions(t) {
  const out = [];
  if (t.state === 'queued' || t.state === 'running') {
    out.push(button('取消', {
      kind: 'ghost', size: 'small', icon: 'stop', title: '只停止本机等待；设备上已开始生成的日志包会继续完成',
      onclick: () => api.post(`/api/tasks/${t.id}/cancel`).catch(showError),
    }));
  }
  if (t.state === 'done' && t.result) {
    out.push(button('打开', { kind: 'ghost', size: 'small', icon: 'folder', title: '在资源管理器里打开', onclick: () => openPath('path', t.result.path) }));
  }
  if (t.state === 'failed' || t.state === 'canceled') {
    out.push(button('重试', { kind: 'ghost', size: 'small', icon: 'refresh', onclick: () => startCollect([t.device_id]) }));
  }
  if (t.has_record) {
    out.push(button('诊断记录', {
      kind: 'ghost', size: 'small', icon: 'record', title: '导出和设备的交互记录（已去掉密码、令牌和设备 IP），反馈问题时附上',
      onclick: () => download(`/api/tasks/${t.id}/record`),
    }));
  }
  return h('div', { class: 'row-actions' }, out);
}

function stat(value, label, tone) {
  return h('div', { class: `stat${tone ? ` stat-${tone}` : ''}` }, h('div', { class: 'stat-value', text: value }), h('div', { class: 'stat-label', text: label }));
}

// ---------- 页面 ----------

export function mount(root) {
  const pickCache = new Map();
  const taskCache = new Map();
  // 选择设备
  let pickable = [];
  const search = input({
    type: 'search', placeholder: '搜索客户、IP、型号…', value: view.search, 'aria-label': '搜索设备',
    oninput: () => { view.search = search.value; render(); },
  });
  const customer = customerFilter((v) => { view.customer = v; render(); });
  const selInfo = h('span', { class: 'selection-info' });
  const btnStart = button('开始收集', {
    kind: 'primary', icon: 'play',
    onclick: async () => {
      const ids = [...view.selected];
      if (!ids.length) return;
      const started = await startCollect(ids);
      if (started?.length) {
        view.selected.clear();
        render();
      }
    },
  });
  const headCheck = h('input', {
    type: 'checkbox', 'aria-label': '全选',
    onchange: () => {
      const all = pickable.length > 0 && pickable.every((d) => view.selected.has(d.id));
      for (const d of pickable) {
        if (all) view.selected.delete(d.id);
        else view.selected.add(d.id);
      }
      render();
    },
  });
  const pickBody = h('tbody');
  const pickCard = h('section', { class: 'card card-flush' },
    h('div', { class: 'toolbar' },
      h('h2', { class: 'toolbar-title', text: '选择设备' }),
      h('div', { class: 'search' }, icon('search'), search), customer.el,
      h('div', { class: 'spacer' }), selInfo, btnStart),
    h('div', { class: 'table-wrap short' },
      h('table', { class: 'grid' },
        h('thead', {}, h('tr', {}, h('th', { class: 'cell-check' }, headCheck),
          ['管理口 IP', '品牌', '型号', '序列号', '账号', '连通状态', '一键收集', '最后收集'].map((t) => h('th', { text: t })))),
        pickBody)),
    h('div', { class: 'table-foot' },
      h('span', {}, `已支持一键收集：服务器 ${store.state?.supported || '华为、超聚变、浪潮'}，网络设备 ${store.state?.supported_net || '思科、华为、新华三'}。联想、Dell、HPE 服务器计划 v0.1，新华三、IBM 服务器计划 v0.2；存储 v0.2。`),
      h('span', { class: 'spacer' }),
      h('a', { href: '#/devices', text: '管理设备' })));
  const noDevices = h('section', { class: 'card' }, empty('devices', '还没有设备', '先在设备列表里添加设备，填写管理口 IP 和账号。',
    button('去添加设备', { kind: 'primary', icon: 'plus', onclick: () => { location.hash = '#/devices'; } })));

  pickBody.addEventListener('click', (e) => {
    const tr = e.target.closest('tr[data-id]');
    if (!tr || tr.classList.contains('disabled') || e.target.closest('a, button')) return;
    const id = tr.dataset.id;
    if (view.selected.has(id)) view.selected.delete(id);
    else view.selected.add(id);
    render();
  });

  // 收集任务
  const stats = h('div', { class: 'stats' });
  const filter = select(filters, view.filter, { 'aria-label': '筛选任务', onchange: () => { view.filter = filter.value; render(); } });
  const btnCancelAll = button('全部取消', {
    kind: 'danger', icon: 'stop',
    onclick: async () => {
      const n = store.tasks.filter((t) => t.state === 'queued' || t.state === 'running').length;
      const ok = await confirm({
        title: `取消全部 ${n} 个任务？`,
        message: '只停止本机的等待和下载；设备上已开始生成的日志包会继续完成，登录会话照常退出。',
        ok: '全部取消',
        danger: true,
      });
      if (ok) api.post('/api/tasks/cancel-all').catch(showError);
    },
  });
  const btnClear = button('清除已结束', {
    icon: 'trash', title: '从列表里移除已结束的任务，不会删除已收集的文件',
    onclick: async () => {
      try {
        await api.post('/api/tasks/clear');
        await loadTasks();
      } catch (err) {
        showError(err);
      }
    },
  });
  const taskBody = h('tbody');
  const taskTable = h('div', { class: 'table-wrap' },
    h('table', { class: 'grid' },
      h('thead', {}, h('tr', {}, ['设备', '状态', '进度', '说明', '开始时间', '用时', ''].map((t) => h('th', { text: t })))),
      taskBody));
  const noTasks = empty('collect', '还没有收集任务', '在上面勾选设备，点“开始收集”。任务列表只保留在本次运行中，收集到的文件在“收集结果”里。');
  const taskCard = h('section', { class: 'card card-flush' },
    h('div', { class: 'toolbar' },
      h('h2', { class: 'toolbar-title', text: '收集任务' }), filter,
      h('div', { class: 'spacer' }), btnClear, btnCancelAll),
    taskTable, noTasks,
    h('div', { class: 'table-foot' },
      h('span', { text: '收集只读取日志，不会清除日志或重启设备。认证失败会立即停止、不重试，避免账号被锁定。' })));

  root.append(
    pageHead('日志收集', '对选中的设备一键收集日志包，每台设备的进度、结果和失败原因都显示在这里。',
      button('打开保存位置', { icon: 'folder', onclick: () => openPath('output') })),
    stats, pickCard, noDevices, taskCard);
  // 有任务时任务列表排在前面，没有任务时先选设备。
  const order = () => {
    const first = store.tasks.length ? taskCard : pickCard;
    if (stats.nextElementSibling !== first) stats.after(first);
  };

  const render = frame(() => {
    order();
    // 设备
    const collecting = collectingIDs();
    const ids = new Set(store.devices.map((d) => d.id));
    for (const id of view.selected) if (!ids.has(id) || collecting.has(id)) view.selected.delete(id);
    pickCard.hidden = store.devices.length === 0;
    noDevices.hidden = store.devices.length > 0;
    view.customer = customer.update(store.devices.map((d) => d.customer), view.customer);
    const q = view.search.trim().toLowerCase();
    const devices = store.devices
      .filter((d) => matchCustomer(d.customer, view.customer))
      .filter((d) => !q || [d.customer, d.mgmt_ip, d.vendor, d.model, d.serial, d.hostname].some((v) => v && v.toLowerCase().includes(q)))
      .sort((a, b) => (a.customer || '').localeCompare(b.customer || '', 'zh-CN') || (a.mgmt_ip || '').localeCompare(b.mgmt_ip || '', 'zh-CN', { numeric: true }));
    pickable = devices.filter((d) => !support(d).blocked && !collecting.has(d.id) && d.mgmt_ip);
    const pickRow = (d) => {
      const s = support(d);
      const busy = collecting.has(d.id);
      const disabled = s.blocked || busy || !d.mgmt_ip;
      const sel = view.selected.has(d.id);
      const st = deviceStatus(d, busy);
      const account = d.credential_id ? '凭据模板' : d.user ? (d.has_password ? d.user : `${d.user}（未填密码）`) : '未填';
      return h('tr', {
        class: [disabled ? 'disabled' : 'clickable', sel && 'selected'].filter(Boolean).join(' '),
        dataset: { id: d.id },
        title: disabled ? (busy ? '正在收集' : !d.mgmt_ip ? '没有填写管理口 IP' : s.reason) : null,
      },
      h('td', { class: 'cell-check' }, h('input', { type: 'checkbox', checked: sel, disabled, 'aria-label': `选择 ${deviceLabel(d)}` })),
      h('td', { text: d.mgmt_ip || '' }),
      h('td', { text: d.vendor || '' }),
      h('td', { text: d.model || '' }),
      h('td', { text: d.serial || '' }),
      h('td', { class: account === '未填' || account.includes('未填') ? 'text-danger' : '', text: account }),
      h('td', {}, tag(st.text, st.tone)),
      h('td', {}, busy ? tag('收集中', 'blue') : tag(s.text, s.tone, s.reason || null)),
      h('td', {}, d.last_collect ? h('span', { class: d.last_collect.ok ? '' : 'text-danger', title: d.last_collect.message },
        fmtTime(d.last_collect.at), d.last_collect.ok ? '' : ' 失败') : ''));
    };
    setChildren(pickBody, devices.length
      ? keyedRows(pickCache, devices, (d) => d.id, (d) => JSON.stringify([d, view.selected.has(d.id), collecting.has(d.id), store.state?.supported]), pickRow)
      : [h('tr', {}, h('td', { class: 'cell-empty', colspan: 9, text: '没有符合条件的设备' }))]);
    const nSel = view.selected.size;
    selInfo.textContent = nSel ? `已选 ${nSel} 台` : '';
    btnStart.disabled = nSel === 0;
    const nPick = pickable.filter((d) => view.selected.has(d.id)).length;
    headCheck.checked = nPick > 0 && nPick === pickable.length;
    headCheck.indeterminate = nPick > 0 && nPick < pickable.length;
    headCheck.disabled = pickable.length === 0;

    // 任务
    const count = (...states) => store.tasks.filter((t) => states.includes(t.state)).length;
    const running = count('running');
    const queued = count('queued');
    stats.replaceChildren(
      stat(running, '正在收集', running ? 'blue' : ''),
      stat(queued, '排队中'),
      stat(count('done'), '已完成', count('done') ? 'green' : ''),
      stat(count('failed', 'canceled'), '失败或取消', count('failed') ? 'red' : ''));
    btnCancelAll.disabled = running + queued === 0;
    btnClear.disabled = count('done', 'failed', 'canceled') === 0;
    const match = {
      all: () => true,
      active: (t) => t.state === 'queued' || t.state === 'running',
      failed: (t) => t.state === 'failed' || t.state === 'canceled',
      done: (t) => t.state === 'done',
    }[view.filter];
    const tasks = store.tasks.filter(match);
    taskTable.hidden = store.tasks.length === 0;
    noTasks.hidden = store.tasks.length > 0;
    const taskRow = (t) => h('tr', { class: t.state === 'failed' ? 'row-failed' : '' },
      h('td', { class: 'cell-device' },
        h('div', {}, t.customer && h('strong', { text: t.customer }), t.customer ? ' ' : '', t.mgmt_ip),
        h('div', { class: 'muted small', text: [t.vendor, t.model, t.serial].filter(Boolean).join(' · ') || '型号待识别' })),
      h('td', {}, stateTag(t)),
      h('td', {}, progress(t)),
      h('td', { class: 'wrap' }, describe(t)),
      h('td', { class: 'muted', text: fmtTime(t.started_at || t.created_at) }),
      h('td', { class: 'muted cell-elapsed', text: elapsed(t) }),
      h('td', { class: 'cell-actions' }, taskActions(t)));
    setChildren(taskBody, tasks.length
      ? keyedRows(taskCache, tasks, (t) => t.id, (t) => JSON.stringify(t), taskRow)
      : [h('tr', {}, h('td', { class: 'cell-empty', colspan: 7, text: '没有符合条件的任务' }))]);
  });

  // 进行中的任务每秒更新一次用时（只改这一格，不重画整行）。
  const timer = setInterval(() => {
    for (const t of store.tasks) {
      if (t.state !== 'running') continue;
      const cell = taskCache.get(t.id)?.el.querySelector('.cell-elapsed');
      if (cell) cell.textContent = elapsed(t);
    }
  }, 1000);
  const offs = [bus.on('changed:devices', render), bus.on('changed:tasks', render)];
  render();
  return () => {
    clearInterval(timer);
    offs.forEach((off) => off());
  };
}
