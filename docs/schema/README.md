# Collector 和 LogDiag 网站之间的格式

Collector 和网站之间只靠三个文件对接，网站不连接 Collector，也不接收任何设备凭据。

| 文件 | 放在哪 | 谁写 | 谁读 | 格式 |
| --- | --- | --- | --- | --- |
| `manifest.json` | 每个设备文件夹里，和日志文件放在一起 | Collector | 网站 | [manifest-1.schema.json](manifest-1.schema.json) |
| `package.json` | 离线上传包（zip）里 | Collector | 网站 | [package-1.schema.json](package-1.schema.json) |
| `latest.json` | `https://logdiag.com/collector/latest.json` | 发布流水线生成，网站发布 | Collector 检查更新、网站下载页 | [latest-1.schema.json](latest-1.schema.json) |

版本规则：每个文件的 `schema` 字段写明格式和大版本（例如 `logdiag.manifest/1`）。同一个大版本里只加字段、不改含义，读取方忽略不认识的字段；遇到更新的大版本时不要硬读，提示用户升级（问题代码 `schema_unsupported`）。

[examples/](examples/) 下是样例，`examples/packages/` 里的样例包和 `expected.json` 是 Collector 和网站共用的契约测试，见文末。

## manifest.json

每次收集写一份，说明日志文件来自哪台设备、怎么收集的、每个文件的大小和 SHA-256。样例见 [examples/manifest.json](examples/manifest.json)。

- 网站仍按文件内容自动识别日志类型。`log.type` 用来核对、统计，以及识别不了时提示“已收到，暂不支持自动分析”。
- `files` 里 `role` 为 `main` 的是设备生成的日志包或命令输出，`extra` 是附带文件（例如 HPE 的 `IML.json`、网络设备的日志缓冲区输出）。`manifest.json` 本身不列在里面。
- `device.serial` 是整机序列号：Dell 填服务标签，网络设备填机框序列号，OS 巡检填所在服务器的序列号，这样同一台机器的 BMC 日志和 OS 巡检结果能对上。
- `device.mgmt_ip` 是收集时连接的地址。日志文件里本来就有这个地址，去掉它并不能隐藏 IP，所以不提供“不写入”的选项。
- 服务器和存储的日志包按设备给的原样保存，`redaction.applied` 为 `false`；网络设备的配置里的口令、SNMP 团体名等在收集时替换掉，`redaction.applied` 为 `true`，`rules` 写用了哪些规则。

### 日志类型编码

格式是“厂商.产品线.日志”，全小写，和网站共用这一份清单。新增编码时同时改这里和 `manifest-1.schema.json` 的 `examples`。

| log.type | 设备与日志 | 主文件（role 为 main） | 附带文件（role 为 extra） | Collector 版本 |
| --- | --- | --- | --- | --- |
| `huawei.ibmc.dump` | 华为 iBMC 一键收集 | `dump_info.tar.gz` | | v0.1 |
| `xfusion.ibmc.dump` | 超聚变 iBMC 一键收集 | `dump_info.tar.gz` | | v0.1 |
| `inspur.bmc.onekeylog` | 浪潮 M5/M6/M7 一键日志 | `.tar`（M5）、`.tar.gz`（M6/M7） | | v0.1 |
| `lenovo.xcc.ffdc` | 联想 XCC FFDC | `.tzz` | | v0.1 |
| `dell.idrac.tsr` | Dell iDRAC SupportAssist（TSR） | `sacollect.zip` | | v0.1 |
| `hpe.ilo.ahs` | HPE iLO AHS，按天数下载时写 `log.range` | `.ahs` | `IML.json`、`IEL.json`（Redfish 日志条目） | v0.1 |
| `h3c.hdm.sds` | 新华三 HDM SDS 日志 | `.sds` | | v0.2 |
| `lenovo.imm2.ffdc` | IBM / 联想 System x IMM2 FFDC | 待实测 | | v0.2 |
| `ibm.storwize.snap` | IBM Storwize / FlashSystem snap | `.tgz` | | v0.2 |
| `dell.powervault.logs` | Dell ME4 / ME5 日志 | `logs.zip` | | v0.2 |
| `hpe.msa.logs` | HPE MSA 日志 | `logs.zip` | | v0.2 |
| `netapp.ontap.asup` | NetApp AutoSupport | 待实测 | | v0.2 |
| `emc.unity.serviceinfo` | Dell EMC Unity 服务数据 | 待实测 | | v0.2 |
| `hpe.3par.cli` | HPE 3PAR / Primera 命令输出 | `.txt` | | v0.2 |
| `cisco.ios.showtech` | Cisco IOS / IOS XE `show tech-support` | `.txt` | 日志缓冲区输出 `.txt` | 1.0 |
| `cisco.nxos.showtech` | Cisco NX-OS `show tech-support` | `.txt` | 同上 | 1.0 |
| `cisco.asa.showtech` | Cisco ASA `show tech-support` | `.txt` | 同上 | 1.0 |
| `cisco.mds.showtech` | Cisco MDS 9000 `show tech-support details` | `.txt` | 同上 | 1.0 |
| `huawei.vrp.diag` | 华为交换机 `display diagnostic-information` | `.txt` | `display logbuffer` 等 | 1.0 |
| `h3c.comware.diag` | 新华三交换机 `display diagnostic-information` | `.txt` | 同上 | 1.0 |
| `logdiag.linux.pm` | Linux 巡检（网站 linux-pm.sh 的原样输出） | `<主机名>.<序列号>.pm-<时间>.tar.gz` | | 1.0 |
| `logdiag.esxi.pm`、`logdiag.aix.pm`、`logdiag.solaris.pm`、`logdiag.hpux.pm` | 其他系统巡检（网站对应脚本的输出） | 和网站脚本一致 | | 1.0 起陆续 |
| `logdiag.windows.pm` | Windows 巡检 | 和网站脚本一致 | | 1.1 |

