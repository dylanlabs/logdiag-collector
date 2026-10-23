// SPDX-License-Identifier: GPL-3.0-or-later

// 各页面共用的小工具：建 DOM、调用本机接口、对话框、提示、格式化、图标。

// h 创建元素：h('button', { class: 'btn', onclick: fn }, '文字', child)。
// 属性里 class、text、style（对象）、dataset（对象）特殊处理；on 开头的是事件；值为 false/null 的属性忽略。
export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v === null || v === undefined) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k === 'style') Object.assign(el.style, v);
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'value' || k === 'checked' || k === 'disabled' || k === 'selected' || k === 'indeterminate') el[k] = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue;
    if (Array.isArray(c)) append(el, c);
    else el.append(c instanceof Node ? c : String(c));
  }
}

export const $ = (id) => document.getElementById(id);

// ---------- 图标（24×24 线条图标，颜色跟随文字） ----------

const paths = {
  devices: 'M4 4.5h16v6H4zM4 13.5h16v6H4zM7.5 7.5h.01M7.5 16.5h.01M11 7.5h5M11 16.5h5',
  collect: 'M12 3.5v11M7.5 10l4.5 4.5 4.5-4.5M4.5 15.5v4h15v-4',
  results: 'M3.5 6.5a1.5 1.5 0 0 1 1.5-1.5h4.2l2 2.2H19a1.5 1.5 0 0 1 1.5 1.5v9.3a1.5 1.5 0 0 1-1.5 1.5H5a1.5 1.5 0 0 1-1.5-1.5z',
  health: 'M3.5 12h4l2.2-5.5 4.3 11 2.3-5.5h4.2',
  os: 'M4 5h16v14H4zM7.5 9.5l3 2.5-3 2.5M12.5 15h4',
  settings: 'M12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6zM19.4 13.5l1.6 1.2-1.8 3.1-1.9-.7a7.4 7.4 0 0 1-1.8 1l-.3 2h-3.6l-.3-2a7.4 7.4 0 0 1-1.8-1l-1.9.7-1.8-3.1 1.6-1.2a7.6 7.6 0 0 1 0-3L3 9.3l1.8-3.1 1.9.7a7.4 7.4 0 0 1 1.8-1l.3-2h3.6l.3 2a7.4 7.4 0 0 1 1.8 1l1.9-.7 1.8 3.1-1.6 1.2a7.6 7.6 0 0 1 0 3z',
  update: 'M20 12a8 8 0 1 1-2.34-5.66M20 4v5h-5',
  language: 'M3.5 12h17M12 3.5c2.5 2.6 3.6 5.4 3.6 8.5s-1.1 5.9-3.6 8.5c-2.5-2.6-3.6-5.4-3.6-8.5S9.5 6.1 12 3.5zM12 3.5a8.5 8.5 0 1 0 0 17 8.5 8.5 0 0 0 0-17z',
  feedback: 'M4.5 5.5h15v10h-8l-4.5 3.5v-3.5h-2.5z',
  open: 'M14 4.5h5.5V10M19.5 4.5l-8 8M17 13.5v5a1 1 0 0 1-1 1H5.5a1 1 0 0 1-1-1V8a1 1 0 0 1 1-1h5',
  plus: 'M12 5v14M5 12h14',
  import: 'M12 14.5V3.5M7.5 10l4.5 4.5 4.5-4.5M4.5 15.5v4h15v-4',
  export: 'M12 3.5v11M7.5 8 12 3.5 16.5 8M4.5 15.5v4h15v-4',
  search: 'M10.5 4a6.5 6.5 0 1 0 0 13 6.5 6.5 0 0 0 0-13zM15.5 15.5 20 20',
  refresh: 'M19.5 12a7.5 7.5 0 1 1-2.2-5.3M19.5 4.5v4.2h-4.2',
  trash: 'M4.5 6.5h15M9.5 6.5V4.5h5v2M6.5 6.5l1 13h9l1-13M10 10v6M14 10v6',
  edit: 'M4.5 19.5h4l10-10-4-4-10 10zM13 7l4 4',
  key: 'M14.5 4a5.5 5.5 0 0 0-5.2 7.3L3.5 17v3.5H7v-2h2v-2h2l1.7-1.8A5.5 5.5 0 1 0 14.5 4zM16 8h.01',
  play: 'M7 4.5v15l12-7.5z',
  stop: 'M6.5 6.5h11v11h-11z',
  folder: 'M3.5 6.5a1.5 1.5 0 0 1 1.5-1.5h4.2l2 2.2H19a1.5 1.5 0 0 1 1.5 1.5v9.3a1.5 1.5 0 0 1-1.5 1.5H5a1.5 1.5 0 0 1-1.5-1.5zM3.5 10h17',
  package: 'M12 3.5 20 7.5v9L12 20.5 4 16.5v-9zM4 7.5l8 4 8-4M12 11.5v9',
  info: 'M12 3.5a8.5 8.5 0 1 0 0 17 8.5 8.5 0 0 0 0-17zM12 11v5.5M12 7.5h.01',
  warning: 'M12 4 21 19.5H3zM12 10v4.5M12 17h.01',
  check: 'M5 12.5l4.5 4.5L19 7.5',
  close: 'M6 6l12 12M18 6 6 18',
  chevron: 'M7 10l5 5 5-5',
  scan: 'M4 8V5.5A1.5 1.5 0 0 1 5.5 4H8M16 4h2.5A1.5 1.5 0 0 1 20 5.5V8M20 16v2.5a1.5 1.5 0 0 1-1.5 1.5H16M8 20H5.5A1.5 1.5 0 0 1 4 18.5V16M4 12h16',
  identify: 'M12 3.5a8.5 8.5 0 1 0 0 17 8.5 8.5 0 0 0 0-17zM8 12.5l2.7 2.7L16.5 9.5',
  list: 'M8.5 6.5h11M8.5 12h11M8.5 17.5h11M4.5 6.5h.01M4.5 12h.01M4.5 17.5h.01',
  record: 'M6.5 3.5h7l4 4v13h-11zM13.5 3.5v4h4M9 12h6M9 15.5h6',
  upload: 'M12 14.5V3.5M7.5 8 12 3.5 16.5 8M4.5 15.5v4h15v-4',
  clock: 'M12 3.5a8.5 8.5 0 1 0 0 17 8.5 8.5 0 0 0 0-17zM12 7.5V12l3 2',
  shield: 'M12 3.5 19 6v5.5c0 4.3-3 7.6-7 9-4-1.4-7-4.7-7-9V6z',
};

