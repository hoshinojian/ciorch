# ciorch 编排器部署（deploy/）

三份产物 + 本说明：

- `ciorch.service`：systemd unit（User=ciorch、Restart=always、StateDirectory）
- `Caddyfile`：TLS 反代模板（`<PUBLIC_IP>.sslip.io` → 127.0.0.1:8080）
- `install.sh`：幂等安装脚本（**不含凭据**，只检查存在与 0600 权限）

## 防火墙（ufw）

编排器机器入站只开 22/80/443（§3.14）：

```sh
ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp    # 运维 ssh（可按来源收窄）
ufw allow 80/tcp    # Caddy ACME HTTP-01 + 重定向
ufw allow 443/tcp   # Caddy HTTPS（webhook 公网入口）
ufw enable
```

注意：runner 机器经同 VPC 私网回连编排器不需要公网入站；ciorch 的 HTTP
面只绑 127.0.0.1，公网流量全部经 Caddy。

## 完整部署/回滚步骤

见 `docs/runbook.md`（含 GitHub App 创建操作清单）。
