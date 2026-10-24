// SPDX-License-Identifier: GPL-3.0-or-later

// OS 巡检：计划在 1.0 提供（Windows 在 1.1）。这里展示计划支持的系统和流程，不显示任何数据。

import { h, button, notice, pageHead, card, soon, tag } from '../lib.js';
import { store } from '../store.js';

const systems = [
  ['Linux', 'linux-pm.sh', 'SSH', '1.0'],
  ['VMware ESXi', 'vmware-esxi-pm.sh', 'SSH', '1.0'],
  ['IBM AIX', 'aix-pm.sh', 'SSH', '1.0'],
  ['Oracle Solaris', 'solaris-pm.sh', 'SSH', '1.0'],
  ['HP-UX', 'hpux-pm.sh', 'SSH', '1.0'],
  ['Windows Server', 'windows-pm.ps1', 'WinRM 或 OpenSSH', '1.1'],
];

export function mount(root) {
  const off = store.state?.settings?.os_module === false;
  root.append(
    pageHead('OS 巡检', '在服务器操作系统里运行 LogDiag 的巡检脚本，取回结果后和日志包一起打包上传。', soon('1.0')),
    ...(off ? [notice('warn', h('p', {}, 'OS 巡检已在设置里关闭，导航里不显示。', h('a', { href: '#/settings', text: '去设置打开' })))] : []),
    notice('soon',
      h('p', {}, h('strong', { text: 'OS 巡检计划在 1.0（2027 年 1 月底）提供，Windows 巡检在 1.1。' }), '下面是计划支持的系统和使用流程。'),
      h('p', { text: '在此之前，可以到 LogDiag 网站下载巡检脚本，手动执行后上传结果。不需要这个功能可以在设置里关掉。' })),
    h('section', { class: 'card card-flush' },
      h('div', { class: 'toolbar' }, h('h2', { class: 'toolbar-title', text: '计划支持的系统' }), h('div', { class: 'spacer' }),
        button('开始巡检', { kind: 'primary', icon: 'play', disabled: true, title: '即将支持（1.0）' })),
      h('div', { class: 'table-wrap' },
        h('table', { class: 'grid' },
          h('thead', {}, h('tr', {}, ['系统', '巡检脚本', '连接方式', '计划版本'].map((t) => h('th', { text: t })))),
          h('tbody', {}, systems.map(([os, script, via, v]) => h('tr', {},
            h('td', { text: os }), h('td', { class: 'mono', text: script }), h('td', { text: via }), h('td', {}, tag(v, 'blue')))))))),
    h('div', { class: 'card-grid' },
      card('使用流程',
        h('ol', { class: 'steps' },
          h('li', { text: '在设备列表里填好主机 IP 和系统，1.0 起还可以填系统账号。' }),
          h('li', { text: '勾选设备开始巡检：脚本上传到临时目录执行，完成后取回结果。' }),
          h('li', { text: '结果和日志包一起出现在“收集结果”里，可以打进同一个离线上传包。' }))),
      card('安全',
        h('ul', { class: 'bullets' },
          h('li', { text: '和 LogDiag 网站提供下载的是同一套脚本，只读取系统信息和日志，不修改配置。' }),
          h('li', { text: '执行完删除上传到服务器上的临时文件。' }),
          h('li', { text: '系统账号和管理口账号一样加密保存在本机；认证失败立即停止，不重试。' })))),
  );
}
