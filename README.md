# LogDiag Collector

面向数据中心运维和服务工程师的免费开源工具：管理设备的管理口信息，一键收集服务器 BMC、存储、网络设备的日志包，再交给 [LogDiag](https://logdiag.com) 分析。

> **状态：开发中，尚未发布可用版本。** 命令行已能对华为、超聚变、浪潮服务器一键收集日志包（等待真机验证），界面还在开发。

## 计划支持的设备

| 类别 | 品牌 |
| --- | --- |
| 服务器 | 华为、超聚变、浪潮、联想、Dell、HPE；随后是新华三、IBM System x |
| 存储 | IBM、Dell EMC、HPE、NetApp |
| 网络 | Cisco、华为、新华三 |

具体型号和固件以每个版本发布说明里的“已验证”列表为准。

## 下载与运行

- 系统：Windows 10 / 11（x64）。
- 在 [Releases](https://github.com/dylanlabs/logdiag-collector/releases) 下载 exe，双击运行。不需要安装，也不需要管理员权限。
- 程序会打开一个独立窗口（Microsoft Edge 的应用模式；没有 Edge 时用默认浏览器）。旁边的命令行窗口显示运行状态，关闭界面窗口后程序自动退出。
- 开发中的测试版：每次提交都会在 [Actions](https://github.com/dylanlabs/logdiag-collector/actions/workflows/ci.yml) 里编出 exe，在运行记录页面底部的附件里下载（需要登录 GitHub）。

下文把下载的文件简称为 `LogDiagCollector.exe`。

## 首次运行与安全提示

### “Windows 已保护你的电脑”

当前版本**没有代码签名**，首次运行时 Windows SmartScreen 会弹出“Windows 已保护你的电脑”。点 **更多信息**，再点 **仍要运行** 即可。

签名计划见下面的[代码签名政策](#代码签名政策)。需要说明的是，即使签名之后，新发布的程序在积累信誉之前也可能出现这个提示。

### 核对文件没有被篡改

每个版本的 Release 页面都附有 `SHA256SUMS`。在 PowerShell 里计算下载文件的 SHA-256，与之比对：

```powershell
Get-FileHash .\LogDiagCollector.exe -Algorithm SHA256
```

Release 还附有 GitHub 构建证明，证明 exe 是由本仓库的公开源码在 GitHub Actions 上自动编译的。装有 [GitHub CLI](https://cli.github.com) 时可以这样核对：

```
gh attestation verify LogDiagCollector.exe --repo dylanlabs/logdiag-collector
```

### 企业电脑与杀毒软件

- 部分企业电脑用应用控制策略（AppLocker、WDAC）只允许运行已签名的程序，未签名版本在这些电脑上无法运行，请联系 IT 部门放行或等待签名版本。
- 本程序不加壳，文件属性里有完整的产品名、版本和发布者。如果杀毒软件误报，请到 [Microsoft 文件提交页](https://www.microsoft.com/wdsi/filesubmission) 提交误报，也欢迎在 Issues 里告诉我们。

## 命令行

```
LogDiagCollector.exe                       启动界面
LogDiagCollector.exe probe --host 10.0.0.10 --user admin --record probe.json
LogDiagCollector.exe collect --host 10.0.0.10 --user admin --record collect.json
LogDiagCollector.exe version               显示版本
LogDiagCollector.exe help                  查看全部命令
```

密码会提示输入（不回显），也可以事先放在环境变量 `LOGDIAG_PASSWORD` 里。

`probe` 只读探测一台设备的 Redfish 接口：显示 Redfish 版本、型号、序列号、固件版本，以及设备支持的日志动作。它只调用读取类接口，不触发收集、不修改设备。不填 `--user` 时只匿名读取服务根，不登录、不产生审计日志。

`collect` 让设备生成一键日志包并下载，目前支持：

| 品牌 | 日志包 | 方式 |
| --- | --- | --- |
| 华为、超聚变 iBMC | 一键收集包 `dump_info.tar.gz` | Redfish OEM 动作 Manager.Dump + Manager.GeneralDownload |
| 浪潮 M5 / M6 / M7 | 一键日志（M5 为 `.tar`，M6/M7 为 `.tar.gz`） | BMC 网页接口 |

品牌自动识别，也可以用 `--brand huawei|xfusion|inspur` 指定。生成要 3 到 10 分钟，期间显示进度；按 Ctrl+C 取消时会退出会话，设备端的生成任务不受影响。工具不清除日志、不重启设备，认证失败立即停止、不重试。

文件保存在 `--out` 指定的位置（默认当前文件夹），一台设备一个文件夹，旁边的 `manifest.json` 写明设备、日志类型和 SHA-256，LogDiag 据此自动识别（格式见 [docs/schema/manifest-1.schema.json](docs/schema/manifest-1.schema.json)）：

```
[客户]\20261026\Huawei_2288HV5_2102311XXXXX_dump_20261026-1403\
    manifest.json
    dump_info.tar.gz
```

已知限制：浪潮部分新固件要求网页登录时对账号密码加密，目前还不支持，这类设备会显示“认证失败”。

`probe` 和 `collect` 加 `--record` 会把这次的接口往返保存成诊断记录（日志包本身只留开头一小段和 SHA-256）。记录里已自动去掉密码、会话令牌、Cookie 和设备 IP，但仍保留型号、序列号、主机名和其他地址等设备信息，发给别人前请先看一遍。

## 隐私说明

- **不收集遥测**，不上报任何使用数据。
- **默认不联网。** 只有你点击“检查更新”“上传到 LogDiag”“问题反馈”“开始分析日志”时才会访问 logdiag.com。和设备之间的通信直连管理网，不经过系统代理。
- **凭据只留在本机。** 设备账号密码加密保存在本机（规划中：默认用 Windows DPAPI 绑定当前 Windows 用户），永远不会上传。
- **日志由你决定是否上传。** 收集到的日志可能含设备配置、序列号、主机名、IP 等信息；上传前会列出文件清单由你确认。
- **诊断记录自动脱敏**，去掉的内容和保留的内容见上面的 `probe` 说明。
- **本机界面只有你能访问**：只监听 127.0.0.1，并用一次性令牌打开，局域网里的其他电脑访问不到。

当前开发版还没有任何联网功能。以上承诺适用于以后的所有版本。

## 代码签名政策（Code signing policy）

> **待补充。** v0.x 暂未签名。计划在首个正式发布的版本（v0.1）之后申请 [SignPath Foundation](https://signpath.org) 为开源项目提供的免费代码签名，获批后在这里公布完整政策。SignPath 要求项目已经以要签名的形式发布过，并会审核项目的信誉，所以不能在发布前申请。

计划中的政策要点：

- 只签名由本仓库 GitHub Actions 从公开源码自动构建的 Release 文件，不签名任何本地构建。
- 每次签名都需要项目批准人手动批准。
- 团队角色（作者、审核人、批准人）：待补充。
- 所有有提交权限的成员都启用 GitHub 双重验证。
- 签名证书上的发布者：待 SignPath 批准后补充。

## 从源码构建

需要 [Go](https://go.dev/dl/) 1.27 或更新版本（`go.mod` 固定了 1.27.3，`go` 命令会自动下载对应工具链）。

```sh
go test ./...                      # 单元测试和回放测试
go run ./cmd/logdiag-collector     # 开发时直接运行
scripts/build.sh v0.1.0            # 编译带版本资源的 Windows exe 到 dist/（Windows 上用 Git Bash）
```

同一提交在任何机器上编出的 exe 完全相同，可以自己编译后与 Release 里的 SHA-256 比对。界面在 `web/` 目录，是原生 HTML、CSS 和 JavaScript，不需要 npm。

## 测试与真机验证

各品牌接口靠两层测试保证：

- **回放测试**：`internal/redfish/testdata/` 里是录制下来的接口往返，测试时代替真实设备，每次提交自动运行。每份记录的 `note` 字段写明它是真机录制还是按文档合成的。
- **真机验证**：只能在能访问管理网的电脑上进行。用 `probe --record` 和 `collect --record` 录下真机的响应，检查后放进对应的 `testdata/`（整机收集流程在 `internal/drivers/testdata/`），再写回放测试。

自动回放通过不等于真机验收；已在真机上验证的品牌、型号和固件会写在发布说明里。

## 目录结构

```
cmd/logdiag-collector/   程序入口和子命令（ui、probe、collect、version）
internal/drivers/        识别设备、选择驱动、执行一次收集
internal/collect/        驱动公共部分：阶段进度、文件落盘与校验、manifest
internal/collect/huawei/ 华为、超聚变 iBMC 驱动
internal/collect/inspur/ 浪潮 M5/M6/M7 驱动
internal/redfish/        Redfish 会话、请求、任务轮询、文件下载
internal/httprec/        HTTP 录制与回放（诊断记录和回放测试共用）
internal/ui/             本机界面服务
internal/buildinfo/      版本信息
web/                     界面静态文件，编译时嵌入 exe
docs/schema/             manifest 的 JSON Schema（与 LogDiag 网站共用）
scripts/build.sh         编译 Windows exe
```

## 许可证

本项目以 [GPL-3.0-or-later](LICENSE) 授权：可以自由使用、修改和再发布，再发布修改版时必须同样开源。

“LogDiag”名称和图标不随代码授权。fork 后再发布请改用其他名称和图标，不要使用 LogDiag 的名义。
