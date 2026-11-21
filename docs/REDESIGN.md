# CI 执行面重设计：ciorch 编排器 + cirunner 机器侧

> 版本：2026-11-20 定稿 v1（维护者与 ZCode 讨论三轮后定案）。
> 读者：**接手写代码的大模型**（以下称"实现者"）。写完交回 ZCode 审查，审查口径见 §12。
> 本文是唯一设计依据：文中没写的设计结论不要自己发明，拿不准的先在 PR 描述里列"待确认"问维护者。
> 标"【待实测】"的条目是设计时无法确证的外部行为，实现前必须先实测，结论回写本文 §11。

---

## 0. 一页纸摘要

- **要做什么**：把"给我的所有 GitHub 项目按需起 CI 机器"做成一个常驻、便宜、会自愈、出事会自己开 issue 并让大模型诊断的系统。
- **两个仓**：
  - **ciorch（新仓，仓名以维护者最终决定为准，下文用 ciorch 指代）**——编排器/控制面。常驻在一台 ~$4/月 的小机上，**不跑任何 CI**。负责：接 GitHub webhook、观察队列、决定建/删机、自愈、预算、事故 → issue → DeepSeek 诊断 → 白名单动作。
  - **cirunner（本仓，重写）**——机器侧。负责：runner 机器的"配方"（cloud-init 模板、快照烘焙配方）和一个装在 runner 机器上的小命令 `cirunner`（health / diag / act），供编排器经 ssh 调用。
- **不改**：vpsctl（DO 建删管的唯一入口，子进程调用）；例外见 §9 阶段 7（需维护者批准）。
- **现状（2026-11-19 起）**：常驻手工 runner `gitrunner-sgp2` 已删除；nms 的 CI 目前**只靠** WSL 上的 cirunner 原型守护进程（钉死在 `~/.local/bin/cirunner-proto`，commit 508a0b0）。原型有一个会让 CI 永久卡住的缺陷（§1.4 #2），**第一个 PR 就是给原型打热补丁**（§9 阶段 0）。

---

## 1. 背景与动机

### 1.1 沿革

| 时间 | 状态 | 问题 |
|---|---|---|
| ≤2026-11-12 | GitHub 托管 runner | 私有仓 Actions 分钟数耗尽，全仓 CI 停摆 |
| 2026-11-13 | 自建常驻 runner `gitrunner-sgp2`（DO c2-4vcpu-8gb，$94/月，nms 决策 #145） | DO **关机不停止计费**，只有删机省钱；24h 常驻但每天只用几小时；单机串行；会静默停摆（pitfalls P139） |
| 2026-11-15 | cirunner 原型：WSL 上的守护进程，按队列建删机 | 见 §1.4，雏形问题多；守护在 WSL，WSL 睡眠 = CI 不起 |
| 2026-11-19 | sgp2 删除，原型独立承载 | 原型缺陷暴露面变大 |
| 本设计 | ciorch + cirunner | — |

### 1.2 目标（按优先级）

1. **快**：PR 推上去到 CI 出结果的总时间尽量短。具体：排队 → 开跑 ≤ 冷启时间（目标 ≤ 3 分钟，快照后 ≤ 1.5 分钟）；7 个 job 不再被单机串行拖长。
2. **强（自愈）**：任何"有活排队但没人干"的状态，在分钟级被发现并自动修复；修不好就开 issue 叫人，**绝不沉默**。
3. **多项目**：一套编排器服务维护者所有 GitHub 仓，加一个项目 = 加一段配置 + 装 App。
4. **省钱**：没活时只有编排器小机在花钱（~$4/月）；runner 机器按需建、空闲删；有硬预算上限。
5. **可审计**：每个决策有日志可追溯，每次事故有 issue 留痕。

非目标：不做 Web UI；不支持 GitHub 托管 runner 调度；不支持 Windows/macOS runner；不做跨云（只 DO，经 vpsctl）。

### 1.3 实测事实（设计依据，2026-11-17～19 日志与 jobs API）

- **冷启动实测约 2.3 分钟**（建机命令返回 → 首个 job 开跑），从 run 排队算约 3 分钟。README 写的 4~6 分钟偏保守。例：11-18 03:41:12 建机返回，03:43:29 首个 job 开跑。
- **run 级状态不可靠**（nms pitfalls P141②）：run 的 `status` 聚合对自建 runner 有滞后，会"停在 queued"而 job 实际在跑；`?status=in_progress` 过滤可能返回 0。**判据必须用 jobs 接口**。
- **一个 run 有 7 个并行 job**（nms：lint / test / build / scripts / gitleaks / sqlc / large-files），test 约 12 分钟是关键路径，其余 6 个合计约 5 分钟。单 runner 时 test 可能排在 6 个轻 job 后面干等（11-18 run 44237158062：test 排队 3 分多钟）。
- **P139 静默停摆**：runner online、busy=false、但不接活，journal 零日志，重启 runner 服务即愈。机器侧"日志静默"不能作判据（test job 跑的十几分钟本来就安静）。
- **空闲删机后很快又来活**：11-17 15:25 空闲删机，15:47 又建，多付一次冷启。c2-4vcpu-8gb 约 $0.14/h，多留 25 分钟约 $0.06。
- **GitHub/网络瞬时错误常见**：日志里有 TLS handshake timeout、HTTP 503。不能因为一次观察失败就做决策，更不能删机。
- WSL 出站 22 端口被选择性阻断（nms pitfalls P3）；DO 机器之间无此问题。vpsctl `list` 输出含 `ipv4_private`（同区同账号同 VPC 可内网直连）。
- vpsctl `create` 的 `-wait` 缺省 0 = 不等待，建机 API 受理即返回（含 droplet id，可能尚无 IP）。

### 1.4 原型（cirunner@508a0b0）缺陷清单

| # | 缺陷 | 位置 | 后果 |
|---|---|---|---|
| 1 | 冷启失败满 2 次永久停建：计数只在出现活跃 runner 时清零，而停建后不会再出现 | `internal/decide/decide.go:145,171` | 死锁，只能手改 states.json |
| 2 | **只数 `status=queued` 的 run**：run 一旦有一个 job 开跑即变 `in_progress`，其余排队 job 不可见 | `internal/githubapi/client.go:50` | ① 不扩容、停摆检测不到；② **致命**：runner 中途挂掉/假活 → 20 分钟后被当空闲删掉 → 剩余 job 所在 run 是 in_progress → 原型永不再建机 → **job 永久卡住**（sgp2 在时被它兜底，现在无兜底） |
| 3 | 完全不看 label | decide 全局 | 需要别的 label 的 job（如 nms `testenv-bootstrap.yml` 的 `ubuntu-latest`）被当需求：机器不回收，甚至"判停摆→删→建"循环 |
| 4 | 建机同步阻塞（`-wait 5m`、ctx 8m），各仓串行 | `internal/engine/engine.go:234` | 建机期间整个循环停转 |
| 5 | 状态按仓一份（PendingSince/IdleSince） | `decide.go:37` | 多机时第二台冷启失败永远检测不到；冷启在途不允许再建 |
| 6 | 一轮内一个动作失败，后续动作全放弃 | `engine.go:148` | 清注册失败 → 删机也不做 |
| 7 | 空闲删机竞态：读到 busy=false 与删机之间 job 刚派到 | `decide.go:187` | 正跑的 job 被杀（runner lost communication） |
| 8 | 告警只写一行日志 | `engine.go:186` | 没人知道 |
| 9 | CLI 与守护并发读写 states.json 无锁 | `cmd/cirunner/main.go` | 状态互相覆盖 |
| 10 | 无机器最长寿命、无 job 卡死处置；nms ci.yml 无 `timeout-minutes`（默认 6h） | — | 卡死 job 占机 6 小时 |
| 11 | 停摆判据：排队 >15min 且连续 2 次 60s 采样 → 约 17 分钟才反应 | `decide.go:174` | 慢 |
| 12 | 冷启超时 15 分钟（实测 2.5 分钟就该上线） | `decide.go:76` | 慢 |
| 13 | 用维护者个人 `gh` 登录态 | `githubapi` | 不适合常驻服务器、多项目 |

原型里**要保留的资产**：`internal/cloudinit` 模板里固化的踩坑修复（§5.3 逐条列出）、vpsctl 调用方式（`-only` 钉账号、`-start-index` 预定名、`-ids account/id -confirm 1` 精确删）、配置校验风格、纯函数 decide + 表驱动单测的结构。

---

## 2. 总体架构

### 2.1 组件与职责

```
                    ┌──────────────────────────── GitHub ────────────────────────────┐
                    │  仓 A / 仓 B / ...（装了 ciorch GitHub App）                      │
                    │  job 排队 ──workflow_job webhook──┐      job 派发给已注册 runner   │
                    └───────────────────────────────────┼──────────────▲──────────────┘
                                                        │              │ (runner 主动长轮询 GitHub)
                     HTTPS（Caddy 终结 TLS）             ▼              │
┌─────────────── ciorch 编排器（DO 最小规格，常驻，不跑 CI）────────────┐ │
│ webhook 收件 ─┐                                                      │ │
│ 轮询兜底 ─────┼→ observe → decide（纯函数）→ execute ──Provider（当前：vpsctl/DO）──┼─┼→ DO：建/删 runner 机器
│               │                    │                                  │ │
│               │                    └→ incident 状态机 → GitHub issue   │ │
│               │                                  └→ DeepSeek API（只出主意）
│ ssh（内网 IP）─────────────────────────→ runner 机器上的 `cirunner diag/act`
└──────────────────────────────────────────────────────────────────────┘ │
                                                                          │
┌──── runner 机器（按需建、用完删；一台只服务一个仓）─────────────────────┐ │
│ cloud-init（cirunner 配方）装底座 + 注册 runner ─────────────────────────┼─┘
│ /usr/local/bin/cirunner（health / diag / act）                          │
└─────────────────────────────────────────────────────────────────────────┘
```

**关键认知**：编排器**不接任务、不转发任务**。GitHub 只把 job 派给已注册的 runner；编排器做的是"看见排队 → 准备好 runner → 盯着它们 → 用完回收"。

