# ciorch 运维手册（runbook）

维护者操作手册：GitHub App 创建 → 部署 → 日常运维 → 回滚。设计依据
`docs/REDESIGN.md`；工程细节见各节引用。

## 一、GitHub App 创建操作清单（只做一次）

1. 用维护者账号打开 **https://github.com/settings/apps/new**（Org 仓则在
   Org Settings → Developer settings → GitHub Apps → New GitHub App）。
2. 填写：
   - **GitHub App name**：`ciorch`（如被占用加后缀，如 `ciorch-orchestrator`）
   - **Homepage URL**：`https://github.com/hoshinojian/ciorch`
   - **Webhook URL**：`https://<编排器公网IP以-连接>.sslip.io/webhook`
     （例：IP `203.0.113.10` → `203-0-113-10.sslip.io`；部署后才有——可先
     填占位、部署后回改，或按"先部署后建 App"顺序执行 §二）
   - **Webhook secret**：随机 32 字节 hex（`openssl rand -hex 32`），存为
     编排器上的 `/etc/ciorch/webhook.secret`
3. 权限（Repository permissions）：
   - **Administration: Read and write**（铸 runner registration token、列/删 runner）
   - **Actions: Read and write**（读 runs/jobs/日志；rerun-failed-jobs）
   - **Issues: Read and write**（开/评论/关闭事故 issue）
   - Metadata: Read（默认自带）
4. 订阅事件（Subscribe to events）：勾 **Workflow job** 与 **Issues** 两个。
5. 点 **Create GitHub App**，随后：
   - 记下 **App ID**（About 面板）→ 填 config `github_app.app_id`
   - 页面底部 **Private keys → Generate a private key**，下载的 `.pem`
     存为编排器上的 `/etc/ciorch/app.pem`（chmod 600）
6. 安装（Install App）：**Only select repositories** → 先只选
   `hoshinojian/ciorch-sandbox`（**暂不选 nms**——阶段 6 切换前才加）。

## 二、部署步骤（编排器机器）

前置：DO 机器已由 vpsctl 建好（规格见 REDESIGN §3.14：与 runner 同账号
同区、最小 1vCPU 档、tag `ciorch-host`），Ubuntu 24.04，root 可登录。

1. 放二进制：本仓 CI 产物或本地 `go build`，拷到 `/usr/local/bin/ciorch`；
   同法放置 vpsctl 二进制与账号文件（路径写进 config 的 `vpsctl_bin` /
   `vpsctl_accounts_file`——账号文件**只含 CI 专用账号**条目）。
2. 手动放凭据（全部 `chmod 600`、属主 ciorch——install.sh 会检查）：
   - `/etc/ciorch/ciorch.json`（config；`github_app.app_id` 按上节）
   - `/etc/ciorch/app.pem`（App 私钥）
   - `/etc/ciorch/webhook.secret`
   - `/etc/ciorch/status.token`（`openssl rand -hex 24`）
   - `/etc/ciorch/ssh/id_ed25519`（编排器对 runner 的 ssh 私钥；公钥进
     `ssh_public_key_files`，另放维护者公钥）
   - `/etc/ciorch/deepseek.key`（阶段 5 事故管线用，可后放）
3. 运行 `deploy/install.sh`（幂等；凭据缺/权限错会报错退出）。
4. Caddy：`apt install caddy`，把 `deploy/Caddyfile` 中 `<PUBLIC_IP>`
   替换为实际 IP 放到 `/etc/caddy/Caddyfile`，`systemctl reload caddy`
   （证书自动申请——若 LE 对 sslip.io 签发不顺，改用自有域名）。
5. ufw 按 `deploy/README.md` 只开 22/80/443。
6. 回 GitHub App 设置页确认 Webhook URL 无误；仓变量配置：ciorch 仓
   Settings → Secrets and variables → Actions → Variables →
   `CIORCH_HOST = <ip>.sslip.io`（激活 healthcheck workflow）。
7. 验收：
   - `systemctl status ciorch`（active）
   - `curl -s localhost:8080/healthz` → `ok`
   - App 设置页 Recent Deliveries 出现 ping/workflow_job 且验签通过（200）
   - 沙箱仓 dispatch 一个 job → ciorch 日志出现该仓 reconcile 行
   - `ciorch check-config`（如已实现子命令）或 `plan` 手动核对

## 三、日常运维

- 状态：`curl -H "Authorization: Bearer $(cat /etc/ciorch/status.token)" localhost:8080/status`
- 日志：`journalctl -u ciorch -f`
- 健康外检：ciorch 仓 Actions → healthcheck（每小时自动；**60 天无仓内
  活动 GitHub 会停用 schedule**——长期无 PR 时定期 workflow_dispatch 一次）
- 事故处理：编排器开的 issue 带 `ciorch` 标签；处理完**关闭 issue** 即
  清熔断/解除 pause（阶段 5 语义）
- 手动建/回收：`ciorch up <repo>` / `ciorch down <repo>`（守护运行时会拒绝）

## 四、回滚步骤

1. 停服务：`systemctl stop ciorch`（runner 机器上的 job 不受影响——
   只是不再有自动建删）。
2. 回滚二进制：恢复上一版本到 `/usr/local/bin/ciorch`，`systemctl start ciorch`。
3. 回滚配置：`/etc/ciorch/ciorch.json` 有误 → 修正后 restart（config 错
   会在启动时校验失败退出，journal 有中文错误与字段路径）。
4. 彻底退役：停服务、关 App 安装（GitHub App 设置 → Uninstall）或删除
   webhook、`vpsctl` 清 tag `ciorch-host` 机器；state 目录 `/var/lib/ciorch`
   可归档后删除。
5. 灾难恢复：编排器机器丢失不影响在跑 CI（GitHub 派发不经过编排器）；
   重建后 state.json 缺失 → 机器冷启记忆清零但 droplet CreatedAt 仍是
   判龄依据，已在线机器不会被误判冷启超时。

## 五、已知门槛与注意

- **V1 已实测（2026-11-21）**：GitHub 拒绝删除忙碌 runner（422）——Drain
  的"先注销后删机"防竞态成立，无需额外 busy 复查。
- **V5**：LE 对 sslip.io 的签发在部署时验证，不顺则换自有域名。
- 编排器机器**绝不注册为 runner**（持有全部凭据；REDESIGN §2.2 原则 12）。
- runner 规格与区域不得改（nms：c2-4vcpu-8gb/sgp1，覆盖率口径依赖）。
