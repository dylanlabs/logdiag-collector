## 下载与运行

下载 `LogDiagCollector-<版本>-win-x64.exe`，Windows 10/11 上双击即可运行，无需安装。

本版本**没有代码签名**，首次运行时 Windows 会提示“Windows 已保护你的电脑”：点 **更多信息 → 仍要运行**。详见 [README](https://github.com/dylanlabs/logdiag-collector#首次运行与安全提示)。

## 核对文件

在 PowerShell 里计算 SHA-256，与本页的 `SHA256SUMS` 比对：

```powershell
Get-FileHash .\LogDiagCollector-*.exe -Algorithm SHA256
```

如果本版本附有 GitHub 构建证明，可以用 GitHub CLI 核对 exe 确实由本仓库的源码编译：

```
gh attestation verify LogDiagCollector-<版本>-win-x64.exe --repo dylanlabs/logdiag-collector
```