export function icon(name, cls = 'icon') {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('class', cls);
  const p = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  p.setAttribute('d', paths[name] || paths.info);
  svg.append(p);
  return svg;
}

// ---------- 按钮、标签 ----------

// button('文字', { kind: 'primary'|'danger'|'ghost', size: 'small', icon: 'plus', onclick })
export function button(text, opts = {}) {
  const { kind = '', size = '', icon: ic, ...attrs } = opts;
  const cls = ['btn', kind && `btn-${kind}`, size && `btn-${size}`, !text && 'btn-icon', attrs.class].filter(Boolean).join(' ');
  return h('button', { type: 'button', ...attrs, class: cls }, ic && icon(ic), text && h('span', { text }));
}

export function tag(text, tone = 'gray', title) {
  return h('span', { class: `tag tag-${tone}`, title }, text);
}

// soon 是“即将支持”的标签，version 例如 'v0.1'、'1.0'。
export function soon(version) {
  return tag(version ? `即将支持 · ${version}` : '即将支持', 'blue');
}

export function notice(tone, ...children) {
  const ic = { info: 'info', warn: 'warning', soon: 'clock', ok: 'check', error: 'warning' }[tone] || 'info';
  return h('div', { class: `notice notice-${tone}` }, icon(ic, 'icon notice-icon'), h('div', { class: 'notice-body' }, ...children));
}

// ---------- 本机接口 ----------

async function request(method, path, body) {
  const opts = { method, headers: {} };
  if (body instanceof FormData) opts.body = body;
  else if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, opts);
  } catch {
    throw new Error('与本机服务的连接已断开；如果程序已退出，请重新运行。');
  }
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { /* 非 JSON */ }
  if (!res.ok) throw new Error((data && data.error) || `请求失败（${res.status}）`);
  return data;
}

export const api = {
  get: (path) => request('GET', path),
  post: (path, body = {}) => request('POST', path, body),
  upload: (path, form) => request('POST', path, form),
};

// download 让浏览器下载接口返回的文件（文件名由服务端给出）。
export function download(url) {
  const a = h('a', { href: url, download: '' });
  document.body.append(a);
  a.click();
  a.remove();
}

// ---------- 事件 ----------

const listeners = new Map();
export const bus = {
  on(name, fn) {
    if (!listeners.has(name)) listeners.set(name, new Set());
    listeners.get(name).add(fn);
    return () => listeners.get(name).delete(fn);
  },
  emit(name, data) {
    for (const fn of listeners.get(name) || []) fn(data);
  },
};

// ---------- 提示与对话框 ----------

