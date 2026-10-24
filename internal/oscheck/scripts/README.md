<!-- SPDX-License-Identifier: GPL-3.0-or-later -->
# 内置巡检脚本

这里的脚本和 LogDiag 网站「OS 巡检」页提供下载的是同一份，编译时内嵌进 exe。
界面和命令行都会显示脚本的 SHA-256，可与网站上的下载核对；界面里也能查看脚本全文。

脚本只做读取类收集，不修改系统配置、不清日志、不重启。

出处：<https://logdiag.com/tools/os-inspection/>

| 文件 | 系统 | 说明 |
| --- | --- | --- |
| `linux-pm.sh` | Linux（RHEL/CentOS/Rocky/Alma/Ubuntu/Debian/SLES/openEuler/麒麟/统信等） | 已内置，v1.0 支持 |

其余系统（VMware ESXi、AIX、Solaris、HP-UX、Windows）的脚本等有对应测试环境后再内置。

更新脚本时：替换文件、同步改 `scripts.go` 里 catalog 的输出目录/文件名约定，并在这里记下新的 SHA-256。

## 校验和

```
SHA-256(linux-pm.sh) = 0425be630adf32bf76aa13739c715490a06cadd3daf8e5b8d1d3464d0c19a5a5
```
