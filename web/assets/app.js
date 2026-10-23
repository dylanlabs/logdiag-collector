// SPDX-License-Identifier: GPL-3.0-or-later

// 本机界面外壳：导航、版本号、与本机服务的连接状态。各页面的功能后续逐个接入。

const pages = {
  devices: {
    title: '设备列表',
    text: '填写管理口 IP 和账号，品牌、型号、序列号自动识别回填；支持 Excel/CSV 导入导出、BMCKit 导出的 CSV 和凭据模板。',
  },
  collect: {
    title: '日志收集',
    text: '对选中的设备一键或批量收集日志包，显示每台设备的进度和失败原因。',
  },
  health: {
    title: '健康与告警',
    text: '汇总各设备的告警事件和部件健康状态。',
  },
  results: {
    title: '收集结果',
    text: '按客户查看已收集的日志文件，打包成离线上传包，再上传到 LogDiag 分析。',
  },
  os: {
    title: 'OS 巡检',
    text: '通过 SSH 或 WinRM 执行 LogDiag 的巡检脚本并取回结果。此模块可在设置中关闭。',
  },
  settings: {
    title: '设置',
    text: '并发数、文件保存位置、主密码等。',
  },
};

const notices = {
  update: '检查更新功能开发中。',
  language: '英文界面开发中。',
  feedback: '问题反馈功能开发中。',
};

const $ = (id) => document.getElementById(id);

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text) node.textContent = text;
  return node;
}

function render() {
  const id = location.hash.replace(/^#\//, '');
  const page = pages[id] ? id : 'devices';
  const { title, text } = pages[page];

  document.title = `${title} - LogDiag Collector`;
  $('page-title').textContent = title;
  for (const a of $('nav').querySelectorAll('a')) {
    a.classList.toggle('active', a.dataset.page === page);
  }

  const card = el('div', 'card');
  card.append(el('p', '', text), el('p', 'muted', '此页面正在开发。'));
  $('page').replaceChildren(card);
}

function setStatus(text) {
  $('status').textContent = text;
}

function setConnected(ok) {
  const tag = $('conn');
  tag.textContent = ok ? '已连接' : '未连接';
  tag.className = `tag ${ok ? 'tag-green' : 'tag-red'}`;
  if (!ok) setStatus('与本机服务的连接已断开；如果程序已退出，请重新运行。');
}

async function loadVersion() {
  try {
    const res = await fetch('/api/info');
    const info = await res.json();
    $('version').textContent = `v${info.version}`;
    $('version').title = info.commit ? `提交 ${info.commit.slice(0, 7)}，${info.go}` : info.go;
  } catch {
    $('version').textContent = '';
  }
}

function connect() {
  // 事件流断开后浏览器会自动重连；程序在所有窗口关闭一段时间后退出。
  const events = new EventSource('/api/events');
  events.addEventListener('hello', () => {
    setConnected(true);
    setStatus('本机模式');
  });
  events.addEventListener('error', () => setConnected(false));
}

for (const btn of document.querySelectorAll('[data-action]')) {
  btn.addEventListener('click', () => setStatus(notices[btn.dataset.action]));
}
window.addEventListener('hashchange', render);
render();
loadVersion();
connect();