### 2.2 设计原则（实现时逐条遵守）

1. **判据只用 GitHub 侧信号**（P139）：就绪 = runner online；空闲 = busy=false；需求 = jobs 接口里的 queued job。机器侧信息（diag）只用于诊断，不用于调度判定。
2. **job 粒度，不是 run 粒度**（P141）。
3. **按 label 匹配需求**：只有 job 的 labels 能被某仓 profile 满足时才算需求。
4. **按机器记状态**，不按仓。
5. **先建后拆**：替换故障机时，有配额就先建新机，再处理旧机，不让队列空窗。
6. **先注销后删机**：缩容时先删 GitHub 注册，成功后才删 droplet（防 #7 竞态，见 §11 待实测 V1）。
7. **观察不完整整轮冻结**：任何一路观察失败（jobs / runners / droplets 分路记录成败），本轮不建、不删、不 ssh、不推进任何采样计数（OfflineSamples/StallSamples/IdleSince/BusySince）、不清理 Machines 记忆——只累计场景 H；droplets 一路成功时仍执行硬寿命回收（防成本泄漏）。
   （2026-11-20 复审修订：原口径"只建不删"在 droplets 一路缺失时会把 N 当 0 而超建、把机器记忆清空，故收紧为冻结。）
8. **动作互相隔离**：一个动作失败只记日志 + 计入退避，不影响同轮其他动作。
9. **大模型只出主意不动手**：拿不到任何凭据和 shell，只返回结构化结论；编排器校验后在硬上限内执行白名单动作。
10. **钱有硬上限**：全局并发机器数、每仓上限、每日预算，由确定性代码执行，大模型无法突破。
11. **一台 runner 机器只服务一个仓**：仓 A 的 CI 代码看不到仓 B 的任何东西。
12. **编排器机器本身绝不注册为 runner**：它持有 DO token、App 私钥、DeepSeek key，而 CI 跑的是 PR 里的任意代码。
13. **机器来源经 Provider 接口**（机器提供商抽象，见 [PROVIDER-ABSTRACTION.md](PROVIDER-ABSTRACTION.md)）：业务包只依赖 `internal/machine` 的提供商无关模型，不感知 vpsctl/DO 或将来的自建虚拟化；`internal/provider/*` 实现与 `cmd/` 装配层之外不得 import 具体实现（CI 检查强制）。

### 2.3 凭据放哪里

| 凭据 | 位置 | 说明 |
|---|---|---|
| GitHub App 私钥（PEM）、App ID、webhook secret | 编排器 `/etc/ciorch/`，属主 ciorch 用户，0600 | 用于签 JWT 换 installation token |
| DO token | 编排器上的 vpsctl 账号文件（**只含 CI 专用账号条目**，如 team2-ci，不放其他账号和密码） | vpsctl `-accounts` 指定 |
| 编排器 ssh 私钥 | 编排器 `/etc/ciorch/ssh/id_ed25519` | 对应公钥经 cloud-init 写入 runner 机器 root 的 authorized_keys |
| DeepSeek API key | 编排器 `/etc/ciorch/deepseek.key` | 服务商侧设月度上限 |
| 维护者个人 ssh 公钥 | 配置里，经 cloud-init 写入 runner 机器 | 人工救援用 |
| runner registration token | 建机瞬间铸、写入 user-data，1 小时失效 | 维护者明确：日志/诊断/issue **不做脱敏** |

**runner 机器上不放任何长期凭据**（只有 1h 时效的 registration token 出现在 cloud-init 日志里）。

---

## 3. ciorch（新仓）详细设计

### 3.1 技术约束