export function toast(message, tone = 'info', ms = 4000) {
  let box = $('toasts');
  if (!box) {
    box = h('div', { id: 'toasts', class: 'toasts', role: 'status', 'aria-live': 'polite', popover: 'manual' });
    document.body.append(box);
  }
  // 提示放在顶层（popover），重新弹出一次，保证盖在打开的对话框上面。
  if (box.matches(':popover-open')) box.hidePopover();
  box.showPopover();
  const t = h('div', { class: `toast toast-${tone}` }, message);
  box.append(t);
  setTimeout(() => {
    t.classList.add('toast-out');
    setTimeout(() => {
      t.remove();
      if (!box.children.length && box.matches(':popover-open')) box.hidePopover();
    }, 300);
  }, ms);
}

// showError 用提示显示错误，返回 null，方便 catch 里直接用。
export function showError(err) {
  toast(err.message || String(err), 'error', 6000);
  return null;
}

// dialog 打开模态对话框。actions 是按钮说明 [{ text, kind, icon, onclick, disabled, left }]：
// onclick 可以是异步的，返回 false 时不关闭，返回其他值时关闭并作为结果；抛出的错误显示在对话框里。
// 没有 onclick 的按钮直接关闭。返回 { el, body, buttons, close, error, done }，done 是关闭时兑现的 Promise。
export function dialog({ title, body, actions = [], wide = false }) {
  const content = h('div', { class: 'dialog-body' }, body);
  const errBox = h('div', { class: 'dialog-error', role: 'alert', hidden: true });
  const foot = h('div', { class: 'dialog-foot' });
  let result;
  const close = (value) => {
    result = value;
    if (el.open) el.close();
  };
  const error = (err) => {
    if (!err) {
      errBox.hidden = true;
      return;
    }
    errBox.replaceChildren(notice('error', h('p', { text: err.message || String(err) })));
    errBox.hidden = false;
  };
  const el = h('dialog', { class: `dialog${wide ? ' dialog-wide' : ''}` },
    h('div', { class: 'dialog-head' },
      h('h2', { text: title }),
      button('', { kind: 'ghost', icon: 'close', title: '关闭', 'aria-label': '关闭', onclick: () => close() })),
    content, errBox, foot);
  let resolve;
  const done = new Promise((r) => { resolve = r; });
  el.addEventListener('close', () => {
    el.remove();
    resolve(result);
  });
  const buttons = actions.map((a) => {
    const b = button(a.text, {
      kind: a.kind, icon: a.icon, disabled: a.disabled, class: a.left ? 'push-left' : '',
      onclick: async () => {
        if (!a.onclick) return close();
        b.disabled = true;
        error(null);
        try {
          const r = await a.onclick();
          if (r !== false) close(r);
        } catch (err) {
          error(err);
        } finally {
          b.disabled = false;
        }
      },
    });
    foot.append(b);
    return b;
  });
  if (!actions.length) foot.remove();
  document.body.append(el);
  el.showModal();
  return { el, body: content, buttons, close, error, done };
}

// confirm 询问用户，确认返回 true。修改类操作先确认。
export function confirm({ title, message, ok = '确定', danger = false }) {
  return dialog({
    title,
    body: typeof message === 'string' ? h('p', { class: 'dialog-text', text: message }) : message,
    actions: [{ text: '取消' }, { text: ok, kind: danger ? 'danger' : 'primary', onclick: () => true }],
  }).done.then((v) => v === true);
}

// ---------- 格式化 ----------

const pad = (n) => String(n).padStart(2, '0');

