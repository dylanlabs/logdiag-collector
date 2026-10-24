// SPDX-License-Identifier: GPL-3.0-or-later

// 各页面共享的数据：设备、凭据模板、收集任务、设置。后台的变化经事件流推送过来，
// 这里更新数据后发出 changed:devices、changed:tasks 等通知，页面据此重画。

import { api, bus, showError } from './lib.js';

export const store = {
  state: null, // /api/state：版本、设置、数据文件夹等
  devices: [],
  credentials: [],
  customers: [],
  tasks: [],
  loaded: { devices: false, tasks: false },
};

export async function loadState() {
  store.state = await api.get('/api/state');
  bus.emit('changed:state');
  return store.state;
}

export async function loadDevices() {
  try {
    const data = await api.get('/api/devices');
    store.devices = data.devices || [];
    store.credentials = data.credentials || [];
    store.customers = data.customers || [];
    store.loaded.devices = true;
    bus.emit('changed:devices');
  } catch (err) {
    showError(err);
  }
}

export async function loadTasks() {
  try {
    const data = await api.get('/api/tasks');
    store.tasks = data.tasks || [];
    store.loaded.tasks = true;
    bus.emit('changed:tasks');
  } catch (err) {
    showError(err);
  }
}

// 事件流里的单台设备更新。
bus.on('event:device', (d) => {
  const i = store.devices.findIndex((x) => x.id === d.id);
  if (i >= 0) store.devices[i] = d;
  else store.devices.push(d);
  bus.emit('changed:devices');
});

bus.on('event:devices', () => loadDevices());

bus.on('event:task', (t) => {
  const i = store.tasks.findIndex((x) => x.id === t.id);
  if (i >= 0) store.tasks[i] = t;
  else store.tasks.unshift(t);
  bus.emit('changed:tasks');
  if (t.state === 'done') bus.emit('changed:results');
});

// 重新连上本机服务后（例如电脑睡眠唤醒），把所有数据重新读一遍。
bus.on('event:hello', () => {
  loadDevices();
  loadTasks();
});

// collectingIDs 返回正在收集（排队或进行中）的设备 ID。
export function collectingIDs() {
  const ids = new Set();
  for (const t of store.tasks) {
    if (t.state === 'queued' || t.state === 'running') ids.add(t.device_id);
  }
  return ids;
}

export function credentialName(id) {
  return store.credentials.find((c) => c.id === id)?.name || '';
}

// ---------- 品牌与支持情况 ----------

// brands 是设计文档里各品牌一键收集的计划版本，顺序即品牌下拉框的顺序。
// keys 是匹配设备表品牌写法的关键词（小写），vendor 是下拉框选中时写进设备表的写法（与识别结果一致）。
export const brands = [
  { keys: ['huawei', '华为'], name: '华为', vendor: 'Huawei', status: 'supported' },
  { keys: ['xfusion', '超聚变'], name: '超聚变', vendor: 'xFusion', status: 'supported' },
  { keys: ['inspur', '浪潮', 'ieit'], name: '浪潮', vendor: 'Inspur', status: 'supported' },
  { keys: ['lenovo', '联想'], name: '联想', vendor: 'Lenovo', status: 'supported' },
  { keys: ['dell', '戴尔'], name: 'Dell', vendor: 'Dell', status: 'supported' },
  { keys: ['hpe', 'hewlett', '慧与', '惠普', 'hp'], name: 'HPE', vendor: 'HPE', status: 'supported' },
  { keys: ['h3c', '新华三', '华三'], name: '新华三', vendor: 'H3C', status: 'v0.2' },
  { keys: ['ibm'], name: 'IBM', vendor: 'IBM', status: 'v0.2' },
];

// brandOf 把设备表里的品牌写法对应到 brands 里的一项，认不出时为 undefined。
export function brandOf(vendor) {
  const v = (vendor || '').toLowerCase();
  return v ? brands.find((b) => b.keys.some((k) => v.includes(k))) : undefined;
}

// netBrands 是已支持一键收集的网络设备厂商，写法同 brands。连上以后按系统识别，品牌只用来提前挡住明显不支持的设备；
// HPE 的 FlexNetwork 交换机（原 H3C）是 Comware 系统，按新华三处理。
export const netBrands = [
  { keys: ['cisco', '思科'], name: '思科', vendor: 'Cisco' },
  { keys: ['huawei', '华为'], name: '华为', vendor: 'Huawei' },
  { keys: ['h3c', '新华三', '华三', 'hpe', 'hewlett', '慧与', '惠普', 'hp'], name: '新华三', vendor: 'H3C' },
];

// netBrandOf 把网络设备的品牌写法对应到 netBrands 里的一项，认不出时为 undefined。
export function netBrandOf(vendor) {
  const v = (vendor || '').toLowerCase();
  return v ? netBrands.find((b) => b.keys.some((k) => v.includes(k))) : undefined;
}