服务器的 `device.type` 是 `server`，存储是 `storage`，网络设备是 `network`（`controller` 填网络操作系统和版本），OS 巡检是 `os`（另有 `host_ip`、`os` 两个可选字段）。网络设备和 OS 巡检的 `log.method` 是 `ssh`。

## 离线上传包

客户的管理网常常不通外网，Collector 把选中的结果按客户打成一个 zip，用户拷到能上网的电脑，再在网站上传。样例见 [examples/package.json](examples/package.json) 和 [examples/packages/ok.zip](examples/packages/ok.zip)。

```
LogDiag-示例客户-20261120-1430-5d0c8e1a.zip
├── package.json
├── Huawei_2288HV5_2102311TYBN0K8000123_dump_20261120-1403/
│   ├── manifest.json
│   └── dump_info.tar.gz
└── HPE_ProLiantDL380Gen10_CZJ00000A1_ahs_20261120-1405/
    ├── manifest.json
    ├── HPE_CZJ00000A1_20261120.ahs
    ├── IML.json
    └── IEL.json
```

- 每台设备一个文件夹，直接放在包的根目录下；`package.json` 列出全部设备文件夹和其中每个文件的大小、SHA-256，`role` 为 `manifest` 的是 `manifest.json`。
- 一个包只含一个客户的结果，`package.json` 的 `customer` 和每份 manifest 的 `customer` 相同。`package_id` 每个包唯一，同一个包再次上传时网站可以据此提示。
- 日志包本身已压缩，在 zip 里直接存储（不再压缩），读取方可以不解压、直接按偏移读出原文件；`.json`、`.txt`、`.log` 用 Deflate 压缩。
- 读取方还要能处理两种情况：用户解压后又用资源管理器重新压缩（所有文件变成 Deflate、多一层顶层文件夹、文件名可能是 GBK 编码且不设 UTF-8 标志），以及超过 4 GB 的包（zip64）。Collector 写的设备文件夹名和文件名都是 ASCII，顶层文件夹名解码错了也不影响。
- `package.json` 没列出的条目一律忽略。读取方只按检查过的名字读文件，不要按 zip 里的条目名解压到磁盘。

### 检查步骤

网站收包时按下面的步骤检查，`logdiag-collector verify` 是参照实现（[internal/results/verify.go](../../internal/results/verify.go)）。发现的问题一次列全，每个问题有代码（`code`）和位置（`path`），位置相对 `package.json` 所在的文件夹。