export function fmtTime(iso, withSeconds = false) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const now = new Date();
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}${withSeconds ? ':' + pad(d.getSeconds()) : ''}`;
  if (d.toDateString() === now.toDateString()) return `今天 ${time}`;
  const y = d.getFullYear() === now.getFullYear() ? '' : `${d.getFullYear()}-`;
  return `${y}${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${time}`;
}

export function fmtDuration(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s} 秒`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} 分 ${pad(s % 60)} 秒`;
  return `${Math.floor(m / 60)} 小时 ${pad(m % 60)} 分`;
}

export function fmtSize(bytes) {
  if (!bytes && bytes !== 0) return '';
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = bytes / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}

// field 是带标签的表单项。
export function field(label, control, hint, cls = '') {
  return h('label', { class: `field ${cls}` },
    h('span', { class: 'field-label', text: label }),
    control,
    hint && h('span', { class: 'field-hint' }, hint));
}

export function input(attrs = {}) {
  return h('input', { class: 'input', type: 'text', autocomplete: 'off', spellcheck: 'false', ...attrs });
}

export function select(options, value, attrs = {}) {
  return h('select', { class: 'input', ...attrs },
    options.map(([v, label]) => h('option', { value: v, selected: v === value }, label)));
}

export function checkbox(label, checked, attrs = {}) {
  return h('label', { class: 'check' }, h('input', { type: 'checkbox', checked, ...attrs }), h('span', { text: label }));
}

// datalist 给输入框提供候选值（例如已有的客户名称）。
let listSeq = 0;
export function withList(inputEl, values) {
  const id = `list-${++listSeq}`;
  inputEl.setAttribute('list', id);
  return [inputEl, h('datalist', { id }, values.map((v) => h('option', { value: v })))];
}

// throttle 把短时间内的多次调用合并成一次（下一帧执行）。
export function frame(fn) {
  let pending = false;
  return () => {
    if (pending) return;
    pending = true;
    requestAnimationFrame(() => {
      pending = false;
      fn();
    });
  };
}

// empty 是空列表时的提示。
export function empty(ic, title, text, ...actions) {
  return h('div', { class: 'empty' },
    icon(ic, 'icon empty-icon'),
    h('div', { class: 'empty-title', text: title }),
    text && h('div', { class: 'empty-text' }, text),
    actions.length > 0 && h('div', { class: 'empty-actions' }, actions));
}

// pageHead 是页面标题行：标题、说明和右侧按钮。
export function pageHead(title, desc, ...actions) {
  return h('header', { class: 'page-head' },
    h('div', { class: 'page-title' }, h('h1', { text: title }), desc && h('p', { class: 'page-desc' }, desc)),
    actions.length > 0 && h('div', { class: 'page-actions' }, actions));
}

export function card(title, ...children) {
  return h('section', { class: 'card' }, title && h('h2', { class: 'card-title' }, title), ...children);
}

// openMenu 在 anchor 旁弹出菜单：items 为 [{ text, icon, onclick, disabled, badge, checked }]，'-' 是分隔线。
// 下方放不下时向上弹出。
export function openMenu(anchor, items) {
  document.querySelector('.menu')?.remove();
  const m = h('div', { class: 'menu', role: 'menu' },
    items.map((it) => it === '-' ? h('div', { class: 'menu-sep' }) : h('button', {
      type: 'button', class: 'menu-item', role: 'menuitem', disabled: it.disabled,
      onclick: () => { close(); it.onclick?.(); },
    }, it.checked || it.icon ? icon(it.checked ? 'check' : it.icon) : h('span', { class: 'icon' }), h('span', { text: it.text }), it.badge)));
  document.body.append(m);
  const r = anchor.getBoundingClientRect();
  m.style.left = `${Math.max(8, Math.min(r.left, window.innerWidth - m.offsetWidth - 8))}px`;
  const below = r.bottom + 4 + m.offsetHeight <= window.innerHeight - 8;
  m.style.top = `${below ? r.bottom + 4 : Math.max(8, r.top - 4 - m.offsetHeight)}px`;
  const off = (ev) => {
    if (ev.type === 'keydown' ? ev.key === 'Escape' : !m.contains(ev.target) && !anchor.contains(ev.target)) close();
  };
  function close() {
    m.remove();
    document.removeEventListener('mousedown', off);
    document.removeEventListener('keydown', off);
    window.removeEventListener('blur', close);
  }
  setTimeout(() => {
    document.addEventListener('mousedown', off);
    document.addEventListener('keydown', off);
    window.addEventListener('blur', close);
  });
  m.querySelector('.menu-item:not(:disabled)')?.focus();
  return close;
}

// menuButton 是点击后弹出菜单的按钮，items 同 openMenu。
export function menuButton(text, opts, items) {
  const btn = button(text, { ...opts, 'aria-haspopup': 'menu' });
  btn.append(icon('chevron', 'icon icon-chevron'));
  btn.addEventListener('click', (e) => {
    e.stopPropagation();
    if (document.querySelector('.menu')) {
      document.querySelector('.menu').remove();
      return;
    }
    openMenu(btn, typeof items === 'function' ? items() : items);
  });
  return btn;
}

// keyedRows 按键复用表格行：签名没变的行直接用上次的元素。列表频繁刷新（例如收集进度）时，
// 正在点击的按钮不会被换掉而让点击落空。cache 是调用方保存的 Map。
export function keyedRows(cache, items, key, sig, build) {
  const seen = new Set();
  const rows = items.map((it, i) => {
    const k = key(it);
    seen.add(k);
    const s = sig(it, i);
    const c = cache.get(k);
    if (c && c.sig === s) return c.el;
    const el = build(it, i);
    cache.set(k, { sig: s, el });
    return el;
  });
  for (const k of cache.keys()) if (!seen.has(k)) cache.delete(k);
  return rows;
}

// setChildren 只在子元素有变化时替换。
export function setChildren(parent, nodes) {
  const cur = parent.children;
  if (cur.length === nodes.length && nodes.every((n, i) => cur[i] === n)) return;
  parent.replaceChildren(...nodes);
}
