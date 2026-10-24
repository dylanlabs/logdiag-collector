// SPDX-License-Identifier: GPL-3.0-or-later

// 设置：保存位置、并发数、超时、诊断记录、Dell 许可协议、OS 巡检模块；数据文件夹、密码加密方式、关于。
// 每一项改完立即保存。

import { h, button, api, bus, toast, showError, input, checkbox, pageHead, card, soon, tag } from '../lib.js';
import { store } from '../store.js';
import { openPath, openLink, links, checkUpdate } from '../actions.js';

function row(name, ...controls) {
  return h('div', { class: 'setting-row' },
    h('div', { class: 'setting-name', text: name }),
    h('div', { class: 'setting-control' }, ...controls));
}

const hint = (text) => h('div', { class: 'field-hint', text });

export function mount(root) {
  let defaults = null;
  const s = () => store.state.settings;

  async function save(patch, okText = '已保存') {
    try {
      const res = await api.post('/api/settings', patch);
      store.state.settings = res.settings;
      bus.emit('changed:state');
      toast(okText, 'ok');
    } catch (err) {
      showError(err);
    }
    fill();
  }

  // 收集
  const output = input({ class: 'input input-path', spellcheck: 'false', 'aria-label': '保存位置' });
  output.addEventListener('change', () => {
    const v = output.value.trim();
    if (v && v !== s().output_dir) save({ output_dir: v }, '已修改保存位置');
    else fill();
  });
  const btnDefaultDir = button('恢复默认', {
    size: 'small',
    onclick: () => defaults && save({ output_dir: defaults.output_dir }, '已恢复默认保存位置'),
  });
  const number = (label, min, max, key) => {
    const el = input({ type: 'number', min, max, step: 1, 'aria-label': label });
    el.addEventListener('change', () => {
      const v = Number(el.value);
      if (!Number.isInteger(v) || v < min || v > max) {
        showError(new Error(`${label}要在 ${min} 到 ${max} 之间`));
        fill();
        return;
      }
      if (v !== s()[key]) save({ [key]: v });
    });
    return el;
  };
  const concurrency = number('同时收集的台数', 1, 32, 'concurrency');
  const timeout = number('单台超时', 5, 180, 'timeout_minutes');
  const record = checkbox('保留诊断记录', false, { onchange: (e) => save({ record: e.target.checked }) });
  const acceptEULA = checkbox('代为接受 Dell SupportAssist 许可协议', false, { onchange: (e) => save({ accept_eula: e.target.checked }) });
  const osModule = checkbox('在导航里显示“OS 巡检”', false, { onchange: (e) => save({ os_module: e.target.checked }) });

  function fill() {
    output.value = s().output_dir;
    concurrency.value = s().concurrency;
    timeout.value = s().timeout_minutes;
    record.querySelector('input').checked = s().record;
    acceptEULA.querySelector('input').checked = s().accept_eula;
    osModule.querySelector('input').checked = s().os_module;
    btnDefaultDir.disabled = !defaults || defaults.output_dir === s().output_dir;
  }

  const st = store.state;
  const info = st.info || {};
  root.append(
    pageHead('设置', '改完立即生效并保存在本机。'),
    card('收集',
      row('保存位置',
        h('div', { class: 'inline' }, output,
          button('打开', { size: 'small', icon: 'folder', onclick: () => openPath('output') }), btnDefaultDir),
        hint('日志包按“客户 / 日期 / 设备”分文件夹保存。可以填 U 盘或网络盘的路径。')),
      row('同时收集的台数',
        h('div', { class: 'inline' }, concurrency, h('span', { class: 'muted', text: '台（1–32）' })),
        hint('设备生成日志包时管理口负载较高，同一网段设备多时建议不超过 8 台。改动对排队中的任务立即生效。')),
      row('单台超时',
        h('div', { class: 'inline' }, timeout, h('span', { class: 'muted', text: '分钟（5–180）' })),
        hint('设备生成日志包较慢时可以调大。超时后只停止本机等待，不会中断设备上的任务。')),
      row('诊断记录', record,
        hint('记录收集时和设备的交互，已去掉密码、令牌、Cookie 和设备 IP；收集失败时可以在“日志收集”页导出，附在反馈里。只保存在内存里，程序退出后清除。')),
      row('Dell 许可协议', acceptEULA,
        hint('iDRAC 第一次导出 SupportAssist 收集包前，要接受 Dell 的 SupportAssist 许可协议（EULA）。勾选后遇到还没接受的 iDRAC，由本工具代为接受，等同于在 iDRAC 网页上点“接受”，iDRAC 会记下接受的账号和时间。不勾选时这类设备会收集失败，并提示你先去接受。'))),
    card('功能模块',
      row('OS 巡检', osModule,
        h('div', { class: 'inline' }, tag('计划 1.0 提供', 'blue'), h('span', { class: 'field-hint', text: '不需要时可以关掉，导航里就不再显示。' })))),
    card('数据与安全',
      row('数据文件夹',
        h('div', { class: 'inline' }, h('span', { class: 'path', text: st.data_dir }),
          button('打开', { size: 'small', icon: 'folder', onclick: () => openPath('data') })),
        hint('设备列表（devices.json）和设置保存在这里。换电脑时可以导出设备表，在新电脑上导入。')),
      row('密码保存方式',
        h('div', { text: st.secret_method }),
        hint('设备和凭据模板的密码加密后保存，只有这台电脑上的当前 Windows 账号能解开；导出的表格不含密码。')),
      row('主密码',
        h('div', { class: 'inline' }, soon(), h('span', { class: 'field-hint', text: '设置后每次打开程序要先输入主密码，才能使用保存的密码。' }))),
      row('联网',
        h('div', { text: '只在你点击时联网（检查更新、上传、反馈），按 Windows 的代理设置访问外网。和设备通信直连管理网，不走代理。' }))),
    card('更新与上传',
      row('检查更新',
        h('div', { class: 'inline' },
          button('检查更新', { size: 'small', icon: 'update', onclick: checkUpdate }),
          button('打开发布页', { size: 'small', icon: 'open', onclick: () => openLink(links.releases) })),
        hint('先读 logdiag.com 上的最新版本信息，连不上时读 GitHub，只发送程序名和版本号。有新版本时由你决定是否打开下载页，程序不会自己下载或安装。')),
      row('上传到 LogDiag',
        h('div', { class: 'inline' }, soon('1.1'), h('span', { class: 'field-hint', text: '届时在这里填写 LogDiag 账号的上传令牌。' })))),
    card('关于',
      h('dl', { class: 'kv' },
        h('dt', { text: '版本' }), h('dd', { text: `v${info.version || ''}` }),
        info.commit ? [h('dt', { text: '提交' }), h('dd', { class: 'mono', text: `${info.commit.slice(0, 12)}${info.modified ? '（有未提交的修改）' : ''}` })] : null,
        h('dt', { text: '构建' }), h('dd', { text: [info.go, info.os && `${info.os}/${info.arch}`].filter(Boolean).join(' · ') }),
        h('dt', { text: '许可证' }), h('dd', { text: 'GPL-3.0-or-later，免费开源' }),
        h('dt', { text: '源代码' }), h('dd', {}, h('a', { href: links.repo, target: '_blank', rel: 'noopener noreferrer', text: links.repo }))),
      h('p', { class: 'muted about-note', text: 'LogDiag Collector 是 LogDiag 旗下的免费工具，和 BMCKit 是同一家族。' })),
  );

  fill();
  api.get('/api/settings').then((res) => {
    defaults = res.defaults;
    fill();
  }).catch(showError);
  const off = bus.on('changed:state', fill);
  return off;
}