1. 打开 zip。不是完整的 zip：结果为 `not_zip`。
2. 找 `package.json`：在根目录；或者所有条目都在同一个顶层文件夹 X 里，并且有 `X/package.json`（重新压缩过的包）。都没有：结果为 `not_package`，不是离线上传包，照旧按普通日志处理。
3. 读 `package.json`（不超过 16 MiB）。出现了不止一个、不是 JSON 对象、`schema` 不对、或者字段不全、类型不对，都只报一个问题，位置是 `package.json`，并停止检查：
   - 先只看 `schema`：是 `logdiag.package/<数字>` 但不是 `logdiag.package/1`，报 `schema_unsupported`。更新的大版本可以改其他字段的类型，所以要先认版本、再按第 1 版查字段。
   - 其他情况：`package_invalid`。字段要求：`package_id` 是 UUID，`created_at` 是带时区的时间，`tool.name`、`tool.version`、`customer` 是字符串，`batch_ids` 是 UUID 数组，`devices` 是非空数组；每台设备有字符串 `dir`、符合编码格式的 `log_type`、数组 `files`；每个文件有字符串 `name`、不小于 0 的整数 `size`、64 位小写十六进制的 `sha256`、`main`/`extra`/`manifest` 之一的 `role`。
4. 按 `devices` 的顺序检查每台设备（设备文件夹记作 D）：
   1. D 不是安全的名字：`path_invalid`（位置 D），跳过这台设备。
   2. D 和前面的设备重复：`duplicate_path`（位置 D），跳过这台设备。
   3. 按 `files` 的顺序检查每个文件（文件名记作 N，位置 D/N）：N 不是安全的名字，`path_invalid`；N 在这台设备里重复，`duplicate_path`；zip 里没有 D/N（或只有同名文件夹），`file_missing`；zip 里有不止一个 D/N，`duplicate_path`；大小不对，`size_mismatch`；SHA-256 不对或读出错（数据损坏），`checksum_mismatch`。一个文件只报第一个问题。
   4. `role` 为 `manifest` 的条目不是恰好一个、或者名字不是 `manifest.json`：`manifest_missing`（位置 D）。
   5. 没有 `role` 为 `main` 的条目：`main_missing`（位置 D）。这两条按 `package.json` 列出的条目算，不管文件本身有没有问题。
   6. `manifest.json` 存在且大小、SHA-256 都对时，读它（不超过 1 MiB）：大版本更新，`schema_unsupported`（同样先看 `schema` 再查字段）；不是 JSON 对象、`schema` 不对、字段不全或类型不对，`manifest_invalid`（字段要求见 manifest 的 schema：必填字段齐全，时间带时区，`batch_id` 是 UUID，`device.type` 在枚举里，`log.type` 符合编码格式，`files` 非空、每个文件 `size` 至少为 1、`role` 是 `main` 或 `extra`、至少一个 `main`，`redaction.applied` 是布尔值）；和 `package.json` 对不上，`manifest_mismatch`。位置都是 D/manifest.json。对得上指：`log.type` 等于 `log_type`，`customer` 相同，`batch_id` 在 `batch_ids` 里，`files` 和 `package.json` 里这台设备除 `manifest.json` 以外的条目一一对应（名字、大小、SHA-256、role 都相同，不管顺序）。