// networkSupport 是网络设备的一键收集支持情况，格式同 support。
function networkSupport(device) {
  if (!device.vendor) return { status: 'auto', text: '收集时识别', tone: 'gray', blocked: false };
  if (netBrandOf(device.vendor)) return { status: 'supported', text: '支持', tone: 'green', blocked: false };
  return {
    status: 'none', text: '暂不支持', tone: 'gray', blocked: true,
    reason: `暂不支持这个品牌的网络设备（已支持${store.state?.supported_net || '思科、华为、新华三'}）。如果品牌填错了，清空品牌后收集时会自动识别`,
  };
}

// support 返回设备的一键收集支持情况：
// { status: 'supported'|'auto'|'v0.2'|'1.0'|'none', text（表格里的短说明）, reason（不能收集时的原因）, tone, blocked }。
export function support(device) {
  const supported = store.state?.supported || '华为、超聚变、浪潮、联想、Dell、HPE';
  if (device.type && device.type !== 'server') {
    if (device.type === 'network') return networkSupport(device);
    const [v, name] = device.type === 'storage' ? ['v0.2', '存储'] : ['', '这类设备'];
    return {
      status: v || 'none', text: v ? `计划 ${v}` : '暂不支持', tone: v ? 'blue' : 'gray', blocked: true,
      reason: v ? `${name}的日志收集计划在 ${v} 支持` : '目前只支持服务器和网络设备的一键收集',
    };
  }
  if (!device.vendor) return { status: 'auto', text: '收集时识别', tone: 'gray', blocked: false };
  const hit = brandOf(device.vendor);
  if (hit?.status === 'supported') return { status: 'supported', text: '支持', tone: 'green', blocked: false };
  if (hit) {
    return { status: hit.status, text: `计划 ${hit.status}`, tone: 'blue', blocked: true, reason: `${hit.name}的一键收集计划在 ${hit.status} 支持` };
  }
  return {
    status: 'none', text: '暂不支持', tone: 'gray', blocked: true,
    reason: `暂不支持这个品牌（已支持${supported}）。如果品牌填错了，清空品牌后收集时会自动识别`,
  };
}

// deviceStatus 返回设备连通状态的文字和颜色：登录成功绿、服务可用青、收集中蓝、认证失败红、不可达灰。
export function deviceStatus(d, collecting) {
  const s = d.status || {};
  if (collecting) return { text: '收集中', tone: 'blue' };
  if (s.reach === 'checking') return { text: '检查中…', tone: 'gray' };
  if (s.login === 'checking') return { text: '识别中…', tone: 'gray' };
  if (s.reach === 'unreachable') return { text: '不可达', tone: 'gray' };
  if (s.login === 'auth_failed') return { text: '认证失败', tone: 'red' };
  if (s.login === 'ok') return { text: '登录成功', tone: 'green' };
  if (s.login === 'failed') return { text: '识别失败', tone: 'amber' };
  if (s.reach === 'service') return { text: '服务可用', tone: 'teal' };
  if (s.reach === 'reachable') return { text: '网络可达', tone: 'amber' };
  if (!d.mgmt_ip) return { text: '无管理口', tone: 'gray' };
  return { text: '未检查', tone: 'gray' };
}

// logTypes 是 manifest 里日志类型编码的中文名。
export const logTypes = {
  'huawei.ibmc.dump': '华为 iBMC 一键收集包',
  'xfusion.ibmc.dump': '超聚变 iBMC 一键收集包',
  'inspur.bmc.onekeylog': '浪潮一键日志',
  'lenovo.xcc.ffdc': '联想 XCC FFDC',
  'dell.idrac.tsr': 'Dell iDRAC TSR',
  'hpe.ilo.ahs': 'HPE iLO AHS',
  'cisco.ios.showtech': '思科 IOS / IOS XE show tech-support',
  'cisco.nxos.showtech': '思科 NX-OS show tech-support',
  'cisco.mds.showtech': '思科 MDS show tech-support details',
  'cisco.asa.showtech': '思科 ASA show tech-support',
  'huawei.vrp.diag': '华为 VRP 诊断信息',
  'h3c.comware.diag': '新华三 Comware 诊断信息',
};

export const deviceTypes = [['server', '服务器'], ['storage', '存储'], ['network', '网络'], ['other', '其他']];
export const typeName = Object.fromEntries(deviceTypes);

// typeChoices 是设备类型下拉框：写明还没支持收集的类型计划在哪个版本支持。
export const typeChoices = [['server', '服务器'], ['storage', '存储（收集计划 v0.2）'], ['network', '网络（交换机、路由器、防火墙）'], ['other', '其他（暂不支持收集）']];

// deviceLabel 是提示里用的设备名称：客户 + 地址。
export function deviceLabel(d) {
  return [d.customer, d.mgmt_ip || d.host_ip].filter(Boolean).join(' ') || '未命名设备';
}
