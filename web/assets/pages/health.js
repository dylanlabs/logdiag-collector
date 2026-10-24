// SPDX-License-Identifier: GPL-3.0-or-later

// 健康与告警：计划在 1.1 提供。这里只展示页面布局和计划内容，不显示任何数据。

import { h, button, notice, pageHead, card, soon } from '../lib.js';

const parts = ['处理器', '内存', '硬盘与阵列卡', '电源', '风扇', '温度', '网卡'];

export function mount(root) {
  root.append(
    pageHead('健康与告警', '汇总各设备的当前告警和部件健康状态，不用逐台登录管理口查看。', soon('1.1')),
    notice('soon',
      h('p', {}, h('strong', { text: '健康与告警计划在 1.1（2027 年 3 月）提供。' }), '下面是页面布局预览，不含任何数据。'),
      h('p', { text: '现在可以先在“日志收集”里收集日志包，上传到 LogDiag 网站分析告警。' })),
    h('div', { class: 'stats preview' },
      ['有告警的设备', '严重告警', '一般告警', '状态正常'].map((label) =>
        h('div', { class: 'stat' }, h('div', { class: 'stat-value faint', text: '—' }), h('div', { class: 'stat-label', text: label })))),
    h('section', { class: 'card card-flush preview' },
      h('div', { class: 'toolbar' }, h('h2', { class: 'toolbar-title', text: '当前告警' }), h('div', { class: 'spacer' }),
        button('刷新健康状态', { icon: 'refresh', disabled: true, title: '即将支持（1.1）' })),
      h('div', { class: 'table-wrap short' },
        h('table', { class: 'grid' },
          h('thead', {}, h('tr', {}, ['设备', '部件', '级别', '告警内容', '发生时间', '处理建议'].map((t) => h('th', { text: t })))),
          h('tbody', {}, h('tr', {}, h('td', { class: 'cell-empty', colspan: 6, text: '1.1 版本提供' })))))),
    card('计划内容',
      h('ul', { class: 'bullets' },
        h('li', { text: `用设备列表里保存的账号，批量读取各设备的部件健康状态：${parts.join('、')}。` }),
        h('li', { text: '汇总当前告警和事件日志里的严重事件，按级别和设备排序，可导出 Excel。' }),
        h('li', { text: '只读取，不清除告警、不修改设备配置；认证失败立即停止，不重试。' }),
        h('li', { text: '有告警的设备可以直接勾选，一键收集日志包。' }))),
  );
}