安全的名字：非空，UTF-8 编码不超过 255 字节，不是 `.` 或 `..`，不含 `/`、`\`、`:` 和控制字符（小于 0x20）。几个格式的正则：

| 项 | 正则 |
| --- | --- |
| UUID | `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$` |
| 带时区的时间 | `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z\|[+-]\d{2}:\d{2})$` |
| SHA-256 | `^[0-9a-f]{64}$` |
| 日志类型编码 | `^[a-z0-9]+(\.[a-z0-9]+)+$` |

### 问题代码

| code | 位置 | 意思 | 建议给用户的提示 |
| --- | --- | --- | --- |
| `package_invalid` | `package.json` | package.json 不是合法的 JSON，或缺字段、类型不对 | 离线上传包已损坏，请在 Collector 里重新打包 |
| `schema_unsupported` | `package.json` 或 D/manifest.json | 更新的大版本 | 这个包由更新的 Collector 生成，网站暂不支持 |
| `path_invalid` | D 或 D/N | 名字不安全 | 离线上传包不合规，已拒收 |
| `duplicate_path` | D 或 D/N | 重复的设备文件夹或文件，或 zip 里有同名条目 | 同上 |
| `file_missing` | D/N | 列出的文件不在包里 | 文件缺失，请重新打包 |
| `size_mismatch` | D/N | 大小不对 | 文件不完整，可能拷贝时中断了，请重新拷贝或重新打包 |
| `checksum_mismatch` | D/N | SHA-256 不对，或数据损坏 | 文件已损坏或被修改过，请重新拷贝或重新打包 |
| `manifest_missing` | D | 没有 manifest.json | 请重新打包 |
| `main_missing` | D | 没有主日志文件 | 请重新打包 |
| `manifest_invalid` | D/manifest.json | manifest.json 不是合法的 JSON，或缺字段、类型不对 | 请重新收集这台设备 |
| `manifest_mismatch` | D/manifest.json | manifest.json 和 package.json 对不上 | 请重新打包 |

有问题的设备不要上传；同一个包里没有问题的设备可以照常处理，由用户决定。

## latest.json

放在 `https://logdiag.com/collector/latest.json`，样例见 [examples/latest.json](examples/latest.json)。发布流水线用 [scripts/latestjson](../../scripts/latestjson) 生成并附在 GitHub Release 里，更新说明取自 [CHANGELOG.md](../../CHANGELOG.md)。和 BMCKit 的 `latest.json` 兼容：只有 `version`、`url`、`notes` 三项也能用；`downloads` 只给网站下载页用。

Collector 的做法：

- 只在用户点“检查更新”时读一次，一次 GET，User-Agent 为 `LogDiagCollector/<版本号>`，不发送任何本机信息；超时 15 秒，只允许 HTTPS，最多跟随 3 次重定向。按 Windows 的代理设置访问（环境变量 `HTTPS_PROXY` 优先；不支持自动配置脚本）。
- 读不到 logdiag.com 时改读 GitHub 的 `releases/latest`（只取版本号、发布页和发布时间）。
- 版本号按语义化版本比较，`0.2.0-beta.1` 比 `0.2.0` 旧。比当前新时询问是否打开下载页；当前版本低于 `min_supported` 时提示必须升级（开发版本除外）。
- 下载页只打开 logdiag.com 和本项目 GitHub 上的 HTTPS 网页，没有 `utm_source` 时加上 `utm_source=collector&utm_medium=app`。更新说明最多显示 600 字。程序不下载、不安装任何文件。

发布顺序：先把 exe 放到 logdiag.com 的下载地址，再更新 `latest.json`，否则用户会拿到还没上传的下载地址。

## 契约样例

`examples/` 下的文件由 `go test ./internal/results -run TestExamples -update`（样例包、manifest.json、package.json）和 `go test ./scripts/latestjson -update`（latest.json）生成，测试会检查提交的样例和代码一致、符合 schema。

| 样例包 | 内容 | 结果 |
| --- | --- | --- |
| `ok.zip` | Collector 打的包：华为、浪潮各一台，HPE 一台带两个附带文件 | ok |
| `ok-rezipped.zip` | 解压后连文件夹一起重新压缩（GBK 顶层文件夹名、全部 Deflate、带文件夹条目） | ok |
| `ok-zip64.zip` | 所有条目都用 zip64 结构 | ok |
| `bad-files.zip` | 内容被改、文件丢失、文件被截短 | 3 个问题 |
| `bad-manifest.zip` | manifest 和 package.json 不一致、不是合法 JSON、没列出 manifest.json | 3 个问题 |
| `bad-paths.zip` | 设备文件夹名带 `../`、设备文件夹重复、文件名带冒号 | 4 个问题 |
| `bad-package.zip` | package.json 缺 `package_id` | `package_invalid` |
| `newer-version.zip` | `logdiag.package/2` | `schema_unsupported` |
| `not-a-package.zip` | 普通 zip | `not_package` |
| `not-a-zip.zip` | 扩展名是 .zip 的文本文件 | `not_zip` |

每个样例包应得到的结果写在 [examples/packages/expected.json](examples/packages/expected.json) 里，网站的服务端和浏览器端实现都应得到同样的 `result` 和同样顺序的 `code`、`path`（`message` 只给人看，不比较）。浏览器端不计算大文件的 SHA-256，比较时跳过位置不是 `manifest.json` 的 `checksum_mismatch`，由服务端在收到文件后核对。样例里的日志文件是合成的小文件，只能测拆包和校验，不能测解析器。
