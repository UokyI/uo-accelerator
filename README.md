# uo-accelerator

GitHub 访问加速代理，纯 Go 实现，单文件运行。

## 使用

```powershell
.\uo-accelerator.exe
```

启动后自动探测最优 IP → 3 秒后自动设置系统代理 → 浏览器直接访问 GitHub。

**Ctrl+C** 退出并自动恢复代理。

## 原理

DoH DNS 解析 (AliDNS/DNSPod) → TCP 并发测速 → 择优路由 → 本地 HTTP/HTTPS 代理 (`:9910`)

## 配置

编辑 `config.yaml` 可自定义域名、端口、探测间隔等。

## 构建

```powershell
go build -o uo-accelerator.exe .
```
