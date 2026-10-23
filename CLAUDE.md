# LogDiag Collector 开发约定

数据中心设备日志一键收集工具，LogDiag（logdiag.com）旗下的免费开源工具。Go 单文件 Windows exe + 内嵌本机网页界面，主要用户是带笔记本去机房的服务工程师（Windows 10/11）。

## 常用命令

```sh
go test ./...              # 单元测试和回放测试
go vet ./... && GOOS=windows go vet ./...
gofmt -l .                 # CI 要求无输出
scripts/build.sh [v0.1.0]  # 编译 dist/LogDiagCollector-<版本>-win-x64.exe
```

## 约定

- 面向用户的文字一律中文，只说结果和该怎么做，不讲协议细节。代码注释用中文，标识符用英文。
- 不主动联网：检查更新、上传、反馈都由用户点击触发。设备流量不走系统代理。
- 对设备只调用读取类和收集类接口；不调用清除日志、重启等写操作。
- 认证失败立即停止，不自动重试（防止 BMC 账号被锁）。会话用完必须删除（`Logout`），取消时也一样。
- 任务取消只停止本地等待，不去中断设备端任务。
- 新驱动放 `internal/collect/<品牌>/`，实现 `collect.Driver`，在 `internal/drivers` 的 `Brands` 里登记；驱动只管触发、等待、下载，文件落盘、校验和 manifest 用 `Job.Save`。日志类型编码（manifest 的 `log.type`）与网站共用，新增时同步改 `docs/schema/manifest-1.schema.json` 的示例。
- 新增设备支持必须附回放测试：录制记录放 `testdata/`（整机收集流程放 `internal/drivers/testdata/`），`note` 字段写明“真机录制：品牌 型号 固件”或“合成：依据哪份文档”。不要把合成记录当成真机验证结果。
- 诊断记录不能含密码、令牌、Cookie、设备 IP（`internal/httprec` 负责脱敏），新增凭据字段时同步更新 `redact.go` 的规则。
- 新文件加 `SPDX-License-Identifier: GPL-3.0-or-later` 头。依赖只用 MIT、BSD、Apache-2.0 等与 GPL-3.0 兼容的许可，不复制 GPL-3.0 以外许可的代码（例如浪潮 inspursmsdk 只参考接口行为）。
- 前端不引入 npm 构建链：原生 ES Module + CSS，视觉沿用 BMCKit 的设计令牌（`web/assets/app.css` 顶部）。
- 真机验证只能在用户的电脑上做，云端环境连不到客户管理网。