- Go（版本与 cirunner 一致：go 1.26.8），**尽量只用标准库**（net/http、crypto/*、log/slog、encoding/json）。允许的外部依赖：`github.com/hoshinojian/cirunner`（公开仓，用其 `pkg/cloudinit`）。不引入 x/crypto/ssh——ssh 走系统 `ssh` 子进程。
- 外部系统一律接口注入，便于 fake 测试：`GitHub`、`VPS`、`SSH`、`LLM`、`Clock`。
- 所有时间判断用注入的 `Clock`，decide 纯函数只接收 `Now`。

### 3.2 仓布局

```
cmd/ciorch/            CLI 入口（子命令见 §3.13）
internal/config/       配置加载、默认值、校验
internal/githubapp/    App JWT、installation token 缓存、REST 客户端（ETag、限流、重试）、webhook 验签与解析
internal/observe/      把 GitHub + vpsctl 的原始数据拼成 decide.Obs（job 级 + label 匹配）
internal/decide/       纯决策：需求计算、扩缩容、自愈矩阵、熔断；无 IO
internal/engine/       执行动作：建机、删机、注销、ssh 动作、退避；动作隔离
internal/budget/       全局配额、每日花费估算
internal/incident/     事故状态机、issue 读写、诊断材料采集、调用 LLM、校验结论、执行白名单动作
internal/llm/          DeepSeek 客户端（OpenAI 兼容 chat/completions + JSON 模式）
internal/machine/     提供商无关机器模型与 Provider/Namer 接口、Registry、契约测试套件（machinetest）
internal/provider/    提供商实现（业务包不得 import；cmd/ 装配层除外）
  └─ vpsctl/          vpsctl（DO）实现：子进程封装（自原型迁入并改造：create 不等待）+ 池配置 Factory
internal/sshx/         系统 ssh 子进程封装
internal/state/        持久化（原子写 + flock）
internal/server/       HTTP：/webhook、/healthz、/status
internal/stats/        jobs 历史统计（排队等待分布、冷启耗时）
deploy/                systemd unit、Caddyfile 模板、安装脚本
.github/workflows/     编排器自身 CI（托管 runner，见 §3.15 的分钟数说明）+ 健康外检 workflow
docs/                  本设计的副本 + runbook
```

### 3.3 配置

路径：`/etc/ciorch/ciorch.json`（开发时可 `-config` 指定）。机器来源池化（2026-11-21 机器提供商抽象，[PROVIDER-ABSTRACTION.md](PROVIDER-ABSTRACTION.md) §3.3）。示例（**仓内示例文件全部用占位符**，完整见 `configs/ciorch.example.json`）：

```json
{
  "github_app": {
    "app_id": 123456,
    "private_key_file": "/etc/ciorch/app.pem",
    "webhook_secret_file": "/etc/ciorch/webhook.secret"
  },
  "pools": {
    "do-sgp1-c2": {
      "provider": "vpsctl",
      "max_machines": 2,
      "vpsctl": {
        "bin": "/usr/local/bin/vpsctl",
        "accounts_file": "/etc/ciorch/vpsctl-accounts.json",
        "account": "<CI 专用账号>",
        "region": "sgp1",
        "size": "c2-4vcpu-8gb",
        "image": "ubuntu-24-04-x64",
        "snapshot": "",
        "prefer_private_ip": true,
        "hourly_cost_hint": 0.21
      }
    }
  },
  "ssh_private_key_file": "/etc/ciorch/ssh/id_ed25519",
  "ssh_public_key_files": ["/etc/ciorch/ssh/id_ed25519.pub", "/etc/ciorch/maintainer.pub"],
  "machine_agent": { "version": "v0.1.0", "sha256": "<release 资产 sha256>" },
  "listen": "127.0.0.1:8080",
  "state_dir": "/var/lib/ciorch",
  "global": {
    "max_machines": 2,
    "daily_budget_usd": 5.0
  },
  "llm": {
    "base_url": "https://api.deepseek.com",
    "model": "deepseek-chat",
    "api_key_file": "/etc/ciorch/deepseek.key",
    "max_calls_per_incident": 3,
    "max_calls_per_day": 20
  },
  "maintainer_login": "hoshinojian",
  "profiles": {
    "go-heavy": {
      "pools": ["do-sgp1-c2"],
      "labels": ["ci"],
      "runners_per_droplet": 1,
      "extra_packages": []
    }
  },
  "repos": [
    {
      "repo": "hoshinojian/nms",
      "profile": "go-heavy",
      "max_runners": 2,
      "issue_repo": ""
    }
  ]
}
```

字段约束（`internal/config` 校验，错误信息中文、指出字段路径；池私有配置由对应 provider 的 Factory 校验）：

- **池（pool）**：`provider` 选实现（须已注册）；同名子对象放该实现的私有配置（只有对应实现解析，其他字段一律拒绝）；`max_machines` 是池容量上限（自有机器的物理上限；DO 池可等于全局上限）。
- **profile**：只描述 runner（labels、每机 runner 数）和用哪些池。`pools` 是**有序列表**，为将来"本地优先、DO 溢出"预留；**当前校验强制长度为 1**，长度 >1 报"多池调度尚未实现"。
- `global.max_machines` ≥ 任一仓的 `max_runners`；`Σ 池容量` 与全局上限的关系不强制；`state_dir` 必填。
- `labels`：`^[A-Za-z0-9_-]+$`；`self-hosted`、`linux`、`x64` 为注册自动标签，不必写。
- `runners_per_droplet` 默认 1，取值 1~2（2 为实验特性，§3.6.4）。
- `extra_packages` 底座五包（docker.io/jq/curl/build-essential/shellcheck）之外追加的包（§5.3 坑 10；渲染器接入时新增，2026-11-21）。
- `issue_repo` 空 = 在 `repo` 本身开 issue；非空 = 改投到指定仓（给不想在本仓开 issue 的项目用）。
- vpsctl 池私有配置：`bin/accounts_file/account/region/size/image` 必填（`account` 钉 vpsctl `-only`，防全账号误建——2026-11-13 教训）；`snapshot` 非空时建机用它替代 image（阶段 7）；`prefer_private_ip` 缺省 true；`hourly_cost_hint` **必须 >0**（空池建机的预算投影不能当 0，§3.7）。
- 对 nms：**必须保持 c2-4vcpu-8gb / sgp1**（专用核：共享核两次击穿 `-race` 时敏用例；同规格同区避免覆盖率基线重校，nms 决策 #148）。
- 旧 schema（顶层 `vpsctl_bin`、`global.max_droplets`、profile 内建机参数等）不做兼容解析，检出即给迁移提示（指向 PROVIDER-ABSTRACTION.md §3.3）。

决策参数（全部有默认值，可在 `global.tuning` 下覆盖，名字与 §3.6.6 参数表一致）。

### 3.4 GitHub 接入（GitHub App）

#### 3.4.1 App 权限（维护者在 GitHub 网页创建 App，实现者在 runbook 里写清操作清单）

- Repository permissions：
  - **Administration: Read and write**——铸 runner registration token、列/删 runner
  - **Actions: Read and write**——读 runs/jobs/日志；write 用于 `rerun_failed_jobs` 动作
  - **Issues: Read and write**——开/评论/打标签/关闭 issue
  - Metadata: Read（默认）
- 订阅事件：**Workflow job**、**Issues**
- Webhook URL：`https://<编排器主机名>/webhook`；secret 随机 32 字节
- 安装：Only select repositories → 勾选要托管的仓

#### 3.4.2 认证

- JWT：RS256（标准库 `crypto/rsa` + `crypto/sha256` 手写），header `{"alg":"RS256","typ":"JWT"}`，claims `iat=now-60s`、`exp=now+9min`、`iss=<app_id>`。
- 每仓 installation id：`GET /repos/{owner}/{repo}/installation`，缓存（重启重查）。
- installation token：`POST /app/installations/{id}/access_tokens`，缓存到过期前 5 分钟；收到 401 立即作废重取一次。
- 所有请求带 `Accept: application/vnd.github+json`、`X-GitHub-Api-Version: 2022-11-28`、`User-Agent: ciorch`。

#### 3.4.3 REST 客户端行为

- 每请求超时 15s；对 5xx、网络错误、超时：同一轮内退避重试 2 次（0.5s、1.5s，带抖动）；4xx 不重试（401 除外，见上）。
- **ETag 条件请求**：对 GET 缓存 `ETag` + body（按 URL 键），带 `If-None-Match`；304 直接用缓存 body（304 是否计入限流【待实测 V3】，无论结果都保留此机制）。
- 读 `X-RateLimit-Remaining` / `X-RateLimit-Reset`：remaining < 10% 时轮询间隔自动乘 4，并打 WARN；收到 403/429 且带 `Retry-After` 时遵守。
- 分页：用到的列表接口 `per_page=100`，跟随 `Link: rel="next"`（实际很少超过一页）。

#### 3.4.4 用到的接口

| 用途 | 接口 |
|---|---|
| 近期未完成的 run | `GET /repos/{r}/actions/runs?per_page=100&created=>={now-24h, RFC3339}`，客户端过滤 `status != completed`（**不依赖 status 过滤参数**，P141） |
| run 的 job | `GET /repos/{r}/actions/runs/{run_id}/jobs?filter=latest&per_page=100` |
| runner 列表（含 labels） | `GET /repos/{r}/actions/runners?per_page=100` |
| 铸注册 token | `POST /repos/{r}/actions/runners/registration-token` |
| 删注册 | `DELETE /repos/{r}/actions/runners/{runner_id}` |
| job 日志（诊断） | `GET /repos/{r}/actions/jobs/{job_id}/logs`（302 跳转下载，取末尾 N KB） |
| 重跑失败 job | `POST /repos/{r}/actions/runs/{run_id}/rerun-failed-jobs` |
| issue | `GET /repos/{r}/issues?labels=ciorch&state=open`、`POST /repos/{r}/issues`、`POST .../issues/{n}/comments`、`POST .../issues/{n}/labels`、`PATCH .../issues/{n}`（关闭） |

#### 3.4.5 Webhook

- 端点 `POST /webhook`，body 上限 5MB。
- 验签：`X-Hub-Signature-256: sha256=<hex>`，HMAC-SHA256(secret, raw body)，`hmac.Equal` 常量时间比较；不通过返回 401 且不解析 body。
- 事件处理（按 `X-GitHub-Event`）：
  - `ping` → 200。
  - `workflow_job`：取 `repository.full_name`；不在白名单 → 200 丢弃。在白名单 → 把 job 快照（id、run_id、status、labels、created_at、started_at、runner_name）并入该仓的"webhook job 缓存"，并**触发该仓立即 reconcile**（2s 防抖合并同一波事件）。
  - `issues`：`action=closed` 且 issue 带 `ciorch` 标签 → 通知 incident 模块"人工已处理"（§3.10.6）。其余忽略。
  - 其他事件 → 200 忽略。
- 返回要快（<1s）：只入队，不在请求里做 reconcile。
- webhook 可能丢（App 网络抖动、编排器重启），**轮询是兜底和真相来源**：webhook 只决定"什么时候立即看一眼"，判定数据仍以 REST 观察为准（webhook job 缓存只用于在轮询结果里补齐刚出现、run 列表尚未反映的 job）。

### 3.5 观察模型（internal/observe → decide.Obs）

每仓每轮：

1. 列近 24h 未完成 run → 对每个 run 拉 jobs（filter=latest）。
2. 合并 webhook job 缓存（同 job id 以 `updated` 更新者为准；缓存条目 2 小时过期）。
3. 只保留 job `status ∈ {queued, in_progress}`；`waiting`（环境审批）、`requested`、`pending` 不算需求但记录计数（status 输出用；2026-11-20 确认落点为 `Obs.PendingJobs` 字段）。
4. **label 匹配**：job 的 labels（小写化）⊆ `{self-hosted, linux, x64} ∪ profile.labels`（小写化）→ `Matchable=true`。不可匹配的 queued job 记入 `UnmatchedQueued`（场景 E）。
5. 列 runner（含 labels、status、busy）。
6. 机器一路：经该仓所属池的 `machine.Provider.List(仓 tag)` 列本仓机器（含 Pool、CreatedAt、Addr、HourlyCost——提供商无关视图）。
7. 任一步失败：`Obs.Complete=false`，分路记录 `JobsOK/RunnersOK/MachinesOK`，其余路照采。decide 对不完整观察**整轮冻结**（见 §2.2 原则 7 修订）——原"只建不删"在机器一路缺失时会超建，已收紧。

```go
type Obs struct {
    Now        time.Time
    Complete   bool          // 三路观察全部成功
    Jobs       []Job         // 未完成且 Matchable 的 job（queued + in_progress）
    UnmatchedQueued []Job    // labels 无法满足的 queued job
    Runners    []Runner      // 本仓全部注册（含外部）
    Machines   []machine.Machine // 本仓 tag 下的机器（提供商无关视图，含 Pool/Addr/HourlyCost）
}
type Job struct {
    ID, RunID  int64
    Status     string        // queued | in_progress
    Labels     []string
    CreatedAt  time.Time     // 本 attempt 的 job 创建时间，作排队起点
    StartedAt  time.Time
    RunnerName string
}
type Runner struct {
    ID     int64
    Name   string
    Status string            // online | offline
    Busy   bool
    Labels []string
}
type Droplet struct {
    ID, Name     string
    CreatedAt    time.Time
    PrivateIP    string
    PublicIP     string
    PriceMonthly float64
}
```

**归属与命名**（替换原型的 `repo:<短名>` tag——多项目下短名会撞）：

- 公共 tag：`ciorch`
- 仓 tag：`ciorch-r-<sanitize(owner)>-<sanitize(name)>-<sha256(owner/name) 前 8 位 hex>`；sanitize = 小写、非 `[a-z0-9]` 替换为 `-`、截断到总长 ≤ 64。
- droplet 名前缀（vpsctl `-name-prefix`）：`ci-<sanitize(name) 截断 20>`；vpsctl 命名为 `{prefix}-{account}-{region}-NN`；**droplet 名 = runner 名**。
- 归属判定：droplet 带本仓 tag 才是"本仓的"；runner 名能对上本仓 droplet 名，或符合本仓命名空间（前缀 + `-{account}-{region}-\d+`）才是"本仓所辖"。**命名空间外的 runner/droplet 绝不触碰**。
- 编排器机器本身用 tag `ciorch-host`（不匹配任何仓 tag）。

### 3.6 决策（internal/decide，纯函数）

```go
func Decide(p Params, st RepoState, obs Obs) ([]Action, RepoState)
```

相同输入恒定输出；不读时钟、不做 IO、不打日志（理由写进 Action.Reason）。

#### 3.6.1 状态

```go
type RepoState struct {
    Machines   map[string]MachineState // key = droplet 名
    Breaker    Breaker
    CreateBackoffUntil time.Time
    CreateFailStreak   int
    LastActivity []time.Time           // 近 2h 有 job 开跑的时间点（自适应空闲用，保留最近 20 个）
}
type MachineState struct {
    FirstSeen      time.Time // 编排器首次看到该 droplet
    FirstOnlineAt  time.Time // runner 首次 online
    IdleSince      time.Time // 连续 busy=false 的起点；忙即清零
    StallSamples   int       // 场景 A 连续命中次数
    OfflineSamples int       // 场景 C 连续 offline 次数
    BusySince      time.Time // 当前这次 busy 的起点（场景 D）
    Remedy         string    // 正在进行的补救：""|"restarting"|"replacing"|"draining"
    RemedyAt       time.Time
    Bad            bool      // 已判死，等待回收
}
type Breaker struct {
    State     string    // closed | open | half-open
    Fails     int       // 连续冷启失败
    OpenedAt  time.Time
    Cooldown  time.Duration // 当前冷却时长；回 open 时翻倍封顶 4h（0=默认 30m）
}
// 2026-11-20 复审补充（§3.6.5 升级判据所需，结构体未列的必要记忆）：
// RepoState 增加 StallEvents/OfflineEvents []time.Time（A/C 的"24h 内第 2 次"
// 升级判据）与 ObserveFailStreak int（H 的连续失败轮数）。
```

冷启计时以 droplet `CreatedAt` 为准（守护重启不丢）。droplet 消失 → 删除其 MachineState。

#### 3.6.2 需求与供给

- `Q` = Matchable 且 status=queued 的 job 数；`QAge` = 其中最老一个的 `Now - CreatedAt`。
- `I` = 本仓所辖、online、busy=false、非 Bad 的 runner 数 × 1（每个 runner 一次只接一个 job）。
- `P` = 冷启在途（droplet 存在、runner 从未 online、未超时）的机器数 × `runners_per_droplet`。
- `N` = 本仓现有 droplet 数（含 Bad 但尚未删的）。
- 扩容量：`want = clamp(Q - I - P, 0, max_runners*runners_per_droplet - N*runners_per_droplet) / runners_per_droplet`（向上取整），再受全局配额、每日预算、熔断、退避约束（§3.7）。
- **不设"给空闲 runner 一点时间先接"的宽限**：GitHub 派发是秒级的，下一轮（≤15s）I 和 Q 都会更新；宁可偶尔多建一台（约 $0.14/h，空闲后自然回收），换取速度。

#### 3.6.3 扩缩容规则

1. **扩容**：`want > 0` → 产出 `want` 个 `Create`。允许同仓同时多台冷启。
2. **缩容（逐台）**：某台 runner online、busy=false 且 `Now - IdleSince ≥ idleTimeout`，并且 `Q == 0`、`Obs.Complete` → 产出 `Drain(该机)`（= 先注销注册，成功后删 droplet，见 §3.8）。
   - `idleTimeout` 自适应：`LastActivity` 中落在近 2h 内的点 ≥ 2 个 → `idle_timeout_busy`（默认 45m），否则 `idle_timeout`（默认 20m）。
   - 多台同时满足时，一轮只回收一台（最老的），避免抖动。
3. **硬寿命**：droplet 年龄 ≥ `max_lifetime`（默认 8h）且 runner busy=false → `Drain`；即使 `Obs.Complete=false` 也执行（防成本泄漏）。年龄 ≥ `max_lifetime + 2h` 无论忙闲 → 告警（场景 D 的变体）。
4. **Q>0 时绝不缩容**（硬寿命除外）。

#### 3.6.4 runners_per_droplet = 2（实验特性，默认关）

同一台机器装两个 runner 实例（名 `<droplet 名>-a` / `-b`），不加钱翻倍并行。风险：nms 的 test job 跑 `-race -p 4`，与 lint/build 抢 CPU 可能击穿时敏用例（共享核教训）。**默认 1**；实现时支持，但打开须先用 `ciorch stats` 在沙箱仓对比 20 次以上 run 的稳定性，结论交维护者决定。runner 名与 droplet 名的对应规则相应扩展（去掉 `-a/-b` 后缀再对 droplet 名）。

#### 3.6.5 自愈矩阵（"有活排队但没人干"及相关故障）

每轮按下表逐台机器、逐场景判定。**所有场景先判定、后汇总动作**；同一台机器同一轮只采取一个补救动作，优先级 C > B > A > D。

| 场景 | 判据（参数见 §3.6.6） | 固定逻辑处置 | 何时升级为事故（§3.10） |
|---|---|---|---|
| **A 假活**（P139：online 不接活） | `Q>0` 且 `QAge ≥ stall_queue_age`(2m)，该 runner online、busy=false、`Now-FirstOnlineAt ≥ stall_min_online`(60s)，连续 `stall_samples`(3) 轮成立 | ① 有配额 → 立即 `Create` 替换机（先建后拆）；② 同时 `SSHAct(restart-runner)`，`Remedy=restarting`；③ 重启后 `stall_restart_grace`(2m) 内该 runner 接到活（busy=true）→ 解除；仍不接 → 标 `Bad` → `Drain`（注销+删机） | 同一仓 24h 内第 2 次发生；或重启失败且无配额建替换机 |
| **B 冷启失败** | droplet 年龄 ≥ `cold_start_timeout`(8m) 且 runner 从未 online | ① 有配额 → `Create` 替换；② 删旧机（不必注销，从未注册成功；若有同名 offline 注册一并删）；③ `Breaker.Fails++` | 熔断打开时（见下） |
| **C 掉线** | 曾 online，当前 offline（或注册消失）连续 `offline_samples`(2) 轮 | 同 B（先建后拆），不计熔断 | 同一台机器以外，同仓 24h 内第 2 次 |
| **D job 卡死** | runner busy 持续 ≥ `job_stuck_alert`(60m) | 只升级事故；≥ `job_stuck_kill`(90m) → `Drain`（强删：先删 droplet 再删注册——job 已判死） | ≥60m 即开事故 |
| **E label 无人能满足** | `UnmatchedQueued` 非空且最老 ≥ 5m | 不建机 | 立即（每仓每种 label 组合一个事故，配置错误） |
| **F 配额/预算用尽** | `want>0` 但全局配额或每日预算不足，且 `QAge ≥ 10m` | 先尝试回收其他仓的空闲机（§3.7） | 回收后仍不足且 `QAge ≥ 15m` |
| **G 外部 runner 假活** | 同 A 的判据，但 runner 不在本仓命名空间内 | **不操作** | 立即（只告知） |
| **H 观察失败** | 连续 `observe_fail_alert`(10) 轮 `Complete=false`（约 2.5 分钟以上） | 本仓只建不删 | 连续 40 轮（约 10 分钟） |
| **I 建机报错** | vpsctl create 返回错误 | 指数退避：1m、2m、4m、8m、15m 封顶，`CreateFailStreak++` | 连续 3 次 |
| **J 孤儿机器** | 带 `ciorch` 公共 tag 但无任何已配置仓 tag 的 droplet | **不删** | 立即（等人确认） |

**熔断器**（替代原型的永久停建）：

- closed：正常建机。场景 B 使 `Fails++`；runner 首次 online 使 `Fails=0`。
- `Fails ≥ breaker_max_fails`(3) → open：停止该仓建机，开事故。
- open 持续 `breaker_cooldown`(30m) → half-open：允许 1 台试探建机；它 online → closed 且 `Fails=0`；它冷启失败 → 回 open，冷却时间翻倍（封顶 4h）。
- 人工关闭对应 issue → 直接 closed、`Fails=0`。
- CLI `ciorch reset <repo>` 同上。

#### 3.6.6 参数默认值

| 参数 | 默认 | 说明 |
|---|---|---|
| `poll_active` | 15s | 有 Q>0、冷启在途、有 busy runner 或有未结事故时的轮询间隔 |
| `poll_idle` | 60s | 全部空闲时 |
| `stall_queue_age` | 2m | A：最老匹配排队 job 年龄阈值 |
| `stall_min_online` | 60s | A：runner 上线不足此时长不判（刚上线还在拉活） |
| `stall_samples` | 3 | A：连续命中轮数（15s 一轮 ≈ 45s） |
| `stall_restart_grace` | 2m | A：重启后观察窗口 |
| `cold_start_timeout` | 8m | B（实测 ~2.5m 上线；快照后可降到 5m） |
| `offline_samples` | 2 | C |
| `job_stuck_alert` / `job_stuck_kill` | 60m / 90m | D |
| `idle_timeout` / `idle_timeout_busy` | 20m / 45m | 缩容 |
| `max_lifetime` | 8h | 硬寿命 |
| `breaker_max_fails` / `breaker_cooldown` | 3 / 30m | 熔断 |
| `create_backoff_max` | 15m | I |
| `observe_fail_alert` | 10 轮 | H |

### 3.7 预算与配额（internal/budget）

机器提供商抽象后的口径（[PROVIDER-ABSTRACTION.md](PROVIDER-ABSTRACTION.md) §3.4）：

- **成本**：`Machine.HourlyCost` 由提供商给出（vpsctl 按 `price_monthly / 672`，DO 月封顶 672 小时口径折算）；budget 包不出现月价与 672。自有机器可为 0。
- **全局并发**：所有仓机器总数 ≤ `global.max_machines`。统计方式：对每个池 `List(公共标记)` 后按 (池, ID) 去重。
- **池容量**：目标池在建机前同时过 `pools.<名>.max_machines`（在途占坑同样计入）。
- **每日预算**：当日花费 = Σ 每台机器当日存活小时 × HourlyCost（按 UTC 自然日，状态里累计已删机器的部分）。预计建一台后超过 `daily_budget_usd` → 不建，触发场景 F。"预计建一台"按**新机 1 小时**计，价格取**目标池最近一台机器的 HourlyCost**；池空时用池配置 `hourly_cost_hint`（加载期强制 >0）；两者皆无则拒绝（不能当 0，防 DO 池绕过预算）。
- **多仓争抢**：同一轮各仓先各自 Decide 出 `Create` 请求，再由 budget 统一分配，**按 QAge 从老到新**排序发放。
- **腾配额**：某仓场景 F 时，若其他仓有 online、busy=false、Q==0 的机器 → 对其 `Drain`（不受该仓 idle_timeout 限制），下一轮再分配。
- 编排器自身机器不计入。
- 持久化的当日花费（state.json 的 `day_utc/spent_today`）字段名不变，升级不清零。

### 3.8 执行（internal/engine）

动作类型：

```go
type ActionKind string
const (
    ActCreate          = "create"           // 建机（异步，不等 IP）
    ActDrain           = "drain"            // 先注销注册，成功后删 droplet
    ActForceDelete     = "force-delete"     // 先删 droplet 再删注册（D-kill、B 场景）
    ActRemoveReg       = "remove-registration" // 清 offline 残留注册
    ActSSHAct          = "ssh-act"          // 经 ssh 调机器上 `cirunner act <name>`
    ActIncident        = "incident"         // 交给 incident 模块（开/更新事故）
)
```

- **建机**（经该仓所属池的 `machine.Provider.Create`，§3.5 路由）：名字由池的 Namer 取（vpsctl 池：现存同名空间最大序号 +1，跳号）→ 铸 registration token → `cirunner/pkg/cloudinit.Render` → 传 `CreateRequest{Name, Tags, UserData}`（cloud-init 落盘 0600、成功即删、失败保留供诊断，由 Provider 负责）。vpsctl 实现内部把名字拆回 `-name-prefix`+`-start-index`，**不带 `-wait`**，受理即返回、IP 可为空。超时 90s。
- **Drain**：`DELETE runner 注册` → 成功 → 按机器的 Pool 路由到所属池 `Provider.Delete(id)`。注销失败（尤其 GitHub 拒绝删忙碌 runner【待实测 V1】）→ 本轮放弃删机，下一轮重判。若 V1 实测 GitHub 允许删除忙碌 runner：删前在同一轮内再拉一次 runner 列表复核 busy=false，并在 §11 记录竞态窗口仍存在。
- **SSHAct**：`ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i <key> root@<私网IP> /usr/local/bin/cirunner act <name> --json`，超时 60s；私网 IP 缺失时用公网 IP。（机器是一次性的、IP 复用，不维护 known_hosts；同 VPC 内中间人风险可接受。）
- **执行顺序**：同一轮先执行 `RemoveReg`/`Drain`/`ForceDelete`，再 `Create`，最后 `SSHAct`、`Incident`。
- **隔离**：每个动作独立 `context` 超时；失败写 WARN 日志并回填到状态（`CreateFailStreak`、事故证据），不中断其他动作。
- **长动作异步**：Create 和 SSHAct 在 goroutine 里执行，每仓一个 worker 串行化本仓动作，主循环不阻塞；全局 budget 加互斥锁。
- **dry-run / shadow 模式**：`daemon -dry-run` 只观察、决策、写日志，不执行任何动作（切换前与原型并跑对比用）。

### 3.9 主循环与调度

- 启动：加载配置 → 取 flock（`state_dir/ciorch.lock`，拿不到即退出，防双开）→ 恢复状态 → 对每仓立即 reconcile 一次。
- 每仓独立定时器：间隔按 §3.6.6 的 `poll_active`/`poll_idle` 动态切换；webhook 触发时立即 reconcile（2s 防抖），并把下次定时重置。
- 每轮：observe → Decide → budget 分配 → engine 执行 → incident 更新 → 原子写状态（tmp + rename）。
- 每轮结束输出一行摘要日志：`repo=… Q=… QAge=… I=… P=… N=… actions=[…] complete=…`。
- 心跳：每轮成功后更新内存里的 `lastReconcileOK`，供 `/healthz` 使用。

### 3.10 事故管线（internal/incident + internal/llm）

#### 3.10.1 事故状态机

```
(none) ──判据成立──► detected ──开 issue──► diagnosing ──LLM 结论──► acting ──动作执行──► verifying
                                                                                        │
     ┌──────── 恢复（判据不再成立且稳定 2 轮）◄──────────────────────────────────────────┤
     ▼                                                                                   │
 resolved（评论+关闭 issue）              未恢复且 LLM 轮次 < 3 ──► diagnosing（带新证据）┘
                                          未恢复且轮次用尽 / needs_human / 结论非法 ──► waiting-human
waiting-human ──issue 被人关闭──► resolved（并清熔断、解除 pause）
```

- 事故键 = `(repo, 场景类型, 主体)`；主体为机器名、label 组合或空。同键 24h 内重复出现 → 复用（reopen/追加评论）同一个 issue，不开新的。
- 固定逻辑的自动处置（§3.6.5）**先于**事故管线执行；事故管线是"固定逻辑修不好"或"需要告知"时的第二层。
- 场景 A/B/C 单次发生且被固定逻辑在 5 分钟内修好 → 不开 issue，只写入事故日志（`state_dir/incidents.jsonl`）并计数；满足 §3.6.5"何时升级"才开 issue。
- 处于 waiting-human 的仓：固定逻辑照常运行（扩缩容不停），但该事故不再调用 LLM、不再自动执行事故动作。

#### 3.10.2 issue 规范

- 开在 `issue_repo`（空则本仓）；编排器自身的问题（如 vpsctl 不可用、App 认证失败）开在 ciorch 仓。
- 标题：`[ciorch] <场景中文名>：<主体>（<repo>）`，如 `[ciorch] runner 假活：ci-nms-team2-ci-sgp1-03（hoshinojian/nms）`。
- 标签：`ciorch`、`ciorch:<场景代码 a~j>`；需要人时加 `needs-human`（标签不存在时先创建）。
- 正文模板：
  1. 发生了什么（一句话）+ 影响（排队 job 数、最老排队时长、受影响的 run 链接）
  2. 时间线（最近 20 条相关观察与动作，表格）
  3. 固定逻辑已做的处置及结果
  4. 下一步（"正在调用大模型诊断"/"等待人工"）
- 每次 LLM 诊断和动作结果各追加一条评论。
- 需要人时评论末尾 `@<maintainer_login>`，写清"人需要做什么"（来自 LLM 的 `human_steps`，或固定文案）。
- 恢复时评论"已恢复：<依据>"并关闭。
- 维护者明确：**不做脱敏**，日志原样贴（注意：公开仓的 issue 所有人可见，这是已知并接受的）。单条评论超过 60000 字符时截断，保留末尾。

#### 3.10.3 诊断材料（确定性采集，不经 LLM）

```json
{
  "incident": {"type": "A", "repo": "...", "subject": "...", "first_seen": "...", "round": 1},
  "timeline": [{"t": "...", "event": "observe|action|result", "detail": "..."}],
  "repo_state": {"Q": 3, "QAge": "4m10s", "I": 1, "P": 0, "N": 1, "breaker": {...}},
  "jobs": [{"id": 1, "run_id": 2, "name": "test", "status": "queued", "labels": [...], "created_at": "..."}],
  "runners": [{"name": "...", "status": "online", "busy": false, "labels": [...]}],
  "droplets": [{"name": "...", "created_at": "...", "region": "...", "size": "..."}],
  "machine_diag": { "...": "`cirunner diag --json` 原样输出，ssh 不可达时为 {\"error\": \"...\"}" },
  "job_log_tail": "卡死/失败 job 日志末尾 32KB（如适用）",
  "recent_vpsctl_errors": ["..."],
  "previous_rounds": [{"verdict": {...}, "action_result": "..."}]
}
```

总大小控制在约 100KB 内（超了按 job_log_tail → machine_diag 内的日志段 → timeline 的顺序截断）。

#### 3.10.4 DeepSeek 调用

- `POST {base_url}/chat/completions`，`Authorization: Bearer <key>`，`model=deepseek-chat`，`temperature=0`，`response_format={"type":"json_object"}`（JSON 模式要求提示词里出现 "json" 字样；具体约束以 DeepSeek 官方文档为准【待实测 V4】），`max_tokens=2000`，超时 90s，5xx 重试 1 次。
- 系统提示词（实现者按此意写成常量，放 `internal/llm/prompt.go`，中文）：
  - 你是 CI 基础设施诊断员。系统架构简介（§2.1 的三五句话）。
  - 已知坑清单（§5.3 + P139/P141 的一句话版）。
  - 你只能从下列动作中选一个：（列出本事故类型允许的动作及参数约束）。
  - 你不能执行任何命令，只输出 JSON，字段严格如下：（给 schema）。
  - 证据不足、或问题看起来是代码/配置/账户层面（如 DO 余额、配额、App 权限）→ `needs_human=true` 并写 `human_steps`。
- 用户消息：诊断材料 JSON。
- 限额：每事故最多 `max_calls_per_incident`(3) 次，全局每日 `max_calls_per_day`(20) 次；超限 → 直接 waiting-human 并在 issue 注明"已达大模型调用上限"。

#### 3.10.5 结论 schema 与白名单

```json
{
  "root_cause": "string，中文，≤500 字",
  "confidence": 0.0,
  "evidence": ["引用诊断材料中的具体字段/日志行"],
  "action": "none | restart_runner | replace_droplet | drain_droplet | switch_pool | pause_repo | clear_breaker | rerun_failed_jobs",
  "action_args": {"machine": "名称", "pool": "池名", "duration_min": 60, "run_id": 0},
  "needs_human": false,
  "human_steps": "string，需要人时写具体步骤",
  "issue_comment": "string，Markdown，写进 issue 的诊断说明"
}
```

校验（任一不过 → 视为 `needs_human=true`、`action=none`，issue 里注明"大模型结论不合法"并附原文）：

- JSON 可解析，必填字段齐全，`confidence ∈ [0,1]`。
- `action` 在**本事故类型允许的子集**内：

| 场景 | 允许的动作 |
|---|---|
| A 假活 | none, restart_runner, replace_droplet, drain_droplet |
| B 冷启失败 / 熔断 | none, switch_pool, clear_breaker, pause_repo |
| C 掉线 | none, replace_droplet |
| D job 卡死 | none, drain_droplet, rerun_failed_jobs |
| E label 不匹配 | none（只能建议人改配置） |
| F 配额不足 | none, drain_droplet（限其他仓空闲机） |
| H 观察失败 / I 建机报错 | none, pause_repo |
| J 孤儿机器 | none（必须人确认） |

- `action_args.machine` 必须是**本仓所辖**的现存机器；`pool` 必须在该仓 profile 的 `pools` 列表里（当前长度 1，即该动作实际不可迁移，待多池调度落地后生效）；`duration_min ∈ [10, 360]`；`run_id` 必须是本仓近 24h 的 run，且该 run 的失败 job 日志里含 runner 丢失类特征（`lost communication`、`shutdown signal`、`The operation was canceled` 之一），否则拒绝。
- `confidence < 0.5` 且 action≠none → 降级为 none + needs_human。
- 动作执行仍受 §3.7 配额/预算约束；`replace_droplet` 无配额时降级为 drain 后再建（droplet 字样为历史动作名，语义即"替换机器"）。
- `switch_pool` 的效果（原 `change_region` 的池化改法，事故管线阶段 5 按此实现）：该仓临时（6h）改用指定池建机，到期恢复；记入状态。
- `pause_repo`：该仓停止建机 `duration_min` 分钟（已有机器照常跑完、照常空闲回收）。

#### 3.10.6 人工介入

- 打 `needs-human` 标签 + @维护者 → 状态 waiting-human。
- 维护者处理后**关闭 issue** → webhook `issues.closed` → 该事故 resolved，清该仓熔断、解除 pause、`CreateFailStreak=0`。
- （后续可选，不在本期：issue 评论里的 `/ciorch <命令>`。）

### 3.11 HTTP 端点（internal/server）

- `POST /webhook`：§3.4.5。
- `GET /healthz`：最近一次全仓 reconcile 成功距今 < 3×`poll_idle` → 200 `ok`；否则 503 + 原因。不需要鉴权，不泄露细节。
- `GET /status`：JSON（各仓 Q/I/P/N、机器列表与状态、熔断、未结事故、今日花费）。需要 `Authorization: Bearer <status token>`（token 存 `/etc/ciorch/status.token`）。
- 只监听 `127.0.0.1:8080`，公网由 Caddy 反代（§3.14）。

### 3.12 状态持久化

- `state_dir/state.json`：全部 RepoState + budget 当日累计 + 事故表（不含 webhook 缓存）。每轮原子写。
- `state_dir/incidents.jsonl`：事故日志（追加）。
- `state_dir/cloudinit/`：建机失败遗留的 user-data（0700 目录，保留 7 天自动清）。
- `state_dir/ciorch.lock`：flock，daemon 持有；CLI 的只读命令不取锁，写命令（up/down/reset）取锁失败时提示"守护进程在运行，请改用……"并通过 `/status` 同机 API 转交（本期简化：写命令在守护运行时直接拒绝，提示先停守护或等后续版本）。

### 3.13 CLI 子命令（cmd/ciorch）

```
ciorch [-config path] daemon [-dry-run]    守护（systemd 托管）；-dry-run = shadow 模式
ciorch [-config path] status               打印各仓观察与机器状态（读 /status 或直接观察）
ciorch [-config path] plan                 观察 + 决策，打印本轮动作，不执行
ciorch [-config path] stats [-repo r] [-since 7d]   排队等待 p50/p95/max、冷启耗时、run 总时长，按 runner 分组
ciorch [-config path] up <repo>            手动建一台（守护运行时拒绝）
ciorch [-config path] down <repo> [-force] 手动回收该仓全部机器与注册（有排队拒绝，-force 覆盖）
ciorch [-config path] reset <repo>         清熔断/退避/pause
ciorch [-config path] check-config         只校验配置与凭据可用性（App token、vpsctl list、DeepSeek 连通）
```

`-config` 允许出现在子命令前后（同原型）。

### 3.14 部署（deploy/）

- 编排器机器：`vpsctl create -only team2-ci -region sgp1 -size s-1vcpu-512mb-10gb -image ubuntu-24-04-x64 -name-prefix ciorch -tags ciorch-host -authorized-keys <维护者公钥> -count 1 -wait 300s`（规格以 DO 当时最便宜的 1vCPU 档为准；内存紧张可升 1GB 档）。**必须与 runner 同账号同区**（内网直连）。
- 系统用户 `ciorch`（无登录 shell），`/etc/ciorch` 0700 属 ciorch。
- 安装：ciorch 二进制、vpsctl 二进制（按维护者指定 commit 编译）、Caddy（apt）。
- TLS：主机名用 `<公网IP以-连接>.sslip.io`（免买域名），Caddy 自动申请证书；Caddyfile：`<host> { reverse_proxy 127.0.0.1:8080 }`。【待实测 V5：Let's Encrypt 对 sslip.io 的签发是否顺利；不顺则改用维护者的域名】
- systemd：`deploy/ciorch.service`（User=ciorch，Restart=always，RestartSec=5，`WatchdogSec` 不用）。
- 防火墙（ufw）：入站只开 22、80、443。
- 安装脚本 `deploy/install.sh`：幂等；不包含任何凭据，凭据文件由维护者手动放置（脚本检查存在与权限，缺了就报错退出）。

### 3.15 编排器挂了怎么办（健康外检）

- ciorch 仓加 `.github/workflows/healthcheck.yml`：`schedule: cron '7 * * * *'`（每小时）+ `workflow_dispatch`，`runs-on: ubuntu-latest`（GitHub 托管，与编排器无关），步骤：`curl -fsS --max-time 20 https://<host>/healthz`；失败 → 用 `GITHUB_TOKEN`（`permissions: issues: write`）在 ciorch 仓开 issue（若已有带 `ciorch-down` 标签的 open issue 则追加评论），@维护者。
- 分钟数：私有仓每次按 1 分钟计，约 720 分钟/月（Free 计划私有仓额度 2000/月）；**若 ciorch 仓公开则不计**。仓的可见性由维护者决定。
- 注意 GitHub 对 60 天无活动仓库会停用 schedule workflow——runbook 里写明。
- ciorch 仓自身的代码 CI 也用 `ubuntu-latest`（不能用自己调度的 runner 测自己）。

---

## 4. 与 vpsctl 的契约（vpsctl 提供商实现的契约）

- 子进程调用，不 import（vpsctl 库代码都在 vpsctl 仓 internal/ 下）。二进制路径在池配置 `pools.<名>.vpsctl.bin` 显式锚定。契约内容不变：
- 用到的命令：
  - `create -only <acc> -region -size -image -name-prefix -count 1 -start-index N -tags a,b,c -user-data <file>`（不带 -wait）
  - `list -tag <tag> -only <acc>`（JSON：`droplets[]` 含 `id`(字符串)、`name`、`created_at`、`ipv4_public`、`ipv4_private`、`price_monthly`、`tags`；`errors[]` 非空即视为观察失败）
  - `delete -ids <acc>/<id> -confirm 1`（JSON 数组，逐项 `ok`）
- vpsctl 的守卫：带 `ssh_password` 的账号与 `--user-data` 互斥 → CI 用**无密码账号条目**（team2-ci）。
- vpsctl 会自动追加 `batch:<时间戳>` tag，不影响归属判定。
- （已落地：封装并入 `internal/provider/vpsctl/client.go`，`Create` 允许空 IP，机器视图折成 `machine.Machine`——2026-11-21 机器提供商抽象。）

---

## 5. cirunner（本仓）重写：机器侧

### 5.1 定位

cirunner 只管"runner 机器长什么样、机器上能被问什么、能做什么"。它**不做调度、不持有任何凭据、不调用 GitHub/DO/LLM API**。

两种产物：

1. **Go 库 `pkg/cloudinit`**（公开 API，ciorch 通过 go module 依赖、钉版本）：渲染 cloud-init。
2. **机器上的二进制 `cirunner`**（linux/amd64，CGO_ENABLED=0，静态）：`health` / `diag` / `act`。通过 GitHub Release 发布（本仓公开，下载无需鉴权），cloud-init 按版本号下载并校验 sha256。

### 5.2 仓布局（重写后）

```
pkg/cloudinit/        模板渲染 + 校验（从 internal/cloudinit 迁出并扩展）
cmd/cirunner/         机器上的 CLI
internal/health/      各项健康检查
internal/diag/        诊断材料采集
internal/act/         白名单动作
internal/sys/         对 systemctl/journalctl/df/docker 的子进程封装（可 fake）
docs/                 本文档、机器侧 runbook
.github/workflows/    本仓 CI（ubuntu-latest）+ release（打 tag 构建二进制、上传资产与 sha256）
```

原型的 `internal/decide`、`internal/engine`、`internal/githubapi`、`internal/vpsctlapi`、`internal/config`、守护相关 CLI：**迁到 ciorch 后从本仓删除**（等 ciorch 切换完成后的一个单独 PR 做，见 §9 阶段 6；在此之前本仓 main 上保留它们，不影响钉死的原型二进制）。

### 5.3 cloud-init 模板（pkg/cloudinit）

**必须保留**的踩坑修复（每条在模板里留一行注释说明来源）：

1. 禁 unattended-upgrades（会在开机约 20 分钟后批量升级、重启服务、杀正跑的 job）。
2. runner 一律装 latest release，并做版本号格式守卫（< 2.329.0 注册 404）。
3. `svc.sh install` 后 `systemctl daemon-reload` 再 start。
4. systemd unit 名里仓名用连字符（`owner-name`），点号会 Unit not found。
5. vpsctl `--authorized-keys` 不会注入自定义 user-data → `ssh_authorized_keys` 模板自带。
6. user-data 纯 ASCII（P75：DO 管道把 >0x7F 字节毁成 C1 控制字符），渲染后强制校验；文件内容用 `write_files` block scalar，不在 runcmd 里 printf（P70）。
7. DO Ubuntu 镜像 root 密码过期会挡公钥登录 → `chage -d "$(date +%F)" root`；`ssh_pwauth: false`。
8. Ubuntu 24.04 ssh.socket 激活陷阱 → 禁 ssh.socket、直接跑 ssh.service；sshd 同时听 22 和 443（救援通道）。
9. runner 用户进 docker 组、免密 sudo（gitleaks/sqlc job 需要 `sudo install`）。
10. 底座包：docker.io、jq、curl、build-essential、shellcheck（nms 需要；改为 profile 可配 `extra_packages`，默认即这五个）。

**新增**：

- 下载并安装机器侧二进制：`curl -fsSL -o /usr/local/bin/cirunner https://github.com/hoshinojian/cirunner/releases/download/<ver>/cirunner-linux-amd64`，`sha256sum -c` 校验，失败则 `exit 1`（让冷启失败暴露为场景 B，而不是带病上线）。
- 编排器 ssh 公钥 + 维护者公钥写入 root 的 authorized_keys（`ssh_authorized_keys`）。
- 每个阶段写进度标记到 `/var/lib/cirunner/stage`（`packages`→`agent`→`runner-download`→`runner-config`→`runner-started`），供 diag 判断卡在哪一步。
- **幂等 + 快照感知**：若 `/var/lib/cirunner/baked` 存在（快照镜像），跳过装包、装 docker、下载 runner 等步骤，只做注册与启动（§5.5）。
- `runners_per_droplet=2` 时渲染两个 runner 实例（目录 `/opt/actions-runner-a`、`-b`，名 `<name>-a`/`-b`）。

`Params`：

```go
type Params struct {
    RepoURL      string   // https://github.com/owner/name
    ServiceRepo  string   // owner-name
    RunnerName   string   // = droplet 名
    Labels       []string
    Token        string   // registration token
    PublicKeys   []string // 编排器 + 维护者
    AgentVersion string   // 机器侧 cirunner 版本 tag
    AgentSHA256  string
    ExtraPackages []string
    RunnersPerDroplet int // 1 或 2
}
```

校验（沿用原型并扩展）：各字段正则；公钥不得含引号/换行；产物纯 ASCII；**产物 ≤ 64KB**（DO user-data 上限）；golden 文件测试。

### 5.4 机器上的 CLI

所有子命令支持 `--json`（编排器只用 JSON），人读模式输出中文。需要 root（经 ssh 以 root 执行）。退出码：0 成功；1 检查不通过/动作失败；2 用法错误。

#### `cirunner health --json`

```json
{
  "ok": true,
  "checks": {
    "cloud_init": {"status": "done|running|error", "stage": "runner-started"},
    "runner_services": [{"unit": "actions.runner.owner-name.xxx.service", "active": "active", "since": "..."}],
    "listener_process": {"running": true, "pid": 1234},
    "worker_process": {"running": false},
    "docker": {"active": true},
    "disk_root_free_pct": 72.5,
    "mem_available_mb": 6100,
    "load1": 0.3,
    "runner_version": "2.3xx.x",
    "uptime_sec": 900
  }
}
```

`ok=false` 的条件：cloud-init error、runner 服务非 active、Listener 进程不在、docker 非 active、磁盘可用 <10%。

#### `cirunner diag --json [--lines 200]`

health 的全部内容，外加各段文本（每段截取末尾 `--lines` 行）：

- `/var/log/cloud-init-output.log`
- `journalctl -u actions.runner.* --no-pager -n N`
- runner `_diag/Runner_*.log` 最新一个、`_diag/Worker_*.log` 最新一个（**P139 Listener 挂起的关键证据在这里**）
- `journalctl -u docker -n N`、`docker ps -a`
- `df -h`、`free -m`、`dmesg | tail`（OOM 证据）
- `/etc/apt/apt.conf.d/20auto-upgrades` 内容、`/var/log/apt/history.log` 末尾（unattended-upgrades 坑的证据）

单段读取失败不影响其他段（该段写 `{"error": "..."}`）。总输出 ≤ 1MB。

#### `cirunner act <name> --json`

白名单，**只有**这些：

| name | 行为 |
|---|---|
| `restart-runner` | `systemctl restart actions.runner.*`（所有 runner 实例），等待 30s 内 Listener 进程出现，否则失败 |
| `stop-runner` | `systemctl stop actions.runner.*` |
| `clean-work` | runner 服务停止状态下才允许：清 `/opt/actions-runner*/_work/*` |

输出 `{"action": "...", "ok": true, "detail": "..."}`。未知 name → 退出码 2。

> 实现注意（V6 实测坑，2026-11-21）：`pgrep -f Runner.Listener` 会匹配到含该
> 模式串的**自身命令行**（如经 ssh 执行 `bash -c 'kill -STOP $(pgrep -f Runner.Listener)'`
> 时会把 STOP 打到会话 bash 上，致 ssh 断连）。机器侧代码用 `pgrep -x`（精确
> 进程名）或 `[R]unner.Listener` 字符集写法避开自匹配。

### 5.5 快照烘焙（阶段 7，依赖 vpsctl 扩展，见 §9）

- 配方 `pkg/cloudinit` 的 bake 模式：做 §5.3 的全部安装步骤，但**不注册 runner**；额外预取：`docker pull` profile 配置的镜像列表（nms：`timescale/timescaledb:2.17.2-pg16`）、预装 profile 配置的工具（nms：Go 工具链按 go.mod 版本、golangci-lint v2.13.2、gitleaks 8.30.1、sqlc 1.31.1）；完成后 `touch /var/lib/cirunner/baked`、`cloud-init clean --logs`，然后关机。
- ciorch 负责编排：`ciorch bake <profile>` → 建 bake 机 → 轮询（ssh 看 `/var/lib/cirunner/baked`，或机器关机）→ 关机 → 打快照 → 删 bake 机 → 记录快照 id 到状态，profile 自动切换到新快照；保留最近 2 个，删更老的。
- 快照建机失败（或 profile.snapshot 不可用）→ 自动回退到裸镜像 + 完整 cloud-init，并开事故。
- 重烤触发：每周一次定时；或维护者手动 `ciorch bake`。

---

## 6. nms 仓侧改动（遵守 NMS2/AGENTS.md：draft PR、五段描述、conventional commits 中文描述、docs-only 门禁）

1. **docs PR（尽快，阶段 0）**：
   - `docs/runbooks/gitrunner.md`：机器节改写——`gitrunner-sgp2` 已于 2026-11-19 删除；当前执行面为 cirunner 原型按需机器（过渡期，守护在维护者 WSL），将迁至 ciorch；"掉线=全仓 CI 停摆"的风险描述更新为"守护不在线=不会自动建机"。
   - `nmsdocs/00-导读与决策记录.md`：新增一条决策记录 sgp2 退役与按需执行面（取号规则按台账"取号说明"：编辑时查台账末条按序取号，**禁止预留编号**），引用 #145。
2. **ci.yml 加 `timeout-minutes`（阶段 0 或 1）**：每个 job 设上限（建议 test 30、其余 10；以实际耗时 ×2 为准，PR 描述里给出依据）。不改 `runs-on`（保持 `[self-hosted, ci]`）。
3. 切换完成后（阶段 6）：runbook 再更新为 ciorch 口径；pitfalls 按"现象→根因→解法"补记本次发现的原型缺陷 #2（job 永久卡住）。

---

## 7. 安全要点汇总

- runner 机器跑的是 PR 里的任意代码，且 runner 用户有免密 sudo → **机器上能读到的一切都视为公开**。所以 DeepSeek key、App 私钥、DO token 只在编排器上。
- fork PR 的 workflow 保持"维护者批准"门槛（nms 决策 #145 ③）——编排器不改变这一点；`action_required` 状态的 run 不算需求。
- 编排器机器：只开 22/80/443；webhook 必须验签；`/status` 需要 token；不注册为 runner。
- 编排器只删"自己的"机器：droplet 必须带本仓 tag 且名字在本仓命名空间内；`ciorch-host` 永不在删除候选里；孤儿机器（场景 J）只告警。
- vpsctl 账号文件在编排器上只含 CI 专用账号。

---

## 8. 测试要求

### 8.1 单元测试（必须，TDD：先写失败测试再实现）

**decide（表驱动，每个用例一行注释写场景）**——至少覆盖：

- 扩容：Q=7、N=0、max_runners=2 → 2 个 Create；Q=7、P=1 → 1 个；Q=1、I=1 → 0 个；全局配额 1 → 1 个（budget 层测）。
- 缩容：空闲 19m 不删、21m 删；近 2h 有 2 次活动 → 44m 不删、46m 删；Q>0 不删；`Complete=false` 不删；两台同时到期一轮只删一台。
- 硬寿命：8h 空闲删、忙不删；`Complete=false` 仍删。
- A：只差一轮不触发；3 轮触发 → Create + SSHAct；重启后 2m 内 busy → 解除；2m 后仍不接 → Drain；上线不足 60s 不判；QAge 不足不判；无配额时只 SSHAct。
- B：7m59s 不判、8m01s 判；替换 + 删旧 + Fails++；熔断 3 次 open；open 内不建；30m 后 half-open 只建 1 台；试探成功 closed；试探失败回 open 且冷却翻倍；冷却封顶 4h。
- C：offline 1 轮不判、2 轮判；从未 online 的走 B 不走 C。
- D：59m 不告、61m 告、91m ForceDelete。
- E：不可匹配 job 不计入 Q、不建机；5m 后出事故动作。
- G：外部 runner 永不出现在任何删除/ssh 动作里。
- J：孤儿 droplet 不删。
- 原型缺陷回归：run 状态 in_progress 但有 queued job、无 droplet → 必须 Create（原型 #2 的回归用例）。
- 命名：NextName 跳号、多仓不同前缀互不干扰、tag sanitize 与 hash。

**其他包**：

- githubapp：JWT 用测试密钥签名后用公钥验签；token 缓存过期刷新；401 作废重取；ETag 304 走缓存（httptest）；Retry-After；分页 Link。
- webhook：签名正确/错误/缺失；非白名单仓丢弃；防抖合并；body 超限。
- observe：label 匹配大小写、self-hosted 自动标签、P141 场景（run status=queued 但 jobs 全 completed → Q=0）。
- engine：执行顺序（删在建前）；Drain 注销失败则不删机；一个动作失败其他照做；create 不带 -wait、允许空 IP；建机失败保留 user-data 文件。
- budget：id 去重；按 QAge 分配；当日花费跨机器累计；跨 UTC 日清零；腾配额只动空闲机。
- incident：状态机全部转移；同键 24h 复用 issue；LLM 结论校验（非法 JSON、未知 action、越权 droplet、region 不在白名单、低置信度、run_id 无 runner 丢失特征）全部降级 needs_human；调用限额；issue 关闭 → resolved + 清熔断。
- llm：请求体字段（JSON 模式、temperature 0）；超时与重试；响应解析。
- cirunner `pkg/cloudinit`：golden 文件；10 条坑逐条有断言（如包含 `Unattended-Upgrade "0"`、`daemon-reload` 在 start 之前、unit 名连字符）；ASCII；≤64KB；baked 分支；两实例分支。
- cirunner 机器侧 CLI：`internal/sys` fake 掉子进程，覆盖 health 每项判定、diag 单段失败不影响其他段、act 白名单外拒绝。

### 8.2 真实环境冒烟（花钱，几美分到几毛钱；**用沙箱仓，不要用 nms**）

- 维护者建一个沙箱私有仓（如 `hoshinojian/ciorch-sandbox`），放一个 workflow：7 个并行 job（`sleep 30~120` + 一个 `sleep 600`），`runs-on: [self-hosted, ci-sandbox]`，加 `workflow_dispatch`。App 装到沙箱仓。
- 场景脚本（实现者写进 `docs/smoke.md`，每条记录命令与输出作为证据）：

| # | 操作 | 期望 |
|---|---|---|
| S1 | dispatch 一次 | 秒级开始建机；2 台；~3 分钟内开跑；跑完 20 分钟后逐台回收；注册清干净 |
| S2 | 跑的过程中 ssh 上去 `systemctl stop actions.runner.*` | 场景 C：替换机自动建起，剩余 job 被接走 |
| S3 | 跑的过程中 `kill -STOP` Runner.Listener 进程 | 观察 GitHub 侧表现（online 还是 offline）并记录；按表现应落入 A 或 C 并被修复 |
| S4 | 配置一个错误的 agent sha256 | 场景 B → 3 次后熔断 → 开 issue → LLM 诊断 → needs-human；关闭 issue 后恢复 |
| S5 | workflow 里一个 job `sleep 7200` | 场景 D：60m 开 issue（测试时可把阈值调到 3m/5m） |
| S6 | job 用 `runs-on: [self-hosted, nonexistent]` | 场景 E：不建机，开 issue |
| S7 | 重启编排器守护（跑的过程中） | 状态恢复，不误删、不重复建 |
| S8 | 停掉编排器 | healthcheck workflow 开 issue |

### 8.3 shadow 并跑（切换前）

ciorch `daemon -dry-run` 托管 nms，与原型并跑 ≥ 24 小时，对比两者每轮的决策日志；分歧逐条在 PR 里解释（预期分歧：ciorch 会多建一台、会更早发现停摆）。

---

## 9. 分阶段实施（每阶段 1~3 个 PR，单一目的，diff ≤ 400 行为宜，测试代码可另计）

| 阶段 | 仓 | 内容 | 验收 |
|---|---|---|---|
| **0a 原型热补丁（第一个 PR，紧急）** | cirunner | 从 `508a0b0` 开分支 `proto-hotfix`：① 排队信号改为"queued run 数 + in_progress run 里 queued job 数"（只需额外拉 in_progress run 的 jobs）；② 熔断冷却：`ColdStartFails ≥ Max` 且距最后一次失败 ≥30m → 允许一次试探建机（State 加 `LastColdStartFailAt`）；③ 单测覆盖这两点（含"in_progress run 有 queued job 且无 droplet → create"）。构建到 `~/.local/bin/cirunner-proto.new`，**由 ZCode 审查后**再替换 `cirunner-proto` 并重启服务 | 单测绿；替换后 `systemctl --user status cirunner` 正常 |
| **0b nms 文档** | nms | §6.1 | NMS2 AGENTS.md 的 docs-only 门禁 |
| **0c nms 超时** | nms | §6.2 | 云端 CI 七项绿 |
| **1 基线统计** | ciorch（新建） | 仓骨架、config、githubapp（先支持用 PAT 或 `gh auth token` 作为临时 token 源，App 认证在阶段 3 补）、`ciorch stats` | 对 nms 近 7 天出报告：排队等待 p50/p95/max、冷启耗时、run 总时长 |
| **2 决策核心** | ciorch | observe、decide（全部场景）、budget、engine、vpsctlapi（迁入改造）、state、`plan`/`status`/`daemon -dry-run` | §8.1 全部单测；本地 `plan` 对 nms 输出合理 |
| **3 机器侧重写** | cirunner | `pkg/cloudinit`（迁出+扩展）、机器侧 CLI、release workflow；**不删**原型代码 | 单测；手动建一台沙箱机验证 health/diag/act 输出 |
| **4 上云 + App + webhook** | ciorch | githubapp 的 App 认证、webhook、server、deploy/、healthcheck workflow；runbook（含"维护者创建 App 的操作清单"） | 编排器机器跑起来；`check-config` 全绿；沙箱 S1、S7、S8 |
| **5 事故管线** | ciorch | incident、llm、issue；全部白名单动作 | §8.1 incident/llm 单测；沙箱 S2~S6 |
| **6 切换** | ciorch / cirunner / nms | shadow 并跑 ≥24h → 原型无机器时停原型（`systemctl --user disable --now cirunner`）→ ciorch 正式托管 nms（`max_runners=2`）→ 观察 3 天 → cirunner 删原型调度代码 → nms runbook 与 pitfalls 更新（§6.3） | `ciorch stats` 与阶段 1 基线对比；3 天内无未处理事故 |
| **7 快照** | vpsctl（需维护者批准）/ cirunner / ciorch | vpsctl 加 `snapshot`（关机+打快照+等待完成）与 `images -private`、`image-delete`，并验证 `create -image <快照id>` 可用【待实测 V2】；cirunner bake 配方；`ciorch bake` | 冷启从 ~2.5m 降到 ≤1.5m（stats 对比）；快照失效时自动回退 |

**阶段之间的硬依赖**：0a 最先；2 依赖 1；4 依赖 2、3；5 依赖 4；6 依赖 5；7 依赖 6 且需要维护者批准改 vpsctl。

---

## 10. 实现者红线（违反任何一条，审查直接打回）

1. **不碰在役原型**：不修改、不重建、不停止 `~/.local/bin/cirunner-proto` 与 `~/.config/systemd/user/cirunner.service`，直到阶段 0a 审查通过（替换由审查后执行）或阶段 6 切换步骤。cirunner 仓 main 上随便 `make build`，不影响它。
2. **不在 nms 上做故障注入或冒烟**；真实环境测试只用沙箱仓。
3. **任何删除 droplet/runner 注册的手动操作**（测试清理除外，且只限沙箱仓、只限自己刚建的机器）先问维护者。
4. **不改 vpsctl**（阶段 7 获批前）。
5. **不改 nms 的 runner 规格/区域**（c2-4vcpu-8gb / sgp1，决策 #148 的覆盖率口径依赖它）。
6. **不把任何凭据写进仓库**，示例全用占位符；`bin/`、`*.local.json`、`.env` 不进库。
7. **不发明设计结论**：本文没写的，在 PR 描述"待确认"一节问；本文与现实冲突（如待实测项结论相反）→ 停下来，回写 §11 并请维护者/ZCode 定。
8. nms 仓内的修改遵守 `NMS2/AGENTS.md` 全部规则（draft PR、五段描述、文档同步、决策取号）。
9. ciorch、cirunner 两仓沿用同样的工程纪律：文档中文，代码/标识符/命令/JSON 字段英文；gofmt + go vet；conventional commits（`feat/fix/docs/refactor/test/build/ci/chore(scope): 中文描述`）；单一目的 PR，以 draft 打开，描述五段（变更/验证/风险/测试/关联）。

---

## 11. 待实测事项（实现前实测，结论回写此表）

| # | 问题 | 影响 | 实测方法 | 结论 |
|---|---|---|---|---|
| V1 | GitHub `DELETE /actions/runners/{id}` 对正在跑 job 的 runner 是否拒绝（422）？ | §3.8 Drain 的竞态防护是否成立 | 沙箱：跑 `sleep 300` 的 job 时调用删除 | **拒绝（422）**（2026-11-21 沙箱实测：busy=true 时 DELETE 返回 422 `Bad request - Runner mc-team2-ci-sgp1-01 is currently running a job and cannot be deleted.`；空闲时 DELETE 返回 204 成功）——Drain"注销失败本轮放弃删机"的防竞态成立，无需注销前同轮复查 busy |
| V2 | vpsctl `create -image <数字快照id>` 能否建机（vpsctl 把 image 作字符串原样传给 DO） | 阶段 7 | 阶段 7 前用一个临时快照试 | |
| V3 | ETag 304 是否计入 rate limit | 只影响轮询频率上限的估算 | 看响应头 `X-RateLimit-Remaining` 前后变化 | **304 不计入**（2026-11-21 实测：同 token 先 `GET /rate_limit` 记 `core.used`，对同 URL 带 `If-None-Match` 连发 20 次全 304，再查 `used` 仅涨 1~3、3 组取最小 Δ1——涨幅与后台其他调用相当，若 304 计费应涨 ≈21）。注意：小时日志的 remaining 首尾不可用于此推导——同账号多方消费（原型/人工 gh）与整点重置会混入 |
| V4 | DeepSeek JSON 模式的具体约束（提示词需含 "json"、是否支持 `deepseek-chat` 当前版本） | §3.10.4 | 阶段 5 前一次手工调用 | |
| V5 | Caddy + sslip.io 能否顺利签发证书 | §3.14 | 阶段 4 部署时 | |
| V6 | P139 式假活在 GitHub 侧的表现：online+idle 持续多久？`kill -STOP` Listener 能否复现 | 场景 A 阈值是否合理 | 沙箱 S3 | **STOP 复现的是场景 C 不是 A**（2026-11-21 沙箱实测：`kill -STOP` Listener 后 GitHub 侧数分钟内转 `status=offline busy=false` 并全程维持；`kill -CONT` 后 **≤16s** 恢复 online）——挂起 Listener 表现为掉线，P139 的 online 假活是另一种形态（Listener 在跑但轮询逻辑卡死）；场景 A 阈值维持 §3.6.6 现值 |
| V7 | `workflow_job` webhook 从 job 排队到送达的延迟 | 反应速度 | 对比 payload 的 `created_at` 与收件时间，取 20 个样本 | |

---

## 12. 交回审查时提交什么

每个 PR 的描述里给出：

1. **变更**：对应本文哪一节、哪个阶段。
2. **验证**：`go test ./...`、`go vet ./...`、`gofmt -l .` 的命令与输出；涉及真实环境的贴冒烟记录（§8.2 表格逐行：命令、时间戳、观察到的现象、截图或 API 输出片段）。
3. **风险**：会不会影响在役原型 / nms CI；最坏情况与回滚方法。
4. **测试**：新增了哪些用例，对应 §8.1 的哪些条目；哪些条目还没覆盖、为什么。
5. **关联**：本文中被实测推翻或补充的内容（同步改了 §11 或其他节）。

ZCode 的审查清单（实现者可先自查）：

- 每个 §3.6.5 场景都有正例和反例单测；原型缺陷 #2 的回归用例存在。
- decide 无 IO、无时钟读取；engine 的执行顺序与失败隔离有测试。
- 任何删除路径都经过归属判定（tag + 命名空间），且有"外部 runner/孤儿机器/ciorch-host 不被删"的测试。
- `Obs.Complete=false` 时不产生删除动作（硬寿命除外）。
- LLM 结论的每一种非法情况都降级为 needs_human，并有测试。
- 示例配置与测试数据里没有真实凭据。
- cloud-init 的 10 条坑逐条有断言。
