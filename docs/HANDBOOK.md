# CI 执行面全景交接手册（ciorch / cirunner / ciorch-sandbox）

> 起草日期：2026-11-22（UTC）。读者：维护者 hoshinojian，以及日后接手的大模型。
> 状态：nms 已于 2026-11-22 切换到 ciorch 托管，并经真实 PR 场景验证通过（§8.9、§12.2）；收尾见 §12.3。
> 事实来源：5 份调研素材（起源、总协调、实现、部署冒烟、架构）+ ciorch/docs 下四份文档 + 协调者亲自核实的切换事实。素材冲突时以代码与 gh 实查为准，并在文中注明。
> 红线：本文不含任何密钥、私钥、token、webhook secret、API key 内容，只写文件路径。

## 目录

- §0 文档说明
- §1 一页纸摘要（TL;DR）
- §2 动机与背景
- §3 选型与关键决策
- §4 系统全景
- §5 工作原理（以代码为准）
- §6 配置参考
- §7 演进历程（时间线）
- §8 验证体系与实测结论
- §9 踩坑大全
- §10 运维手册摘要
- §11 多 session 协作模式回顾
- §12 已知问题与后续工作
- §13 索引

## 0. 文档说明

### 0.1 目的

本文是 CI 执行面（ciorch 编排器 + cirunner 机器侧 + ciorch-sandbox 沙箱 + vpsctl 建删管）的**第一份要读的文档**。读完应能回答：为什么有这套东西、它是什么、怎么演进来的、现在怎么运维、坑在哪、下一步是什么，并知道去哪里查细节。

### 0.2 读者

- 维护者 hoshinojian：日后回看。
- 日后接手的大模型：把本文当入口，再按 §13 索引去读仓内权威文档（`docs/REDESIGN.md`、`docs/PROVIDER-ABSTRACTION.md`、`docs/runbook.md`、`docs/smoke.md`）。

### 0.3 阅读顺序

| 目的 | 先读 |
|---|---|
| 5 分钟了解现状 | §1 |
| 理解为什么这样设计 | §2、§3 |
| 理解系统怎么跑 | §4、§5 |
| 要改配置或加托管仓 | §6、§10 |
| 出事了排查 | §10、§9、§12 |
| 要继续开发或收尾 | §12、§11、§13 |

### 0.4 给接手大模型的速读指引

**先读**：§1（现状）→ §4（三个仓与沙箱的关系）→ §10（运维）→ §12（未修问题与待办）。其余章节按需查阅。

**绝不能碰的红线**（出处：REDESIGN §10、runbook §五、各 session 派工红线）：

1. 不改 nms 的 runner 规格与区域：`c2-4vcpu-8gb` / `sgp1`（专用核；nms 决策 #148 的覆盖率基线依赖同口径环境）。
2. 编排器机器 `ciorch-team2-ci-sgp1-01` 绝不注册为 runner，也绝不打 `ciorch` 公共标签（只用 `ciorch-host`），否则会被当 runner 机器计入配额甚至进入删除候选。
3. 用 vpsctl 建机必须带 `-only <account>`（2026-11-13 漏过一次，全账号各建了一台）。
4. 不碰原型回滚路径：`~/.local/bin/cirunner-proto`、`~/.config/systemd/user/cirunner.service`、`~/cirunner` 仓，在收尾 PR 之前保留不删（§12 TODO）。
5. 不在 nms 上做故障注入或冒烟，真实环境测试只用沙箱仓 `hoshinojian/ciorch-sandbox`。
6. 不改 vpsctl 仓（快照扩展需维护者批准）；改 nms 仓须遵守 `NMS2/AGENTS.md`（draft PR、五段描述、conventional commits、文档同步清单）；nms 侧只用 `nms`，不用 `nmsctl`。
7. 不把任何凭据写进仓库、文档或聊天；示例一律占位符。
8. 删 droplet 或 runner 注册的手动操作，先问维护者（沙箱里自己刚建的测试机除外）。
9. 排查时先对照代码与 `gh` 实查，不凭印象，也不要假设切换或观察期结论（见 §12 TODO）。

### 0.5 口径约定

- 时间一律 UTC（编排器 journal 与 GitHub API 同为 UTC；WSL 时钟与 UTC 基本一致）。
- 标【代码与文档不一致】的条目以代码为准；标【推断】的是读代码推出、未实测的行为；标【不确定】的是素材未能核实、此处保留原样。
- 出处写法：`PR #N`（ciorch 仓，其他仓会注明仓名）、`文件:行号`、session id 前缀（见 §13.3）、nms 决策 `#NNN` 与 pitfalls `PNN`。
- 素材之间有冲突时，以代码、gh 实查与协调者亲自核实的事实为准，冲突点汇总在 §12.4。
- 本文所述当前线上配置只写结构，不含任何密钥内容。

## 1. 一页纸摘要（TL;DR）

### 1.1 现状（2026-11-22 起草时）

- **nms 的 CI 已由 ciorch 托管**：2026-11-22 07:23–07:24 UTC 完成切换（备份配置、加 pool/profile/repo、停原型、重启 ciorch），首个 nms CI（run 44998142282）7 个 job 全 success，覆盖率棘轮 89.3% ≥ 基线 89.1%（出处：协调者亲自核实）。
- 切换后发现并修复了 5 个隐患：ciorch PR #50（webhook job 缓存跨仓污染 + 并发写 map）、#51（ETag 缓存无锁）、#52（dry-run 仍会开 issue、调 LLM）、#53（diag 退出码 1 时丢证据）、#54（stats 在编排器上无法用 App 认证）。均已合并并部署到编排器。
- **验证通过**：维护者 11-22 决定不等自然 PR，改由独立 session 造真实测试 PR（nms #627–#631）覆盖 docs-only、全量 7 job、并发排队、cancel-in-progress、空闲回收，全部符合预期，两仓零事故 issue（§8.9）。观察结论见 §12.2。
- 原型调度器（WSL 上的 `cirunner-proto`）已 `disable --now`，二进制与单元文件**只停不删**，作为回滚路径保留；cirunner 仓内的原型调度**源码**已由 cirunner PR #7 删除（最后包含原型的提交 `b0225ce`）。

### 1.2 三个仓、一个沙箱、一个建机工具：一句话定位

| 名称 | 一句话定位 |
|---|---|
| **ciorch**（`~/ciorch`，公开，MIT） | 控制面。常驻在 $4/月 的 DO 小机上，不跑任何 CI；接 webhook + 轮询，观察队列，固定逻辑决定建删机，出事故开 issue 并让 DeepSeek 诊断（只出主意、白名单动作） |
| **cirunner**（`~/cirunner`，公开，MIT） | 机器侧。提供 cloud-init 渲染库 `pkg/cloudinit` 与装在 runner 机器上的 `cirunner health/diag/act`（GitHub Release v0.1.0–v0.1.3）；其中原型调度器代码已停用，仅作回滚保留 |
| **ciorch-sandbox**（私有） | 故障注入与冒烟专用仓，只有手动触发的 workflow；不触发则零成本；事故 issue 开在这里而不污染真实项目 |
| **vpsctl**（`~/vpsctl`，公开，MIT） | DO 建删管的唯一入口；ciorch 只以子进程调用，不 import、不改它 |
| **nms**（私有） | 第一个托管仓（租户）；7 个并行 job，`runs-on: [self-hosted, ci]` |

### 1.3 关键数字

| 指标 | 数值 | 出处 |
|---|---|---|
| 冷启（建机→首个 job 开跑） | 约 2–2.5 分钟（切换首跑 2m09s；REDESIGN 实测约 2.3 分钟；旧文档写 4–6 分钟，偏保守） | REDESIGN §1.3；切换首跑 07:25:22 建机 → 07:27:31 开跑 |
| 排队到建机决策 | run 创建→建机 14 秒（07:25:08→07:25:22）；其中 webhook 送达→建机 8 秒（07:25:14→07:25:22） | 切换首跑实测 |
| run 创建→首 job 开跑 | 2m23s（07:25:08→07:27:31） | 切换首跑实测 |
| nms 整个 run 时长 | 14m26s（首跑）、17m17s（#50 部署后第二次）；原型时代约 23 分钟 | 协调者实测；01-origins §1.3 |
| test job | 约 11–13 分钟，是关键路径 | 同上 |
| 机器规格 | c2-4vcpu-8gb / sgp1 / ubuntu-24-04-x64，$94/月 ≈ $0.1399/h（94/672） | 池配置 `hourly_cost_hint` |
| 并发上限 | nms `max_runners=2`；全局 `max_machines=3`；每日预算 $8 | 线上配置（07:23 修改） |
| 编排器 | DO `s-1vcpu-512mb-10gb`，$4/月，sgp1，tag `ciorch-host` | 04-deploy-smoke §1 |
| 首个 nms CI 花费 | 约 $0.15 | 协调者实测 |
| 沙箱冒烟 + 回归花费 | 约 $0.22（远低于每日上限） | 04-deploy-smoke §8 |
| 空闲回收 | 默认 20 分钟（近 2h 有 ≥2 个不同 run 则 45 分钟）；一轮回收一台 | 代码 `decide/scenarios.go` |
| 代码规模 | ciorch PR #1–#54；cirunner PR #1–#6，tag v0.1.0–v0.1.3 | gh 实查（见 §13.2） |

### 1.4 月成本构成（口径说明）

- 常驻部分：编排器约 **$4/月**。原来的常驻 runner `gitrunner-sgp2` 是 **$94/月**，已于 2026-11-19 删除。
- 按需部分：runner 机器只在有活时存在，约 $0.14/h/台；一个完整 nms run 约 $0.15（2 台、约半小时含空闲回收窗口）。
- DeepSeek：只在事故里调用，每事故最多 3 次、每天最多 20 次，金额很小，**素材中没有实际账单**。
- 2026-11-15 的预估是「按需 220h×$0.139 ≈ $31/月」或「$28/月」，**属于预估，素材中未见实际账单核对**【不确定】。真实月成本要等一个完整月的 DO 账单。

### 1.5 为什么这样设计（三句话）

1. 私有仓托管分钟数耗尽、常驻机一天只用几小时却按 24h 计费、单机串行很慢，于是做成「按需建删机 + 自建 runner」。
2. 调度全部用固定逻辑（纯函数、可单测、可 golden 回放），大模型只在事故里出主意，不拿凭据也不拿 shell。
3. 控制面（有凭据、常驻、便宜）与执行面（一次性、不持长期凭据、跑任意 PR 代码）严格分开。

## 2. 动机与背景

### 2.1 痛点演进链

| 日期 | 事件 | 出处 |
|---|---|---|
| ≤2026-11-12 | 使用 GitHub 托管 runner；私有仓 Actions 分钟数耗尽（Free 额度 2000 Linux 分钟/月），全仓 CI 停摆 | nms 决策 #145；REDESIGN §1.1 |
| 11-12 | docs-only PR 被全量 `-race`（10–13 分钟）反复拦 → 决策 #143 快速放行；本地全量与云端重复 → 决策 #144 本地降为快速门禁 | nms 台账 |
| 11-13 | 决策 #145：七项 checks 的 `runs-on` 改为 `[self-hosted, ci]`，迁自建常驻 runner（PR #590）。共享核（s 系）两次击穿 `-race` 时敏用例，换 DO 专用核 `c2-4vcpu-8gb`，即 `gitrunner-sgp2`，$94/月 | nms runbook gitrunner.md |
| 11-13 | 已知坑三连：unattended-upgrades 两次打断正跑 job；runner 2.328.0 注册 404（须 ≥2.329.0）；`svc.sh install` 后需 daemon-reload | 同上 |
| 11-14 | P139 静默停摆：online、busy=false、journal 零日志、不接活，重启即愈；决策 #148：换机致覆盖率读数下移约 0.1pp，基线一次性 89.2→89.1 | nms pitfalls；台账 |
| 11-15 | 维护者嫌常驻机费钱，提出「一键拉起、用完删机」，当天诞生 cirunner 原型（WSL 守护 + vpsctl 建删 + cloud-init 注册） | S-14a3 |
| 11-16 | 维护者嫌「所有动作都串行、慢」：诊断为两层串行（job 级：只有 1 台 runner；包级：`-race -p 1`）。加第二台 runner，并行度 `-p 1`→`-p 4`（nms 决策 #152）：test 19m14s→11m35s | S-9997 |
| 11-19 | 维护者提出「便宜的常驻编排器 + 大模型诊断 + 服务所有项目」；删除 sgp2，原型独立承载 | S-a68a |
| 11-20 | REDESIGN.md 定稿 v1，交另一个模型写码 | REDESIGN 头部 |

### 2.2 维护者原话要点（带出处）

- 常驻费钱：「我感觉 ci 服务器一直开着也很费钱」「能不能整一个脚本，以后可以一键拉起自己的 ci 服务器，用完了就把这个服务器删掉」（S-14a3，11-15 04:48）。DO 关机不停止计费，只有删机才停，所以必须是「删」。
- 项目无关：「即便 nms 以后不用跑 ci 了，我的其他项目也要跑 ci」（S-14a3，11-15 04:51）→ 编排器做成通用池，nms 只是第一个租户。
- 串行慢：「所有动作都是要串行完成的，会导致速度很慢。有没有更好的办法」（S-9997，11-16）。
- 高速、强大：「尽可能高速和尽可能的强大，比如说发现一直没有跑任务同时有很多任务排队时候怎么做」（S-a68a，11-19 16:13）。
- 便宜编排器：「平时只有这个编排器一直活着，一有活的时候……编排器再去 vpsctl 申请机器，然后监控着跑 ci」；「上面接大模型来决定送哪里去或者上面跑死逻辑」（S-a68a，16:34）。
- 服务所有项目、出事开 issue：「我其实希望这个编排器能够成为我的所有项目的 cirunner 的编排器」；出问题「就直接在对应的那个库里面交 issue，同时调用大模型来给自己看着排查一下……需要人力介入就在那个 issue 里面增加内容等人介入」（16:43）。
- 新仓、DeepSeek、不脱敏：「新开一个仓库作为编排器，当前的 cirunner 就作为简单的 runner」；「我的代码没有什么脱敏的需求」；「大模型的话我目前打算用 deepseek」（16:52）。
- 分工：「你只用负责列 plan，干活的话我会找其他人的」（11-21 10:36）。

### 2.3 目标（REDESIGN §1.2，按优先级）

1. **快**：PR 推上去到出结果的总时间尽量短；排队到开跑 ≤ 冷启时间（目标 ≤3 分钟，快照后 ≤1.5 分钟）；7 个 job 不再被单机串行拖长。
2. **强（自愈）**：任何「有活排队但没人干」的状态在分钟级被发现并修复；修不好就开 issue 叫人，绝不沉默。
3. **多项目**：一套编排器服务所有仓，加项目 = 一段配置 + 装 App。
4. **省钱**：没活时只有编排器小机在花钱；runner 机器空闲即删；有硬预算上限。
5. **可审计**：每个决策有日志，每次事故有 issue 留痕。

非目标：不做 Web UI；不调度 GitHub 托管 runner；不支持 Windows/macOS runner；不跨云（只 DO，经 vpsctl）。

### 2.4 原型（cirunner@508a0b0）为什么不够

原型是 WSL 上的 systemd 用户服务，每 60 秒按「run 粒度」判断队列。REDESIGN §1.4 列了 13 个缺陷，最致命的两个：

- **缺陷 #2（run 粒度）**：只数 `status=queued` 的 run。run 里一个 job 开跑即变 in_progress，其余排队 job 不可见。后果：runner 中途挂或假活 → 20 分钟后被当空闲删 → 原型永不再建机 → **job 永久卡住**。sgp2 在时被它兜底，11-19 删掉 sgp2 后暴露。
- **缺陷 #1（死锁）**：冷启失败满 2 次永久停建，计数只在出现活跃 runner 时清零，而停建后不会再出现。

其余包括：完全不看 label（nms `testenv-bootstrap.yml` 的 `ubuntu-latest` 会被当需求）、建机同步阻塞、状态按仓而非按机器、空闲删机竞态、告警只写日志、无锁读写 states.json、无机器最长寿命、用维护者个人 gh 登录态等（完整表见 REDESIGN §1.4）。原型热补丁（cirunner PR #1，commit 53be365）只补了 #1 与 #2。

### 2.5 WSL 依赖（结构性短板）

- 原型守护进程跑在维护者 WSL2 的 systemd 用户服务（enable + linger）。**WSL 必须在线**，否则不会自动建机；协作者推 PR 时 checks 停在 queued 不丢，等守护回来再建机。11-19 02:30–03:09 实测停过一段。
- WSL 出站到 DO 新机 22 端口按目标 IP 选择性丢包（V8：4 个新 sgp1 IP 中 1 个全 TCP 被丢，ICMP 通），因此也不适合当常驻控制面。
- nms 决策 #151 把风险口径从「机器掉线=全仓 CI 停摆」改为「守护不在线=不会自动建机」。
- 迁移到 DO 小机上的 ciorch 后，不再依赖 WSL 开着（协调者的切换方案，S-a68a 11-22 02:03）。

## 3. 选型与关键决策

每条给出：决策 / 备选 / 理由 / 出处。编号 D1–D18 仅为本文内部引用用，不是 nms 台账编号。

### 3.1 执行面与机器

| # | 决策 | 备选（被否） | 理由 | 出处 |
|---|---|---|---|---|
| D1 | **按需建删 DO droplet + 自建 runner** | GitHub 托管 runner；常驻自建 runner（sgp2） | 私有仓 Free 额度 2000 分钟/月且已耗尽，托管 runner 还丢热缓存；常驻机 $94/月却每天只用几小时；DO 关机不停止计费，只有删机省钱；自建 runner 不吃分钟数 | S-9997 11-16 09:56；S-14a3；nms #145 |
| D2 | **runner 规格钉死 c2-4vcpu-8gb（专用核）** | 共享核 s 系（约便宜 8 倍） | 共享核两次实录击穿 `-race` 时敏用例（TestReparentForDeployWaitCtxCanceled、TestSNMPv3SessionInteroperatesWithEntryAgent）；换测量环境会让覆盖率基线重校（nms #148 先例：89.2→89.1）。无 `-race` 时敏用例的其他项目可用 s 系 | nms runbook gitrunner.md；#148；REDESIGN §3.3、§10.5 |
| D3 | **区域钉死 sgp1** | 其他区 | 同 D2：同规格、同区、同镜像规避基线漂移；编排器与 runner 同账号同区，可走 VPC 内网 ssh | REDESIGN §3.14；S-14a3 |
| D4 | **runner 用会话式常驻（非 ephemeral）**：disposable 的是机器，不是注册 | `--ephemeral` 一单一注 | nms 一个 run 有 7 个并行 job，一单一注则每个 job 多付一次冷启 | S-14a3 04:57；cirunner README |
| D5 | **回收由控制面统一执行，机器上不放 DO token** | 机器自毁 | 专机专用、不存凭据；runner 上能读到的一切都视为公开（PR 里的任意代码 + 免密 sudo） | S-14a3；REDESIGN §7 |
| D6 | **一台 runner 机器只服务一个仓；编排器机器绝不注册为 runner** | 多仓共享机器 | 仓 A 的 CI 代码看不到仓 B 的东西；编排器持有 DO token、App 私钥、DeepSeek key | REDESIGN §2.2 原则 11、12 |
| D7 | **编排器用 $4/月 `s-1vcpu-512mb-10gb`，不升 1GB 档** | 1GB 档 | 实测内存 458MB、可用约 302MB，ciorch 常驻约 5.9MB，无压力 | 04-deploy-smoke §1.1 |

### 3.2 与 vpsctl、GitHub 的接口

| # | 决策 | 备选（被否） | 理由 | 出处 |
|---|---|---|---|---|
| D8 | **子进程调用 vpsctl 二进制**，只用 create/list/delete 三个动词，CLI 有 JSON 输出 | fork vpsctl；import 其代码 | fork 会漂移（vpsctl 活跃演进）；vpsctl 逻辑全在 `internal/`，Go 禁止跨仓 import；继承变依赖。ZCode 提出修正，维护者接受。记录验证过的 vpsctl commit 与二进制校验和（vpsctl 无版本机制） | S-14a3 04:50；REDESIGN §4 |
| D9 | **CI 用无密码账号条目 `team2-ci`**（复用 team2 的 DO token） | 沿用带 ssh_password 的 team2 | vpsctl 对带密码账号与 `--user-data` 硬性互斥（pitfalls P70）；CI 机只走公钥。代价：team2 与 team2-ci 同 token，不加 `-only` 同一台机会被列两遍，且归属只能靠 tag 圈地 | PF P70；01-origins §4 |
| D10 | **GitHub App 认证**（RS256 JWT → installation token，标准库实现） | 维护者个人 PAT / `gh auth token` | 常驻服务器不能依赖个人登录态；多项目安装按仓显式白名单；额度独立（个人 token 与原型、shadow、手动 gh 共用 5000 次/小时，污染度量） | REDESIGN §3.4；PR #27；S-a68a V3 纠正 |
| D11 | **webhook + 轮询双通道，轮询是真相来源** | 纯 webhook；纯轮询 | webhook 秒级触发但可能丢（网络抖动、编排器重启）；轮询兜底并作判定依据，webhook 只决定「何时立即看一眼」与补齐刚出现的 job | REDESIGN §3.4.5 |
| D12 | **判据只用 GitHub 侧信号**：就绪 = runner online，空闲 = busy=false，需求 = jobs 接口的 queued job；机器侧 diag 只用于诊断 | 用机器日志做就绪或回收判据 | P139 教训：假活时 journal 零日志，且 test job 跑十几分钟本来就安静；`vpsctl --wait` 只等到 active+公网 IP，不等 cloud-init | PF P139；REDESIGN §2.2 原则 1 |
| D13 | **TLS 用 Caddy + `<IP横杠>.sslip.io`** | 自有域名 | 免买域名；V5 实测 Let's Encrypt HTTP-01 一次签发成功；ciorch 只绑 127.0.0.1:8080 | REDESIGN §11 V5 |

### 3.3 调度与诊断

| # | 决策 | 备选（被否） | 理由 | 出处 |
|---|---|---|---|---|
| D14 | **固定逻辑优先；大模型只在事故里出主意，且只能选白名单动作** | 让大模型日常调度 | 日常调度 10–15 秒一判、规则简单、要能单测、判错会杀 job；大模型拿不到凭据与 shell，主要防提示词注入（job 日志是 PR 代码打印的）；非法结论一律降级 needs_human；成本硬上限由确定性代码执行 | S-a68a 16:34、16:43；REDESIGN §2.2 原则 9、10 |
| D15 | **DeepSeek（OpenAI 兼容、JSON 模式）**；每事故最多 3 次、每天 20 次（此上限代码未接线，见 §12） | 其他模型 | 维护者指定。V4 实测 JSON 模式可用，`deepseek-chat` 现指向 `deepseek-flash` | S-a68a 16:52；REDESIGN §11 V4 |
| D16 | **诊断材料与 issue 不脱敏** | 脱敏后再发 | 维护者原话「我的代码没有什么脱敏的需求」；registration token 1 小时失效可接受。原计划「公开仓 issue 只写摘要」随之放弃，沙箱与 nms 均为私有仓 | S-a68a 16:52；REDESIGN §3.10.2 |
| D17 | **机器来源经 Provider 接口，纯重构、行为零变化**；profile.pools 强制长度 1（不实现多池调度） | 直接写死 vpsctl | 维护者可能以后有自己的高性能机器（Incus/Proxmox）或「本地优先、DO 溢出」；趁建机路径还浅，成本最低。用 golden 回放证明零变化，CI 强制 import 边界 | PROVIDER-ABSTRACTION；PR #16–#23 |
| D18 | **新开 ciorch 仓做控制面，cirunner 降为机器侧**；故障注入只用私有沙箱仓 | 在原仓重写；在 nms 上冒烟 | 原仓重写核心是对的，但控制面与机器侧职责不同；沙箱避免在 nms 上做破坏性测试，事故 issue 也不污染真实项目 | S-a68a 16:52；REDESIGN §8.2 |

### 3.4 许可证与署名

- 三个公开仓（ciorch、cirunner、vpsctl）统一 MIT，版权行 `Copyright (c) 2026 hoshinojian`（ciorch PR #15、cirunner PR #2、vpsctl PR #32）。
- 用户原话是「署名就用 mit」，措辞含糊；协调者理解为用 MIT 并署 GitHub 用户名，并说明想换真名可改，用户未再异议。是否曾明确确认「真名还是用户名」**【不确定】**。
- vpsctl PR #32 只有 S1 自述，素材中未用 gh 单独核对。

### 3.5 沿用的 nms 侧决策（不是本套新作，但约束本套设计）

| nms 编号 | 内容 | 对本套的约束 |
|---|---|---|
| #143 | docs-only 变更在 test job 内快速放行 | 依赖 `core.quotepath=false`（P144） |
| #144 | 本地快速门禁，云端 CI 全量为唯一权威 | CI 稳定性直接决定合并速度 |
| #145 | 执行面迁自建 runner；fork PR 保持维护者批准 | `action_required` 的 run 不算需求 |
| #148 | 覆盖率基线 89.1；换测量环境须同口径三连最小值复测 | D2、D3 的根据 |
| #151 | sgp2 退役，按需执行面，规格钉死 | 风险口径：守护不在线 = 不会自动建机 |
| #152 | `-race -p 1` → `-p 4 -timeout 20m` | test 约 11–13 分钟，成为关键路径 |

注意：台账 #150 撞号（NetFlow/IPFIX 国家归属与测试并行化先后取号），测试并行化顺延为 #152（nms PR #625）。切换完成后台账新增条目须按「取号说明」查末条按序取号，禁止预留编号。

## 4. 系统全景

### 4.1 全景图

```
                  ┌──────────────────────────── GitHub ────────────────────────────┐
                  │ 托管仓：hoshinojian/nms（私有）、hoshinojian/ciorch-sandbox（私有）│
                  │   （均已装 ciorch GitHub App，App ID 5863295，bot=ciorch[bot]）   │
                  │ job 排队 ─workflow_job webhook─┐   job 派发给已注册 runner         │
                  │ issue 被关 ─issues webhook─────┤   ▲（runner 主动长轮询 GitHub）   │
                  │ REST：runs/jobs/runners/注册token/issues/日志/rerun ◄─┐          │
                  │ hoshinojian/ciorch：healthcheck.yml（托管 runner，每小时）│          │
                  └──────────────────────────────┼───────────────────────┼─────────┘
                          HTTPS（Caddy，sslip.io） │       App JWT → installation token
                                                  ▼                       │
 ┌── 编排器 ciorch-team2-ci-sgp1-01（DO s-1vcpu-512mb-10gb，tag ciorch-host）──────────┐
 │ Caddy :443 → 127.0.0.1:8080（/webhook /healthz /status）                          │
 │ ciorch daemon：每仓一个 goroutine，15s/60s 轮询 + webhook 即时触发               │
 │   observe → decide（纯函数）→ budget Gate → engine 执行                          │
 │     ├─ Provider "vpsctl" ─ 子进程 vpsctl create/list/delete ──► DigitalOcean API │
 │     ├─ ssh（系统 ssh，root@私网IP）──► runner 机器上 cirunner act/diag            │
 │     └─ incident.Manager → GitHub issue → DeepSeek（只出主意）→ 白名单动作         │
 │ 凭据在 /etc/ciorch/（0700），状态在 /var/lib/ciorch/                               │
 └───────────────────────────────────────────────────────────────────────────────────┘
                                    │ 建机（user-data = cirunner pkg/cloudinit 渲染）
                                    ▼
 ┌── runner 机器（按需建、空闲删；一台只服务一个仓；c2-4vcpu-8gb / sgp1）───────────┐
 │ 名字 ci-<仓短名>-team2-ci-sgp1-NN（= runner 名）                                  │
 │ tag：ciorch + ciorch-r-<owner>-<name>-<hash8>                                     │
 │ actions runner（会话式常驻）+ /usr/local/bin/cirunner health|diag|act             │
 │ 只有 1h 时效的 registration token，不持长期凭据                                   │
 └───────────────────────────────────────────────────────────────────────────────────┘

维护者 WSL：~/vpsctl（建编排器用）、~/.local/bin/cirunner-proto（停用的原型）、
  ~/.config/systemd/user/{cirunner,ciorch-shadow}.service（均 disabled）、
  ~/.local/state/ciorch-observe/observe.sh（观察期脚本，见 §12.2）
```

### 4.2 仓库与沙箱一览

| 名称 | 路径 / 地址 | 可见性 | 角色 |
|---|---|---|---|
| ciorch | `~/ciorch`，`hoshinojian/ciorch` | 公开（MIT） | 控制面：编排器代码、REDESIGN/runbook/smoke 文档、healthcheck workflow、deploy/ |
| cirunner | `~/cirunner`，`hoshinojian/cirunner` | 公开（MIT） | 机器侧 agent 与 cloud-init 库；同仓保留原型调度器（已停用） |
| ciorch-sandbox | `hoshinojian/ciorch-sandbox`（2026-11-20 07:35Z 创建） | 私有 | 故障注入与冒烟，只含手动触发 workflow |
| vpsctl | `~/vpsctl` | 公开（MIT） | DO 建删管唯一入口，不改它 |
| nms | `~/NMS2`，`hoshinojian/nms` | 私有 | 第一个托管租户 |

可见性已用 `gh repo view --json visibility` 核实（2026-11-22）：ciorch、cirunner、vpsctl 为 PUBLIC（均 MIT），ciorch-sandbox 与 nms 为 PRIVATE。

### 4.3 重点：ciorch、cirunner、沙箱三者的关系

**① cirunner 的角色演变**

```
2026-11-15  cirunner 原型 = 调度器（WSL 守护：观察队列 + vpsctl 建删 + cloud-init）
               │  暴露 13 个缺陷（REDESIGN §1.4）；11-20 热补丁 PR #1 只补 #1、#2
               ▼
2026-11-20  REDESIGN：新开 ciorch 做控制面；cirunner 降为"机器侧"
               │  调度逻辑（decide/engine/githubapi/vpsctlapi）迁到 ciorch 重写
               ▼
2026-11-21  cirunner 阶段 3：pkg/cloudinit（复制迁出并扩展）+ health/diag/act
               + release workflow，v0.1.0 → v0.1.3
               ▼
2026-11-22  nms 切换：原型 disable --now（只停不删），cirunner 仓只剩"机器侧"在役
```

cirunner 现在同时存在两部分（代码层面都还在 main 上）：

| 部分 | 状态 | 谁用它 |
|---|---|---|
| `pkg/cloudinit`（库）+ `cmd/cirunner` 的 health/diag/act（机器侧二进制）| **在役** | ciorch 通过 go module 依赖（钉 v0.1.3）；cloud-init 在机器上下载二进制并校验 sha256 |
| `cmd/cirunner` 的 status/plan/up/down/daemon + `internal/{decide,engine,githubapi,vpsctlapi,config,cloudinit}`（原型调度器）| **源码已删除**（cirunner PR #7；最后版本 `b0225ce`）；WSL 上的二进制与单元文件保留作回滚 | 回滚时 `systemctl --user enable --now cirunner`（见 §10.7），不依赖仓内当前代码 |

注意：`pkg/cloudinit` 是**复制**迁出，不是移动；原型 engine 仍用 `internal/cloudinit` 版本，两份会随时间分叉，这是有意为之（在役原型是钉死副本）。

**② ciorch 与 cirunner 的接口**

- 编译期：ciorch `go.mod` 依赖 `github.com/hoshinojian/cirunner v0.1.3`，只用 `pkg/cloudinit.Render`。
- 运行期：ciorch 经 ssh 在 runner 机器上执行 `/usr/local/bin/cirunner act <name> --json` 或 `diag --json`；机器上不反向调用编排器。
- 版本钉扎：配置 `machine_agent.version/sha256` 决定 cloud-init 下载哪个版本；sha256 校验失败 → cloud-init `exit 1` → 冷启失败暴露为场景 B（S4 冒烟正是用它注入故障）。

**③ 沙箱仓的角色**

- 不碰 nms 即可做故障注入：停 runner 服务、`kill -STOP` Listener、把 agent sha256 改成全 0、无人能满足的 label、7200s 长睡、停编排器。这些在真实仓上都不能做。
- 事故 issue 开在沙箱（私有），不污染真实项目。
- 只有 `workflow_dispatch`：不触发则零成本，也不会出现「无 runner 时 job 无限排队」污染事件面。
- 内容：`smoke.yml`（7 个短 job + 1 个 long-sleep，共 8 个，`runs-on: [self-hosted, ci-sandbox]`）、`smoke-badlabel.yml`（`nonexistent-label`）、`v1-sleep.yml`（V1 实测用 `sleep 300`）。**注释写「7 个并行 job」，实际是 8 个**，所以冒烟时 Q=8，而 smoke.md 写的是 Q=7。
- 沙箱对应编排器 profile `sandbox-sleep`（label `ci-sandbox`，规格 `s-1vcpu-2gb`，`hourly_cost_hint` 0.018）。
- 沙箱仓的 App 安装 installation id 为 184393577。

### 4.4 控制面与执行面

| 面 | 承载 | 持有的凭据 | 绝不做 |
|---|---|---|---|
| 控制面 | 编排器机器（ciorch + Caddy + vpsctl 二进制） | App 私钥、webhook secret、status token、DO token（vpsctl 账号文件只含 team2-ci）、编排器 ssh 私钥、DeepSeek key（均已脱敏，在 `/etc/ciorch/`，0600） | 不注册为 runner，不跑 CI |
| 执行面 | runner 机器（一次性） | 仅 1 小时时效的 registration token（会出现在 cloud-init 日志里） | 不调 DO / LLM API，不持长期凭据 |
| GitHub | 派发 job 给已注册 runner | — | 编排器「不接任务、不转发任务」 |

关键认知：**编排器不接任务**。GitHub 只把 job 派给已注册的 runner；编排器做的是「看见排队 → 准备好 runner → 盯着它们 → 用完回收」。因此编排器宕机不影响在跑的 CI（派发不经过它），只是不会再自动建删机（REDESIGN runbook §四.5）。

### 4.5 凭据分布（只列位置，内容均已脱敏）

| 凭据 | 位置 | 备注 |
|---|---|---|
| GitHub App 私钥 | 编排器 `/etc/ciorch/app.pem`（0600）；维护者 WSL `~/.config/ciorch/app.pem`（0600，副本） | WSL 副本是否仍需保留待维护者决定（见 §12.1 R9） |
| webhook secret | 编排器 `/etc/ciorch/webhook.secret`；GitHub App 设置页 | 64 位 hex 无尾换行；读取时 TrimSpace，不足 16 字节拒绝启动 |
| status token | 编排器 `/etc/ciorch/status.token` | `/status` 的 Bearer |
| DO token | 编排器 `/etc/ciorch/vpsctl-accounts.json`（仅 team2-ci 一条） | 维护者本机 `~/.config/vpsctl/accounts.json` 含全部账号 |
| 编排器 ssh 私钥 | `/etc/ciorch/ssh/id_ed25519`（0600，目录 0700） | 公钥经 cloud-init 写入 runner root authorized_keys |
| 维护者公钥 | `/etc/ciorch/maintainer.pub` | 人工救援用 |
| DeepSeek key | 编排器 `/etc/ciorch/deepseek.key`（0600） | **曾明文出现在聊天记录**，建议轮换，见 §10.9 |
| 个人 PAT | WSL `~/.config/ciorch/shadow.token`（0600，shadow 用） | 同 R9 |

### 4.6 其他相关机器

- **vpsctl 宿主机**（与 CI 间接相关）：`vpsctl-do-myteam-sgp1-01`（s-1vcpu-1gb，$6/月，sshd 40222，ufw 只放行维护者出口 IP 与 nms 生产宿主机），账号 `do-myteam`。**不承担 CI 编排**；是否与 ciorch 编排器有关联，素材未核实【不确定】（S-a171）。
- **已删除**：`gitrunner-team2-sgp1-02`（即 gitrunner-sgp2，DO id 618783288，runner 注册 id 22），2026-11-19 删除。

## 5. 工作原理（以代码为准）

> 本节以代码为准（ciorch `main@85e3efc`，含 PR #50；cirunner `main@b0225ce` = v0.1.3）。#51–#54 在此基线之后合并，相关差异在 §12.1 标注。行号引用来自 05 素材，代码继续演进后可能漂移，以 `grep` 为准。

### 5.1 端到端数据流

```
PR 推送 / workflow_dispatch
   │
   ▼ job 进入 queued
GitHub ──workflow_job webhook──► Caddy ──► ciorch /webhook（验签→白名单→JobCache.Put→Trigger(repo)）
   │                                                   │ 立即触发该仓 reconcile（Trigger 容量 1，多次触发合并）
   │   轮询兜底（活跃 15s / 空闲 60s）─────────────────┤
   ▼                                                   ▼
                                  observe（runs+jobs / runners / machines 三路）
                                                   ▼
                                  decide（纯函数）→ []Action + 新 RepoState
                                                   ▼
                          engine.execute（删在建前；动作互相隔离）← budget.Gate
                                                   ▼
   Provider.Create：取名 → 铸 registration token → cloudinit.Render → vpsctl create（不带 -wait）
                                                   ▼
   机器冷启 ~2.5 分钟：cloud-init 装底座 → 校验并装 agent → 装 actions runner → 注册 → 启动
                                                   ▼
runner online → GitHub 直接把 job 派给它 → 跑完 → 空闲满阈值 → Drain（先注销后删机）
```

代码路径（05 素材 §1.3）：

1. **webhook**：`internal/server/server.go`。HMAC-SHA256 校验 `X-Hub-Signature-256`，`hmac.Equal` 常量时间比较，失败 401，body 上限 5MB。`workflow_job`：仓不在白名单则 200 丢弃，否则 `Cache.Put(JobCacheEntry{Repo, Job, UpdatedAt})` 后 `Trigger(repo)`。`issues`：只处理 `action=closed`，且 sender 不是本 App bot、issue 带 `ciorch` 标签，才回调 `incident.HumanResolved`。`ping` 与其他事件一律 200。
2. **daemon**：`internal/daemon/daemon.go`。每仓一个 goroutine，启动先立即跑一轮，之后 `select{ctx, time.After(next), trigger}`。【代码与文档不一致】文档写「2s 防抖」，代码没有延迟，只合并重复触发。
3. **engine**：`internal/engine/engine.go`。`Reconcile` 与 `execute`；decide 已把动作排序为 RemoveReg/Drain/ForceDelete → Create → SSHAct → Incident。
4. **建机超时 90s**；Provider 先把 cloud-init 以 0600 落盘到 `state_dir/cloudinit/<名>.yaml`，成功即删，失败保留供诊断。

### 5.2 观察口径（observe，`internal/observe/observe.go`）

每仓每轮三路观察，各自记录成败：

| 路 | 做什么 | 要点 |
|---|---|---|
| jobs | `ListRuns(created>=now-24h)`，客户端跳过 `status=completed`；对每个未完成 run 取 `ListJobs(filter=latest)` | 不依赖 status 过滤参数（run 级状态对自建 runner 滞后，nms pitfalls 2026-11-15「GitHub API/rollup 显示怪象族」，REDESIGN 记作 P141②）；`per_page=100`，跟随 Link，最多 20 页 |
| runners | `ListRunners` | runner 的 labels 在 API 里是对象 `{id,name}`，不是字符串（PR #12，shadow 实录） |
| machines | `Provider.List(仓tag)` | vpsctl 一路 `errors[]` 非空即整体失败，绝不返回半截列表 |

加工规则：

- **webhook 缓存合并**：`Cache.Snapshot(now, repo)` 只取**本仓**、2 小时内的条目（PR #50）；同 job id 时，缓存条目收件时刻晚于轮询 job 的 `max(started_at, created_at)` 才覆盖。
- **状态过滤**：`queued` / `in_progress` 进入需求候选；`waiting`/`requested`/`pending` 及 in_progress run 中已 completed 的 job 只计入 `PendingJobs`。
- **label 匹配**：job labels（小写化）⊆ `{self-hosted, linux, x64} ∪ profile.labels` 时 Matchable。不可匹配的 queued job 进 `UnmatchedQueued`（场景 E），不计入 Q。
- **任一路失败**：`Complete=false`，分路记录 `JobsOK/RunnersOK/MachinesOK`，其余路照采。

**PR #50 修了什么**（2026-11-22，`d2bddd3`）：原 JobCache 全局一份、合并时不按仓过滤，nms 的 `[self-hosted,ci]` 排队 job 混入 sandbox 观察，导致 sandbox 误开 issue #8「label 不匹配：ci,self-hosted」（2 分钟后自愈关闭）；若 label 恰好相同还会被误算成本仓需求而建机。同时该 map 无锁而 webhook handler 每请求一个 goroutine 并发写，有 `fatal error: concurrent map writes` 崩进程风险。修复：`JobCacheEntry` 增加 `Repo`、`Snapshot` 按仓过滤、加 `sync.Mutex`；回归测试 `TestWebhookCacheScopedToRepo`、`TestJobCacheConcurrentAccess`（变异测试证明两条各自能抓住对应 bug）。注意：缓存条目只在读时按 2h 过滤、从不删除，map 无界增长（量级小）。

### 5.3 Decide 主流程（`internal/decide/decide.go`）

- 签名 `Decide(Params, RepoState, Obs) ([]Action, RepoState)`，纯函数：不读时钟、不做 IO，**入口深拷贝入参**（PR #11；`TestDecideDoesNotMutateInput` 钉住）。
- **观察不完整整轮冻结**（REDESIGN §2.2 原则 7，PR #7 由「只建不删」收紧）：只累计场景 H；机器一路成功时仍做 `syncMachines` 与硬寿命回收（防成本泄漏）；不建机、不做其他删除、不 ssh、不推进任何采样计数；但 `syncMachines` 会删掉已从列表消失的机器的 MachineState（decide.go:256-257、484-488）。代价：抖动期不扩容。shadow 期间 runners 解析 bug 连续 16 轮零动作、没有误建误删，即此机制在真实故障下生效。
- 观察完整时依次：`demand` 算 Q/QAge → `recordActivity` → `syncMachines`（机器消失即删其 MachineState）→ `buildViews`（维护 FirstOnlineAt / IdleSince / BusySince / OfflineSamples；首次 online 清 `Breaker.Fails`，half-open 转 closed）。
- **场景判定顺序**：`handleBadMachines` → C → B → A → D → G → E → H → RemoveReg。同一台机器一轮只做一个补救，优先级 C > B > A > D。
- 最后 `scaling`：扩缩容 + 硬寿命；本轮场景已产出的替换机计入在途供给，防重复建机。
- 需求与供给：
  - Q = Matchable 且 queued 的 job 数；QAge = 其中最老一个的年龄。
  - I = 所辖、online、空闲、非 Bad 的 runner 数。
  - P = 冷启在途（未 online、年龄 ≤ cold_start_timeout、非 Bad）机器数 × runners_per_droplet。
  - N = 本仓机器数。

### 5.4 参数默认值（`decide.go:133-156`，可由 `global.tuning` 覆盖；例外见表后注）

| 参数 | 默认 | 参数 | 默认 |
|---|---|---|---|
| `poll_active` | 15s | `poll_idle` | 60s |
| `stall_queue_age` | 2m | `stall_min_online` | 60s |
| `stall_samples` | 3 | `stall_restart_grace` | 2m |
| `cold_start_timeout` | 8m | `offline_samples` | 2 |
| `job_stuck_alert` / `kill` | 60m / 90m | `idle_timeout` / `busy` | 20m / 45m |
| `max_lifetime` | 8h | `breaker_max_fails` | 3 |
| `breaker_cooldown` | 30m（half-open 失败翻倍，封顶 4h） | `create_backoff_max` | 15m（**tuning 不生效**，见注） |
| `observe_fail_alert` | 10 轮 | | |

不可 tuning 的常量：E 宽限 5m、硬寿命超限告警 +2h、A/C 事件窗口 24h、LastActivity 窗口 2h。轮询档位：Q>0、有机器带 Remedy、有机器 BusySince 非零、有机器从未 online（冷启在途）任一成立用 `poll_active`，否则 `poll_idle`；文档里「有未结事故」这一条未实现。

> 注：`create_backoff_max` 虽能被 `ApplyTuning` 解析校验（tuning.go:25），但建机失败回填调用 `decide.RecordCreateResult(decide.Params{}, …)`（engine.go:277、294）传的是空参数，封顶恒为 15m——**配了也不生效**（代码问题，见 §12.1 B-20）。其余参数可正常覆盖。

### 5.5 场景逐条（`internal/decide/scenarios.go`）

场景字母沿用 REDESIGN §3.6.5。下表「判据」均为代码实际口径。

| 场景 | 判据（默认值） | 固定逻辑动作 | 何时升级为事故（开 issue） |
|---|---|---|---|
| **Bad 机 / Drain 残留** | 已标 Bad 的机器 | 每轮重发 ForceDelete（Remedy=draining 则重发 Drain），幂等。Remedy=draining 且注册已消失：判 Bad 并重试删机，不记为掉线（R7，PR #11） | — |
| **C 掉线** | 曾 online，本轮无 online runner，`OfflineSamples ≥ 2` | `Q>0 且 N<max_runners` 时先 Create 替换机；标 Bad、Remedy=replacing；ForceDelete（先删机后删注册） | 同仓 24h 内第 2 次（`OfflineEvents`），key=机器名 |
| **B 冷启失败** | 从未 online，机器年龄 ≥ `cold_start_timeout`(8m) | 替换条件同 C；ForceDelete；`Breaker.Fails++`；`Fails ≥ 3` 或处于 half-open 时转 open | **只在转入 open 的瞬间**开一次，key=`breaker`（仓级单发，PR #48） |
| **A 假活（P139）** | `Q>0 且 QAge ≥ 2m`，本机 online 且不忙（busy=false **且** jobs 接口无分给本机 runner 的 in_progress job），**本机持续空闲（`IdleSince`）≥ 2m**，上线 ≥60s，连续 3 轮（后两条为 PR #56 新增，见 §9.1 #28） | 替换条件同 C；对每个 online runner 发 SSHAct `restart-runner`，Remedy=restarting；`stall_restart_grace`(2m) 内转 busy 或 jobs 显示已接活则解除，仍不接活标 Bad、Drain | 同仓 24h 内第 2 次（`StallEvents`）。【代码与文档不一致】文档说「重启失败且无配额」也升级，代码在 SSHAct 失败时只记日志 |
| **D job 卡死** | 按 **job 粒度**：本机 online 且 busy 的 runner 名与某 in_progress job 的 `RunnerName` 相同，取 `now-StartedAt` 最大值；找不到 job 佐证就不判（防止连续接单的机器被误杀，PR #8） | ≥90m：标 Bad、ForceDelete | ≥60m 起每轮发 incident（由 incident 模块去重） |
| **D 变体：硬寿命超限** | 年龄 ≥ `max_lifetime+2h` 且仍忙 | 无 | 发 D 事故，key=机器名 |
| **G 外部 runner 假活** | `Q>0 且 QAge≥2m` 时，名字不在命名空间内、online 且空闲的 runner | **不操作** | 立即，key=runner 名；开 issue；配了 LLM 时直接转 waiting-human 并 @维护者、不调 LLM；未配 LLM 时停在 diagnosing，靠安静恢复收尾（diagnose.go:53-63） |
| **E label 无人能满足** | 按 label 组合分组，最老一个 ≥5m（常量，不可 tuning） | 不建机 | 立即，每种组合一个事故 |
| **H 观察失败** | 连续 `observe_fail_alert`(10) 轮 `Complete=false` | 整轮冻结 | 【代码与文档不一致】文档表写「连续 40 轮升级」，代码 ≥10 轮每轮都发 |
| **RemoveReg** | 命名空间内、offline、且没有同名机器（去掉 -a/-b 后缀再比）的注册 | ActRemoveReg | — |
| **I 建机报错** | create 返回 err（含铸 token、渲染失败） | `CreateFailStreak++`，退避 1m/2m/4m/8m/15m | streak **恰好 =3** 时发一次 |
| **F 配额/预算** | `Gate.Reserve` 拒绝 | 跳过本次建机 | QAge ≥15m 时发。【代码与文档不一致】「腾配额」（回收其他仓空闲机）**未实现**；`budget.Allocate` 只在测试里用 |
| **J 孤儿机器** | 带公共 tag 但无任何仓 tag | — | 【代码与文档不一致】`engine.ScanOrphans` 生产代码无调用，**未接线** |

### 5.6 扩缩容（`scaling`）

- **扩容**：`Q>0` 且不在 `CreateBackoffUntil` 退避期内。`want = ceil(clamp(Q-I-P, 0, (max_runners-N)*rpd) / rpd)`；half-open 时 want ≤ 1。不设「空闲 runner 先接」宽限：宁可偶尔多建一台（约 $0.14/h，空闲后回收），换速度。
- **缩容（空闲回收）**：前提 `Q==0 且 Complete`；机器非 Bad、无 Remedy；所有 runner online 且空闲；`now-IdleSince ≥ 阈值`。**一轮只回收一台**（IdleSince 最早者），Remedy=draining，发 Drain。
  - **自适应阈值**：`LastActivity` 近 2h 内的点数 ≥2 用 `idle_timeout_busy`(45m)，否则 `idle_timeout`(20m)。
  - **LastActivity 口径**（PR #47）：按**不同 RunID** 各记一个点，首次可见即定，用 `RecordedRuns` 跨轮去重。此前一个 8 job 的 run 被记 8 个点，把阈值顶到 45m，是冒烟 S1 多等约 45 分钟的原因。窗口 2h，最多 20 点。
- **硬寿命**：年龄 ≥ `max_lifetime`(8h) 且不忙 → Drain；观察不完整（只要机器一路成功）也执行。
- `Q>0` 时绝不做空闲回收（硬寿命除外）。

### 5.7 熔断器（Breaker）

| 转换 | 条件 |
|---|---|
| closed → open | B 场景 `Fails ≥ 3` |
| open → half-open | 冷却满（默认 30m）**且 Q>0**（在 scaling 里判定，Q=0 时一直停在 open） |
| half-open → closed | 任一机器首次 online，`Fails=0` |
| half-open → open | 试探机冷启失败，冷却翻倍（封顶 4h）；half-open 时 P>0 就不再建（只一台试探，PR #8） |
| 人工复位 | 对应 issue 被人关闭 → `HumanCleared`：清熔断、`CreateFailStreak=0`、清退避、清 pause/switch_pool |
| LLM 动作 | `clear_breaker` |

`ciorch reset <repo>` 子命令**不存在**，见 §12.1。

### 5.8 执行面细节（engine）

- **Create**：先查 pause → `Gate.Reserve` → create → `Commit`（成功）/ `Abort`（失败）。失败时 `RecordCreateResult`，并把错误原文写入 `RecentProviderErrors`（≤10 条，供诊断材料 `recent_vpsctl_errors`；PR #48）。同一轮多个 create 取名时并入本轮已建名，防撞名（PR #43：此前两台同名，GitHub 拒绝第二台注册，成了不被管理的僵尸机继续计费）。
- **Drain**：`RunnerID≠0` 时先 `DELETE runner`，失败则本轮放弃删机、下轮重判。**V1 已实测**：GitHub 对忙碌 runner 返回 422，对空闲返回 204，所以先注销后删机能防「读到 busy=false 与删机之间 job 刚派到」的竞态。再按名清理 `-a/-b` 实例 → 按机器 Pool 路由 `Provider.Delete` → 预算入账 → `OnRemedy`。
- **ForceDelete**：先删机，再按 id 删注册。代码里有「id 缺失时按名」兜底（engine.go:331-335），但 decide 产出的 ForceDelete 从不填 `RunnerName`，兜底实际不会触发；id=0 时残留注册由后续轮次的 RemoveReg 清理。
- **SSHAct**：`ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i <key> root@<Addr> /usr/local/bin/cirunner act <name> --json`，Addr 由 provider 选定、私网优先。代码里**没有 60s 超时**，只受 ctx 约束。
- 【代码与文档不一致】文档要求 Create/SSHAct 在 goroutine 里异步执行，实际在每仓 goroutine 内**同步**执行。各仓并行，一个仓建机最长阻塞本仓约 90s，不影响别的仓。
- **dry-run**：`Reconcile(dryRun=true)` 只打日志。**PR #52 之前**事故模块未置 DryRun，dry-run 仍会 `fireIncident`，`ciorch plan` / `daemon -dry-run` 会真开 issue、真调 LLM、真执行 LLM 动作（`status` 只做 Collect、不走 Reconcile，不受影响）；#52 的 wiring 增加 readOnly 后已修。

### 5.9 预算与配额（`internal/budget/budget.go`）

- 成本：`Machine.HourlyCost = price_monthly / 672`（DO 月封顶口径）。
- **Gate.Reserve**（互斥锁内一次判三件事）：
  1. 对**所有池** `List("ciorch")` 按（池, ID）去重，加在途占坑，须 `< global.max_machines`；
  2. 目标池机器数加占坑，须 `< 池 max_machines`；
  3. 当日花费 + 新机 1 小时投影价须 `≤ daily_budget_usd`。当日花费 = 已删机器入账 + 活机当日存活小时 × 时价；投影价取池里最近一台机器的时价，池空用 `hourly_cost_hint`，都没有就拒绝。任一池 List 失败保守拒绝。
- 占坑：保留到机器出现在列表里，或自 **Reserve** 起 TTL 2 分钟（budget.go:260、290；Commit 只补机器名）。建机本身最长 90s，所以 Commit 后实际只剩约 30s–2 分钟。
- 删机入账：`RecordDeletion`；当日花费持久化在 state.json 的 `budget.day_utc/spent_today`，跨 UTC 日清零。
- 为何按「新机 1 小时」投影：按满 24h 投影的话，$5 日预算永远建不了 c2；真实花费由 SpentToday / RecordDeletion 兜底。

### 5.10 事故管线（`internal/incident` + `internal/llm`）

**类型、键、去重**

- 场景码 A–J，issue 标签 `ciorch` + `ciorch:<小写场景码>`；事故键 = (repo, Type, Subject)，Subject 取机器名 / runner 名 / label 组合 / `breaker` / 空串。
- 同键事故仍活跃（非 resolved）→ 只更新 LastSeen 并清零 QuietRounds，不再开 issue。同键 resolved 后 24h 内复发 → reopen 原 issue 并追加评论；超过 24h 开新 issue。
- B/C/D 的固定逻辑强删成功后经 `OnRemedy → NoteRemedy` 只写 `incidents.jsonl` 并计数（A 的 SSHAct 与其后的 Drain 不记录：engine.go:342-350，Drain 无 `Scenario` 字段），不建 Incident、不开 issue；是否开 issue 完全取决于 decide 有没有发 ActIncident（即上表「升级」列）。

**状态机（代码实际）**

```
Handle：(none/resolved) ──► detected ──开/复用 issue──► diagnosing
Tick（daemon 每轮、每仓）：
  diagnosing + G ───────────────────────────► waiting-human（不调 LLM；仅当配置了 LLM）
  diagnosing + 限额已满（每事故 3 / 每日 20）──► waiting-human（评论注明上限）
  diagnosing ──LLM──┬ 调用失败 ─► 留在 diagnosing（轮次已计，下轮重试）
                    ├ 非法 JSON / 校验降级 ─► waiting-human（附原文）
                    ├ needs_human ─► waiting-human（needs-human 标签 + @维护者 + human_steps）
                    └ 合法动作 ─► 执行 ─► verifying（评论「已执行动作」）
  安静恢复：凡非 resolved、非 waiting-human 的事故 QuietRounds++；≥2 → resolved，评论「已恢复」并关 issue
issues.closed（人工，非本 App bot）─► resolved + HumanCleared
```

【代码与文档不一致】`StateActing` 定义了但没用；**verifying 不会回到 diagnosing**（文档说「未恢复且轮次<3 带新证据再诊断」，代码没有这条转换）。若判据每轮复发，Handle 每轮把 QuietRounds 清零，事故就停在 verifying，既不再诊断也不转人工；「每事故 3 次」实际只在 LLM 调用失败重试时才会耗尽。

**issue 内容**：开在 `issue_repo`（空则本仓；nms 当前为空，即开在 nms 私有仓）。标题 `[ciorch] <场景中文名>：<主体>（<repo>）`；正文四段（发生了什么、时间线、固定逻辑处置、下一步）；每轮 LLM 结论与动作结果各追加一条评论；单条评论超 60000 字符时截断保留末尾。「下一步」固定写「正在调用大模型诊断」，即使 LLM 未配置也这么写。【代码与文档不一致】文档要求编排器自身问题（vpsctl 不可用、App 认证失败）开在 ciorch 仓，**未实现**。

**诊断材料**（`incident/evidence.go`，确定性采集，不脱敏）：incident 元信息、timeline、repo_state、jobs、runners、machines、`machine_diag`（主体机优先最多 3 台，经 ssh 调 `cirunner diag --json`）、`job_log_tail`（仅 D，最老 in_progress job 日志末尾 32KB）、`recent_vpsctl_errors`、`unmatched_queued`、`previous_rounds`。总量超 100KB 按 `job_log_tail` 砍半 → 非主体机 diag 移除 → 丢最老 timeline 的顺序截断。

**DeepSeek 调用**（`internal/llm`）：`POST {base}/chat/completions`，`model=deepseek-chat`，`temperature=0`，`response_format=json_object`，`max_tokens=2000`，超时 90s，5xx 或网络错误重试 1 次（0.5s 后）、4xx 不重试。系统提示词（中文）含架构简介、已知坑、本事故类型允许的动作、输出 schema、约束，**且必须含小写 "json"**（JSON 模式要求）。

**白名单与校验**（`incident/verdict.go`）：

| 场景 | 允许动作 |
|---|---|
| A | none, restart_runner, replace_droplet, drain_droplet |
| B | none, switch_pool, clear_breaker, pause_repo |
| C | none, replace_droplet |
| D | none, drain_droplet, rerun_failed_jobs |
| E / G / J | none（G 根本不调 LLM） |
| F | none, drain_droplet |
| H / I | none, pause_repo |

任一校验不过 → 降级为 `needs_human=true, action=none`，issue 注明「大模型结论不合法」并附原文：JSON 合法、`root_cause` 非空、`confidence∈[0,1]`；action 在本场景白名单；机器类动作的 `machine` 必须是**可下手的机器**（在最新内存状态里存在、未标 Bad、Remedy 不是 replacing/draining，`actionableMachines`，PR #48 修了 S3 中 LLM 对已强删机器选 replace 仍被放行）；`switch_pool` 的池须在 profile.pools（当前长度 1，实际不可迁移）；`pause_repo` 的 `duration_min∈[10,360]`；`rerun_failed_jobs` 的 run_id 须在近 24h 已完成 run 内且失败 job 日志末尾 64KB 含 `lost communication` / `shutdown signal` / `The operation was canceled` 之一；`confidence<0.5` 且 action≠none 降级。

注意 `rerun_failed_jobs` 的证据采集有上限：全仓最多检查 5 个失败 job（cmd/ciorch/main.go:205-209、221），每个 run 只看第一个失败 job（main.go:236）；超出上限的 run 即使符合特征也会被判越权而降级。

动作执行（`engine/incident_exec.go`）：restart_runner 走 ssh；drain_droplet 按名删注册后删机（**不先确认注销成功**，与固定逻辑 Drain 口径不同）；replace_droplet 先试建（过 Gate），失败则先 Drain 旧机再建；pause_repo 只拦建机；switch_pool 临时 6h；clear_breaker；rerun_failed_jobs。`pausedUntil` 与 `poolOverride` **只在内存里**，守护重启即丢。

**人工关 issue 的语义**：webhook `issues.closed`、issue 带 `ciorch` 标签、sender 不是本 App bot（`<app_slug>[bot]`）→ `HumanResolved(issue仓, number)`：按 (IssueRepo, number) 找事故；已是 resolved（编排器自己关 issue 的回声）则忽略；否则标 resolved，并对**托管仓**调用 `HumanCleared`。端到端测试 `cmd/ciorch/wiring_test.go`。

**调用上限**：每事故 `LLMRounds ≥ 3` 或当日 `llmDayCount ≥ 20` 转 waiting-human；日计数持久化在 state.json 的 `llm_day`。【代码与文档不一致】配置项 `llm.max_calls_per_incident / max_calls_per_day` **未接线**，永远用默认 3/20。

### 5.11 持久化与守护

- `state_dir/ciorch.lock`：flock，拿不到就退出防双开（注意 HTTP 监听在取锁**之前**就启动）。
- `state_dir/state.json`：每仓每轮原子写（tmp+rename，0600），内容 `{repos, budget, incidents, llm_day}`，不含 webhook 缓存；恢复失败按全新启动。
- `state_dir/incidents.jsonl`：追加写。
- `state_dir/cloudinit/`：建机失败遗留的 user-data（含 1h token）。【代码与文档不一致】文档写「保留 7 天自动清」，**未实现**清理。
- 恢复语义：冷启与寿命以 DO 的 `CreatedAt` 为准，重启后不误判（冒烟 S7 实测，6 次以上换二进制均无损）。

### 5.12 机器侧（cirunner）

**cloud-init**（`cirunner/pkg/cloudinit/cloudinit.go`），按序做：

1. `ssh_pwauth: false`；`ssh_authorized_keys` 写编排器与维护者公钥（vpsctl `--authorized-keys` 不进自定义 user-data）。
2. `packages`：docker.io / jq / curl / build-essential / shellcheck + `extra_packages`。
3. `write_files`：`20auto-upgrades` 设 `Unattended-Upgrade "0"`；sshd 同时听 22 与 443（救援通道）；`/var/lib/cirunner/stage` 初值 `packages`。
4. `users`：`runner` 用户，docker 组，免密 sudo（gitleaks/sqlc job 需要 `sudo install`）。
5. `runcmd`：启 docker；`chage -d today root`（DO 镜像 root 密码过期会挡公钥登录）；禁 ssh.socket 直接跑 ssh.service；**下载 agent 并 `sha256sum -c`**（前有 `set -e`，失败 exit 1，冷启失败暴露为场景 B；`agent` 标记在该步成功后才写，失败时 stage 停在 `packages`）；取 latest actions runner（只做版本号**格式**守卫，不比较大小；避开 <2.329.0 靠一律取 latest），成功后写 stage=`runner-download`；`config.sh` 注册（stage=`runner-config`）；`svc.sh install` → `daemon-reload` → `systemctl start actions.runner.<owner-name>.<name>.service`（unit 名用连字符，stage=`runner-started`）。
6. `/var/lib/cirunner/baked` 存在（快照镜像）则跳过 docker 启用、agent/runner 下载；但**快照烘焙未实现**（阶段 7）。
7. rpd=2 时渲染两个实例（`/opt/actions-runner-a`/`-b`，runner 名 `<name>-a`/`-b`），实验特性、未真机验证。
8. 渲染前校验：公钥无引号/换行；各字段正则；labels 非空；版本 `vX.Y.Z`；sha256 64 位小写 hex；产物纯 ASCII（P75）且 ≤64KB；有 golden 文件。

**机器上的 CLI**（`cmd/cirunner/machine.go`，需 root；退出码 0 成功 / 1 检查不过或动作失败 / 2 用法错误）：

- `health [--json]`：cloud-init status 与 stage、各 `actions.runner.*` unit、Listener / Worker 进程、docker、根分区可用率、可用内存、load1、runner 版本（`config.sh --version`）、uptime。`ok=false` 条件：cloud-init error、无 unit 或有 unit 非 active、Listener 不在、docker 非 active、磁盘可用 <10%。
- `diag [--json] [--lines=200]`（必须用 `=` 形式；`--lines 200` 会因参数分离逻辑以退出码 2 失败，machine.go:31-44）：health 全量 + cloud-init-output.log、runner journal、`_diag/Runner_*.log` 与 `Worker_*.log` 各最新一个（P139 关键证据）、docker journal、`docker ps -a`、`df -h`、`free -m`、dmesg 尾部、20auto-upgrades、apt history；单段失败只在该段写 error；总输出 ≤1MB。**health 不 OK 时退出码为 1**（PR #53 之前，ciorch 的 ssh 封装遇非零退出就丢弃 stdout，最需要诊断时 `machine_diag` 只剩 `exit status 1`；#53 改为 exit 1 且输出为合法 JSON 时保留）。
- `act <restart-runner|stop-runner|clean-work> [--json]`：restart 重启全部 runner unit 并等 30s 内 Listener 数达到 unit 数；stop 停止；clean-work 仅当 runner 服务都不 active 时才清 `/opt/actions-runner*/_work/*`；其他名字退出码 2。flag 写在位置参数后也能识别（v0.1.2，PR #5）。机器侧代码实际用 `pgrep -f`（sys.go:96-107）——由 Go 直接 exec、外面没有 shell，pgrep 会排除自身，所以不自匹配；V6 的自匹配坑只出现在经 ssh `bash -c '… $(pgrep -f Runner.Listener)'` 的人工命令里。REDESIGN §5.4（PR #26 加的注记）称机器侧用 `pgrep -x`，与代码不符。

**发布**：tag `v*` → `release.yml` 先跑 vet+test，再 `CGO_ENABLED=0` 静态构建 linux/amd64，上传 `cirunner-linux-amd64` 与 `.sha256`；`softprops/action-gh-release` 钉 commit SHA（它持有 `contents: write`）。

| tag | commit | 内容 |
|---|---|---|
| v0.1.0 | 980e572 | 阶段 3 首发（PR #3） |
| v0.1.1 | ff6d590 | runner 版本改 `config.sh --version`（PR #4） |
| v0.1.2 | 3858e90 | act 支持 flag 后置（PR #5） |
| v0.1.3 | b0225ce | Listener 计数、dash 兼容、CI 门禁、action 钉 SHA（PR #6） |

线上 `machine_agent` 钉 v0.1.3，sha256 `2239fe3d…a940b2d`（部署时核对过两次，素材 04 §1.6）。

### 5.13 CLI、HTTP 与可观测性

**CLI**（`cmd/ciorch/main.go`）：实际只实现 `stats`、`plan`、`status`、`daemon [-dry-run]`；`-config` 可在子命令前后。【代码与文档不一致】REDESIGN §3.13 的 `up`、`down`、`reset`、`check-config`、`bake` **全部没有实现**，runbook 与 smoke.md 里引用这些命令的地方是错的（实测引用处：runbook.md:110 `check-config`、runbook.md:120 `up`/`down`、smoke.md:168 `reset`；`bake` 只出现在 REDESIGN.md:806-808、898；REDESIGN.md:439、642-646 另有 reset/check-config/up/down）。复位熔断目前只能靠：人工关 issue、LLM 的 `clear_breaker`、或停服务后手改 state.json。`ciorch stats` 在 PR #54 之前只走 `gh auth token` 兜底，编排器上直接失败；#54 抽出 ghClient，使 stats 与 daemon 一样优先 App 认证（编排器上实测正常：近 24h 3 run，冷启代理 n=5 avg 2m30s）。

**HTTP**（监听地址来自配置 `listen`，线上为 `127.0.0.1:8080`，公网经 Caddy；代码不强制回环；只有配置了 `github_app.webhook_secret_file` 才起 HTTP，否则连 /healthz 也没有）：

| 端点 | 行为 |
|---|---|
| `GET /healthz` | 无鉴权；`lastOK` 距今 <3×`poll_idle`（180s）→ 200 `ok`，否则 503。【代码与文档不一致】`lastOK` 在**任一仓**某轮观察完整时更新，文档写「全仓」 |
| `GET /status` | 需 `Authorization: Bearer <status token>`；token 为空一律 401；返回 `{repo: {Q,I,P,N,QAge(ns),Complete,Breaker,Machines}}`。【代码与文档不一致】文档要求的机器列表、未结事故、今日花费都没有；token 比较非常量时间（小问题） |
| `POST /webhook` | 见 §5.1 |

**日志**：标准库 `log`（不是 slog），journal 收集：`journalctl -u ciorch -f`。每轮一行摘要 `[owner/name] Q=.. QAge=.. I=.. P=.. N=.. actions=[...|无] complete=true`；动作日志 `已建机 <name>`、`已回收`、`已强删`、`Drain 注销失败，保留机器下轮重判`、`跳过建机（场景 F）`、`事故 X/subject: reason`；每小时一行 `api-usage 1h: requests= 304= remaining a→b`。

**外部健康检查**（`ciorch/.github/workflows/healthcheck.yml`）：cron `7 * * * *` + 手动 dispatch，托管 runner；`curl -fsS https://$vars.CIORCH_HOST/healthz`；变量缺失红叉失败（PR #49）；失败时自建 `ciorch-down` 标签、新开 issue 时 @仓主，已有 open issue 则只追加评论（不 @）。GitHub 对 60 天无活动仓库停用 schedule；实测 schedule 触发很稀疏（11-21 20:26 到 11-22 01:07 之间只见 2 次），不要依赖它做分钟级告警。

## 6. 配置参考

> 来源：加载入口 `ciorch/internal/config/config.go`，示例 `ciorch/configs/ciorch.example.json`（全部占位符）。**线上配置本文没有直接读取**（本次起草不碰线上系统），§6.3 的线上结构由 04 素材（部署时）加协调者记录的 07:23 改动拼出，以编排器上 `/etc/ciorch/ciorch.json` 的实际内容为准。

### 6.1 加载与校验

1. `checkLegacyFields`：发现旧 schema 字段（顶层 `vpsctl_bin` / `vpsctl_accounts_file`、`global.max_droplets`、profile 内的 `account/region/size/image/snapshot/fallback_regions`）就报迁移提示，不做兼容解析。
2. `json.Decoder.DisallowUnknownFields()`：**未知字段一律拒绝**。理由：拼错键名而静默走默认值（比如 `max_runners` 拼错悄悄变 1）的风险大于前向兼容的损失（PR #2）。
3. `validate()`：错误信息是中文，指出字段路径。池私有配置由对应 provider 的 Factory 在启动期试构造校验。

### 6.2 字段参考

**顶层**

| 字段 | 类型 | 含义 | 校验 / 备注 |
|---|---|---|---|
| `github_app.app_id` | int64 | App ID | 与 `token_file` 二选一 |
| `github_app.private_key_file` | string | App 私钥 PEM（PKCS#1 或 PKCS#8） | `app_id≠0` 且路径非空才走 App 认证 |
| `github_app.webhook_secret_file` | string | webhook secret 文件 | 配置后才起 HTTP 面；读取时 TrimSpace，去空白后 <16 字节拒绝启动 |
| `github_app.app_slug` | string | 可选；用于识别 `<slug>[bot]` 自关 issue 的 webhook | 不填则该过滤关闭，仍有事故侧状态防护兜底 |
| `token_file` | string | 开发备选 PAT | 与 App 互斥；内容 <16 字节报错 |
| `pools` | map | 机器池，见下 | 至少 1 个 |
| `ssh_private_key_file` | string | 编排器对 runner 的 ssh 私钥 | 无校验 |
| `ssh_public_key_files` | []string | 写入 runner root `authorized_keys` 的公钥文件（编排器 + 维护者） | 逐行 trim、空行跳过（PR #42）；全部读失败则 Render 报「PublicKeys 不能为空」 |
| `machine_agent.version` / `.sha256` | string | cloud-init 要下载的 cirunner 机器侧版本与校验和 | config 层不校验；建机时由 cirunner Render 校验（`vX.Y.Z`、64 位小写 hex） |
| `listen` | string | HTTP 监听地址 | 应为 `127.0.0.1:8080` |
| `status_token_file` | string | `/status` 的 Bearer token 文件 | 为空时 `/status` 永远 401；示例配置与 runbook 都没列这个字段（R8，§12.1） |
| `state_dir` | string | 状态根目录 | 必填 |
| `global.max_machines` | int | 全局并发机器上限 | ≥ 任一仓 `max_runners` |
| `global.daily_budget_usd` | float | 每日预算（UTC 自然日） | >0 |
| `global.tuning` | map[string]string | 决策参数覆盖（键见 §5.4） | 每轮 `decide.ApplyTuning` 严格解析，未知键或坏值使该仓 reconcile 报错（PR #44 接通） |
| `llm.base_url` / `model` / `api_key_file` | string | 默认 `https://api.deepseek.com` / `deepseek-chat` | key 文件读失败或为空 → 诊断禁用，事故照常开 issue |
| `llm.max_calls_per_incident` / `max_calls_per_day` | int | 文档：3 / 20 | **未接线**，代码永远用默认 3/20 |
| `maintainer_login` | string | issue 里 @ 的人 | 无校验 |
| `profiles` | map | 见下 | 至少 1 个 |
| `repos` | []object | 见下 | 至少 1 个 |

**pools.\<名\>**：只允许三个键 `provider`、`max_machines` 与和 provider 同名的子对象，多出任何键报错。`provider` 须已注册（当前只有 `vpsctl`，经 `cmd/ciorch/main.go` 的 blank import 注册，PR #23 修过漏注册）；`max_machines` ≥1，池容量。

**pools.\<名\>.vpsctl**（同样 `DisallowUnknownFields`）

| 字段 | 含义 | 校验 |
|---|---|---|
| `bin` | vpsctl 二进制路径 | 必填 |
| `accounts_file` | `-accounts` 文件（只含 CI 账号 team2-ci） | 可选 |
| `account` | 钉 vpsctl `-only` | 必填（防全账号误建） |
| `region` | DO region slug | 必填，正则 `^[a-z]+[0-9]+$` |
| `size` / `image` | 规格 / 镜像 | 必填；nms 必须 `c2-4vcpu-8gb` / sgp1（#148） |
| `snapshot` | 非空时代替 image（阶段 7 预留） | 无 |
| `prefer_private_ip` | 缺省 true，`Machine.Addr` 优先私网 IP | 无 |
| `hourly_cost_hint` | 空池时预算估价（USD/h） | **必须 >0** |

**profiles.\<名\>**

| 字段 | 含义 | 校验 |
|---|---|---|
| `pools` | 有序池列表 | **长度强制为 1**，>1 报「多池调度尚未实现」 |
| `labels` | runner 自定义 label（`self-hosted/linux/x64` 为自动标签，不必写） | 每项 `^[A-Za-z0-9_-]+$`；config 允许空，但 cirunner Render 要求非空，空 labels 会使建机在渲染阶段失败 |
| `runners_per_droplet` | 0 视为 1，取值 1–2 | 2 为实验特性，默认关 |
| `extra_packages` | 底座五包之外追加的包 | 正则 `^[a-z0-9][a-z0-9+.-]+$` |

**repos[]**

| 字段 | 含义 | 校验 |
|---|---|---|
| `repo` | `owner/name` | 不得重复 |
| `profile` | 引用的 profile | 必须存在 |
| `max_runners` | 每仓机器上限 | ≥1 |
| `issue_repo` | 事故 issue 开在哪个仓；空 = 本仓 | 非空时 App 也必须装在那个仓上（issue 写操作按 issue 仓取 installation token） |

**命名与 tag**（归属判定的基础）

- 公共 tag `ciorch`；仓 tag `ciorch-r-<sanitize(owner,20)>-<sanitize(name,20)>-<sha256(owner/name) 前 8 位>`；名前缀 `ci-<sanitize(name,20)>`；vpsctl 命名 `{prefix}-{account}-{region}-NN`，**droplet 名 = runner 名**，例如 `ci-nms-team2-ci-sgp1-01`。
- 编排器自身用 `ciorch-host`，不匹配公共 tag，所以不计配额也不会被删。
- 序号「跳号」只相对当前列表：机器全删后序号会从 01 重用，所以按名字统计冷启要改按 runner_id（cirunner 原型 stats 的教训，ciorch PR #1）。
- runner 名须匹配池 Namer 的 `Pattern`（`^<prefix>-<account>-<region>-\d{2}$`）才算本仓所辖；不匹配的视为外部 runner，**绝不触碰**。

### 6.3 当前线上配置（结构示例，脱敏）

文件：`/etc/ciorch/ciorch.json`（0600，属主 ciorch）。**2026-11-22 07:23 UTC 切换前已备份为 `ciorch.json.bak-pre-nms`**。切换时的改动：新增 pool `do-sgp1-c2`、profile `go-heavy`、repo `hoshinojian/nms`；`global.max_machines` 2→3；`daily_budget_usd` 3→8。候选文件先经 `ciorch plan` 校验（nms complete=true、Q=0、无动作）再替换。

```json
{
  "github_app": {
    "app_id": 5863295,
    "app_slug": "ciorch",
    "private_key_file": "/etc/ciorch/app.pem",
    "webhook_secret_file": "/etc/ciorch/webhook.secret"
  },
  "pools": {
    "do-sgp1": {
      "provider": "vpsctl",
      "max_machines": 2,
      "vpsctl": {
        "bin": "/usr/local/bin/vpsctl",
        "accounts_file": "/etc/ciorch/vpsctl-accounts.json",
        "account": "team2-ci", "region": "sgp1",
        "size": "s-1vcpu-2gb", "image": "ubuntu-24-04-x64",
        "prefer_private_ip": true, "hourly_cost_hint": 0.018
      }
    },
    "do-sgp1-c2": {
      "provider": "vpsctl",
      "max_machines": 2,
      "vpsctl": {
        "bin": "/usr/local/bin/vpsctl",
        "accounts_file": "/etc/ciorch/vpsctl-accounts.json",
        "account": "team2-ci", "region": "sgp1",
        "size": "c2-4vcpu-8gb", "image": "ubuntu-24-04-x64",
        "prefer_private_ip": true, "hourly_cost_hint": 0.1399
      }
    }
  },
  "ssh_private_key_file": "/etc/ciorch/ssh/id_ed25519",
  "ssh_public_key_files": ["/etc/ciorch/ssh/id_ed25519.pub", "/etc/ciorch/maintainer.pub"],
  "machine_agent": { "version": "v0.1.3", "sha256": "<已脱敏/见 §5.12>" },
  "listen": "127.0.0.1:8080",
  "status_token_file": "/etc/ciorch/status.token",
  "state_dir": "/var/lib/ciorch",
  "global": { "max_machines": 3, "daily_budget_usd": 8.0 },
  "llm": { "base_url": "https://api.deepseek.com", "model": "deepseek-chat",
           "api_key_file": "/etc/ciorch/deepseek.key",
           "max_calls_per_incident": 3, "max_calls_per_day": 20 },
  "maintainer_login": "hoshinojian",
  "profiles": {
    "sandbox-sleep": { "pools": ["do-sgp1"], "labels": ["ci-sandbox"], "runners_per_droplet": 1 },
    "go-heavy":      { "pools": ["do-sgp1-c2"], "labels": ["ci"], "runners_per_droplet": 1 }
  },
  "repos": [
    { "repo": "hoshinojian/ciorch-sandbox", "profile": "sandbox-sleep", "max_runners": 2 },
    { "repo": "hoshinojian/nms", "profile": "go-heavy", "max_runners": 2, "issue_repo": "" }
  ]
}
```

要点与不确定处：

- `hourly_cost_hint` 0.1399 = 94/672；沙箱池 0.018 对应 `s-1vcpu-2gb` $12/月。
- 以上结构已于 2026-11-22 由协调者在编排器上逐项核实（密钥类字段只看路径）：沙箱池 `do-sgp1` 的 `max_machines`=2，`do-sgp1-c2` 的 `max_machines`=2。
- `status_token_file` 线上已配置（核实）；仓内示例配置遗漏它（R8）。`llm.max_calls_*` 线上写了 3/20，但代码不读这两项（§12.1 B-9）。
- `machine_agent.version` 写 v0.1.3 来自部署记录（04 §1.6）；示例配置里写 v0.1.0。线上是否在 11-22 之后变动，以文件为准。
- 沙箱仓的 `max_runners` 为 2（04 §1.5）。
- 切换时 `global.max_machines`=3 ≥ 任一仓 `max_runners`，满足校验；池容量之和（沙箱池 + c2 池）与全局上限的关系不强制。
- 冒烟期间曾临时加过 `global.tuning`（S5：`job_stuck_alert/kill` 3m/5m；回归：`idle_timeout/idle_timeout_busy` 5m），已清空；若线上文件出现 `tuning`，说明有人在做实验，记得还原。

### 6.4 编排器机器上的文件布局

| 路径 | 用途 | 权限 / 属主 |
|---|---|---|
| `/etc/ciorch/` | 配置与凭据目录 | **0700** ciorch（线上；仓内 `deploy/install.sh` 写 0750，口径不一致，见 §12.1） |
| `/etc/ciorch/ciorch.json` | 主配置 | 0600 ciorch |
| `/etc/ciorch/{app.pem,webhook.secret,status.token,deepseek.key,vpsctl-accounts.json}` | 凭据（内容均已脱敏） | 0600 ciorch |
| `/etc/ciorch/maintainer.pub` | 维护者公钥 | 0644 |
| `/etc/ciorch/ssh/id_ed25519(.pub)` | 编排器 ssh 密钥对 | 0600 / 0644，目录 0700 |
| `/etc/ciorch/ciorch.json.bak-pre-nms` | 切换前配置备份（回滚用） | 0600（预期） |
| `/var/lib/ciorch/{state.json,incidents.jsonl,cloudinit/,ciorch.lock}` | 状态目录 | 0700 ciorch |
| `/usr/local/bin/ciorch`、`/usr/local/bin/vpsctl` | 二进制；旧版按 `ciorch.bak-<sha12>` 保留（见 §10.2） | root |
| `/etc/systemd/system/ciorch.service` | unit：`User=ciorch`，`Restart=always`，`RestartSec=5`，`StateDirectory=ciorch`，`NoNewPrivileges`，`PrivateTmp` | — |
| `/etc/caddy/Caddyfile` | `168-144-104-79.sslip.io { reverse_proxy 127.0.0.1:8080 }` | caddy |

ufw：默认拒入，只放行 22 / 80 / 443（v4+v6）。

## 7. 演进历程（时间线）

> 时间一律 UTC。nms 侧编号为 nms 仓 PR/决策/pitfalls，ciorch、cirunner 的 PR 会注明仓。

### 7.1 阶段总览

| 阶段 | 时间 | 做了什么 | 主要 PR / 产物 | 结论 |
|---|---|---|---|---|
| 前史 | ≤11-14 | 托管分钟耗尽 → 自建常驻 runner（sgp1，后换 c2 专用核 sgp2）；P139 静默停摆；基线 89.2→89.1 | nms #145、#148、PR #590/#591/#598/#599 | 常驻机 $94/月，单机串行 |
| 原型 | 11-15 | cirunner 原型：WSL 守护 + vpsctl 建删 + cloud-init | cirunner b1ba8bb、47221cc、508a0b0 | 真机冒烟七项全绿，test 17m41s，棘轮通过 |
| 并行化 | 11-16 | 双 runner + `-p 1`→`-p 4` | nms #612、#613、#615 | test 19m14s→11m35s |
| 设计 | 11-19～20 | 提出便宜编排器；删 sgp2；写 REDESIGN.md（初稿 915 行） | REDESIGN.md | 两仓架构 + 阶段 0–7 |
| 阶段 0a/0b/0c | 11-20 | 原型热补丁；nms runbook+#151；ci.yml 加 timeout | cirunner #1；nms #622、#623 | 通过（但「#622 走 docs-only」说法不实，见 §9） |
| nms 小修 | 11-20 | quotepath 修复 P144；#150 撞号→#152；验证 PR | nms #624、#625、#626 | #626 日志证实 `docs_only=true`，test 48s |
| 阶段 1 | 11-20 | ciorch 骨架 + `ciorch stats` | ciorch 首提交 1d72f0a、#1、#2 | 打回 4 处后通过 |
| 阶段 2 | 11-20 | 决策核心（4 个 PR，约 5500 行） | ciorch #3–#6；修正 #7–#11 | 首审**不通过**（10 条必修），复审基本通过 |
| shadow | 11-20 10:25 → 11-21 10:25 | `daemon -dry-run` 与原型并跑 24h | ciorch #12–#14 | 通过；抓到 labels 对象解析 bug |
| 提供商抽象 | 11-21 10:46–13:14 | Provider 接口，行为零变化 | ciorch #16–#23、#28 | golden 未改，通过 |
| 阶段 3 | 11-21 10:53–12:53 | cirunner 机器侧 + V1/V6 实测 + 渲染器接入 | cirunner #2–#6，v0.1.0–v0.1.3；ciorch #24–#26 | 通过 |
| 阶段 4/5 代码 | 11-21 13:03–14:09 | App 认证、webhook、deploy/、事故管线、LLM | ciorch #27、#29–#39 | 通过，但抓出 3 处必修（#39） |
| 部署 | 11-21 15:39–16:22 | 建编排器、Caddy+sslip.io、App、webhook | ciorch #40、#41 | 通过 |
| 冒烟 | 11-21 16:36–22:50 | 沙箱 S1–S8 | ciorch #42–#44、#46、#47 | 8/8 通过，抓 4 个真 bug |
| 冒烟后修复 | 11-22 00:15–01:25 | 5 项切换前修复 + 小回归 | ciorch #48、#49 | 通过 |
| **nms 切换** | 11-22 07:23–08:34 | 配置切换、停原型、首个与第二个验证 run | — | 两次均 7 job 全 success |
| 切换后修复 | 11-22 | 读代码发现并修复同类隐患 | ciorch #50–#54 | 均 TDD、已部署 |
| 观察期 | 11-22 08:16 → 11-23 08:16 | 24h 脚本观察 | `~/.local/state/ciorch-observe/` | **进行中，见 §12.2** |

### 7.2 逐日详述

#### 11-13～14（nms 侧前史）

- 11-13：Actions 分钟耗尽，全仓停摆 → 决策 #145，七项 checks 改 `[self-hosted, ci]`；共享核两次击穿 `-race` 时敏用例，换 DO 专用核 c2-4vcpu-8gb（`gitrunner-sgp2`，$94/月）；同日踩 unattended-upgrades、runner 2.328.0 注册 404、daemon-reload 三个坑（runbook PR #591）。
- 11-14：P139 静默停摆（PR #598）；换机致覆盖率下移，基线 89.2→89.1（决策 #148，PR #599）。

#### 11-15（原型诞生，S-14a3）

- 04:47–04:57：维护者问 CI 状态，随即提出「一键拉起、用完删机」；选「全自动随 CI 队列起停」与「新 private 仓」；要求项目无关；ZCode 劝不 fork vpsctl，改子进程调用；计划获批。
- 05:14：cirunner 首提交 b1ba8bb，建私有仓。05:15–05:54 真机冒烟：第一台 22/ping 全不通 35 分钟未注册、删；第二台修 root 密码过期后 runner online；05:25 停旧 sgp2 runner 服务保证派发无歧义；05:54 七项 checks 全绿，test 17m41s，覆盖率棘轮通过（#148 口径未漂）。冒烟机约 30 分钟约 $0.07。
- 05:55–06:00：daemon 以 systemd 用户服务 + linger 常驻。06:25：全历史泄漏扫描后改为 public。
- 冒烟期修掉的四个坑：vpsctl 的 droplet `id` 是字符串；team2 与 team2-ci 同 token 须 `-only`；DO 镜像 root 密码过期；部分新 IP 的 22 被路径过滤（加 443 救援通道）。

#### 11-16（并行化，S-9997）

- 09:46/09:52：维护者抱怨串行慢。诊断为两层串行：job 级（只有 1 台在线 runner）与包级（`-race -p 1` 占 test 83%）。
- 10:35：复用闲置 sgp2（`systemctl start`，零新增成本）。11:57–13:07：nms #612（P142）、#613（`-p 4`，后改号 #152）、#615（P143）合并；13:22 main 全量绿，test 11m35s。
- 13:5x：`cirunner-team2-ci-sgp1-01` 消失。S-9997 推测是用户清理；按原型逻辑更像是正常空闲回收，**未查守护日志核实**【不确定】。

#### 11-17～18

- 11-17：vpsctl + accounts.json 部署到 `vpsctl-do-myteam-sgp1-01`（与 CI 间接相关，S-a171）。11-17 15:25 空闲删机、15:47 又建，多付一次冷启。
- 11-18 03:41:12 建机 → 03:43:29 首个 job 开跑：冷启实测约 2.3 分钟。

#### 11-19（立项，S-a68a）

- 16:13：用户要求「尽可能高速和尽可能的强大」，助手通读原型并对照日志，16:19 交出第一版（10 项问题、阶段 0–6、场景 A–H）。
- 16:34–16:52：用户提出便宜常驻编排器；确认服务所有项目；拍板新仓、cirunner 降为机器侧、不脱敏、用 DeepSeek；sgp2 可删。16:58 用户要求写详尽文档、换大模型去写代码。
- 17:01–17:29：把原型二进制固定到 `~/.local/bin/cirunner-proto`（commit 508a0b0，sha256 一致），systemd 改指向它（17:01 第一次 edit 失败，17:02 重试成功）；注销 runner 22 并 `vpsctl delete` 删 sgp2（id 618783288）；发现原型致命缺陷 #2。

#### 11-20（设计 → 阶段 2 → shadow）

- 01:52：REDESIGN.md 定稿 v1（915 行，当时在 cirunner 仓未提交）。
- 04:36：写码 session S1 开工。04:37–04:52 阶段 0a：TDD 修原型缺陷 #1/#2，构建 `.new`，审查通过后替换并重启；cirunner PR #1 合并（bf0c615）。04:52–05:28：nms #622、#623。
- 05:48–05:54：核对原型热补丁后首个完整建删循环（04:57 建机 → 05:48 空闲删 → 05:50 清残留）；发现 quotepath bug 与 #150 撞号。05:57–07:34：nms #624、#625、#626。
- 07:34：建 `ciorch`（公开）与 `ciorch-sandbox`（私有，07:35:15Z）。阶段 1：`ciorch stats` 对 nms 近 7 天（92 run / 644 job）出基线。08:35 打回 4 处，ciorch #2 修复。
- 08:44–09:20：阶段 2 四个 PR。**09:27 不通过**，10 条必修（假 dry-run、观察不完整误删误建、场景 D 口径错、B/C 不幂等、半开多建、Drain 失败后不回收、并发写 map、预算配额没接、残留注册、先建后拆不看需求），并指出自查清单「凭印象打勾」。10:14–10:17 复审：修正 #7–#10，仍有「修一半」的浅拷贝竞态；放行 shadow（限定条件）。
- 10:25：`ciorch-shadow` 启动（PR #11 之后）；10:25:27 一次启动失败（systemd 环境无 `gh`），7 秒后改 token 文件恢复。
- 10:30、11:13、11:58：手动触发 3 次 nms CI 作对比样本（run 44719916539 / 44725882488 / 44731941101）。10:33–10:38：shadow 抓到 runners labels 是对象数组的 bug（读 runner 列表连续 16 轮失败，**整轮冻结按设计生效，零动作、没误建**），PR #12 修复。
- 11:47：用户问能否迁到自己的高性能机器 → 提供商抽象的起点。

#### 11-21（抽象 → 机器侧 → 部署 → 冒烟）

- 10:25–10:38：shadow 满 24h；V3 结论被纠正（api-usage 数据推不出 304 是否计额度）；实测 304 不计入（PR #14）；shadow 报告通过后停 shadow。
- 10:45–10:46：分工调整：抽象交新 session S2，S1 只做阶段 3 + 三仓 MIT。10:49：用户要求协调者每小时监听。
- 10:54–11:22：S2 完成 PR ⓪–③（#16–#20，③ 于 `5d6a190` 合并）；11:24–11:38：#21–#23；11:38–13:01 shadow 83 分钟 134 轮全部 complete=true。
- 10:53–12:05：S1 cirunner #3→v0.1.0（11:00:38Z）；沙箱真机验证（`mc-team2-ci-sgp1-01`）修出 #4、#5（v0.1.1 11:08:48Z、v0.1.2 11:11:25Z）；11:06 误用 `git add -A` 把 REDESIGN.md 推到公开仓远端分支约 1 分钟，立即删除重建，判定不构成泄漏；11:59 阶段 3 通过；#6 → v0.1.3（12:05:14Z）。12:15–12:42 V1（422）、V6（STOP 表现为 offline）实测。
- 13:03–14:09：阶段 4 代码（ciorch #26、#27、#29、#31、#32、#34）与阶段 5 事故管线（#28、#30、#33、#35–#37）并行；14:00 审查抓出 3 处必修，S2 以 #39 修复；S1 做冒烟准备（#38）。
- 15:37：用户「同意建机」。15:38–15:39：SendToSession 对 S1、S2 均报 sessionNotFound，协调者新开部署 session（D）。15:43：用户找不到私钥入口，协调者承认步骤过时，更正为 Credentials → Key pairs → New key。15:47：App 就绪（只装沙箱）。
- 15:41–16:01：建编排器机器，前 3 台废机（PAM 密码过期 ×2、坏 IP ×1），第 4 台 `ciorch-team2-ci-sgp1-01` 成功（约 $0.02 废机费）。16:03–16:05 装机、Caddy 证书（V5）；16:12 `enable --now`；16:18:50 webhook ping 200；16:22 部署审查通过、DeepSeek key 落机、V4 实测。
- 16:36–22:50：S1–S8（顺序 S1→S7→S2→S3→S6→S4→S5→S8）。途中修 #42（公钥换行）、#43（同轮撞名）、#44（tuning 死配置）、#47（LastActivity 跨轮不去重）。22:36–22:43 S8 暴露 healthcheck 空转；22:47 冒烟报告交回，花费 $0.166。

#### 11-22（回归 → 切换 → 修隐患）

- 00:15：冒烟通过，提出 5 项切换前修复；00:27/00:30 合并 #48、#49 并部署二进制 `a67b45f6…`；00:30–01:24 小回归（S4 只开 1 个 issue、healthcheck 真探测成功）。
- 02:03：协调者向用户提出切换方案，等用户把 App 加装到 nms。07:12：用户「好啦，你来安排吧~」，协调者核实 App 已装 nms。
- **07:14**：原协调 session（sess_a68a5090…）向冒烟 session 派发切换任务时 SendToSession 被拒（proto.sessionNotFound）；其后每次模型调用 finishReason=content-filter、仅输出 1 token（上下文约 78.5 万 token，含明文 DeepSeek key 与大量 ssh/删机/读密钥命令，过滤器不给原因）。该 session 已归档，新协调 session 接手；向冒烟 session 转发切换指令仍被 sessionNotFound 拒收，协调者改为亲自执行切换。
- **07:23**：备份 `/etc/ciorch/ciorch.json` → `ciorch.json.bak-pre-nms`；新增 pool `do-sgp1-c2`（vpsctl / team2-ci / sgp1 / c2-4vcpu-8gb / ubuntu-24-04-x64 / `hourly_cost_hint` 0.1399 = 94/672 / `max_machines` 2）、profile `go-heavy`（pools=[do-sgp1-c2]，labels=[ci]，runners_per_droplet 1）、repos 加 `hoshinojian/nms`（max_runners 2，issue_repo 空）；`global.max_machines` 2→3、`daily_budget_usd` 3→8。候选文件先用 `ciorch plan` 校验（nms complete=true、Q=0、无动作）再替换。
- **07:23:34** 再核实（nms 无活动 run、原型名下 0 机、nms runner 注册 0）→ `systemctl --user disable --now cirunner`（只停不删：`~/.local/bin/cirunner-proto`、`~/.config/systemd/user/cirunner.service`、`~/cirunner` 仓保留为回滚路径）。**07:24:27** 重启 ciorch，日志出现 nms 轮询 complete=true，`/healthz` 200。
- **首个 nms CI（run 44998142282，workflow_dispatch，main）**：07:25:08 创建；07:25:14 webhook `workflow_job queued` 送达（App 投递记录 200）；07:25:22/26 建机 `ci-nms-team2-ci-sgp1-01/-02`（run 创建后 14s、webhook 送达后 8s 决策）；07:27:31 首 job 开跑（建机后 2m09s；run 创建后 2m23s）；test 在 -02（07:27:50–07:39:33），其余 6 个轻 job 串行在 -01；07:39:34 完成，7 job 全 success，覆盖率棘轮 89.3% ≥ 基线 89.1%；总时长 14m26s（原型时代约 23 分钟）；机器数全程 ≤2；空闲回收 07:52（-01）、07:59（-02）一轮一台，之后 droplets=0、nms runner 注册=0；花费约 $0.15。
- **07:30**：沙箱误开 issue #8「label 不匹配：ci,self-hosted」（2 分钟后自愈关闭）。根因即 §5.2 的 JobCache 跨仓污染，同时发现该 map 无锁的崩进程风险。修复 PR #50（变异测试证明两条测试各自能抓住对应 bug）。
- **08:16**：部署 #50（二进制 `a67b45f`→`e40767506aee`）；08:17 第二次验证 run 45002087281：7 job 全 success，08:17:09→08:34:26（17m17s，test 13m），sandbox 零误报事故。
- 随后读代码做架构梳理，发现并修复同类隐患，均 TDD（先红后绿）、CI 全绿后合并、部署到编排器：#51（githubapp ETag 缓存无锁，部署二进制 `5761d103e977`）；#52（`plan`/`status`/`daemon -dry-run` 时事故模块未置 DryRun）与 #53（`cirunner diag` 在 health 不 OK 时 exit 1，ciorch 丢弃输出）一并部署为 `d5ace1280b26`；#54（`ciorch stats` 不走 App 认证）部署为 `f652ebce1b48`，编排器上 stats 实测正常。旧二进制按 `/usr/local/bin/ciorch.bak-<sha12>` 保留（`a67b45f677b4`、`e40767506aee`、`5761d103e977`、`d5ace1280b26`）。
- 08:16 起：24 小时观察期，WSL 脚本 `~/.local/state/ciorch-observe/observe.sh` 每 30 分钟记录（详见 §12.2）。

### 7.3 PR 范围一览（ciorch）

| 范围 | PR |
|---|---|
| 阶段 1（stats） | 首提交 `1d72f0a`（直推空仓 main，审查认可为例外）、#1、#2 |
| 阶段 2（决策核心 + 复审） | #3 config+observe；#4 decide；#5 budget/vpsctlapi/state/engine；#6 daemon+githubapp 健壮性；#7–#10 复审修正；#11 深拷贝/预算持久化等 |
| shadow 回填 | #12 labels 对象；#13 小时用量；#14 V3 结论 |
| LICENSE | #15 |
| Provider 抽象 | #16 计划文档；#17 machine；#18 provider/vpsctl；#19 golden 夹具；#20 切换（关键路径）；#21 池化配置；#22 文档同步；#23 blank import 注册；#28 收尾 |
| 阶段 3 衔接 | #24 V1/V6 回写；#25 渲染器接入；#26 go.mod tidy 门禁 |
| 阶段 4（上云） | #27 App 认证；#29 server；#31 deploy/；#32 healthcheck；#34 runbook |
| 阶段 5（事故管线） | #30 llm；#33 issue API；#35 incident 状态机；#36 诊断轮；#37 接线；#39 审查修复 |
| 冒烟准备与部署回填 | #38 smoke.md；#40 runbook 实测回写；#41 V4 |
| 冒烟中修复 | #42 公钥 trim；#43 同轮撞名；#44 tuning；#46 smoke 回填；#47 LastActivity；#48 熔断单发/动作目标/诊断材料；#49 healthcheck 红叉+label |
| 切换后 | #50 JobCache 按仓+锁；#51 ETag 锁；#52 dry-run 只读；#53 diag 退出码；#54 stats App 认证 |

说明：编号 #45 是 ciorch 仓的 S8 issue（healthcheck 失败），不是 PR。cirunner 与 nms 的 PR 见 §13.2。

## 8. 验证体系与实测结论

### 8.1 验证分层总览

| 层 | 手段 | 抓到什么 | 局限 |
|---|---|---|---|
| 单元测试 | 纯逻辑包 TDD：先写失败测试再实现；表驱动；ciorch 30 个测试文件、219 个 `Test*`（2026-11-22 main@250135c 实数：decide 46、incident 31、engine 28、githubapp 22、provider 13、budget 10、config 10、observe 10、stats 9、server 8、cmd 8、daemon 7、machine 7、llm 5、state 5） | 决策逻辑、状态机、校验 | 假数据与真实形态不一致时抓不到（labels 对象、公钥尾换行、webhook secret 尾换行、LastActivity 跨轮） |
| fake / 注入 | GH / VPS / SSH 用记录调用的 fake；`machinetest.Fake` 内存 Provider；vpsctl 实现注入假子进程；GitHub / DeepSeek 用 httptest 假服务器（JWT 现场生成 RSA 密钥并验签） | 零写保证（`TestDaemonDryRunZeroWrites`）、动作顺序、失败隔离 | fake 与真实契约可能漂移（曾因 fake 不按 tag 过滤、list 看不到新机而返工） |
| 契约测试套件 | `machinetest.Run`：Fake 与 vpsctl 实现跑同一套（建机后按标记可列且名字原样、无标记不出现、删后列不出、重复删返回 nil、部分失败必须报错、`-race` 并发安全） | Provider 抽象的真实契约 | — |
| golden 回放 | `decide/golden_test.go` + `testdata/golden/*.json`，9 个场景多轮链式：scale-up、idle-teardown、stall-a、coldstart-b-breaker、offline-c、job-stuck-d、frozen-h、unmatched-e、external-g；重新生成 `go test ./internal/decide -run TestGoldenReplay -update` | 证明抽象重构「行为零变化」（#19 在 ③ 之前单独合入，#20 之后一字未改通过） | 对**任何**决策语义变化敏感，#37/#47/#48 这类修正需同步重生成，审查时必须核对差异只是新增字段 |
| `-race` | `go test -race -count=1 ./...` 是每个 PR 合并前的本地门禁 | 并发写 map 等 | 测试夹具里没有机器 / 只有单仓时抓不到（阶段 2 的「修一半」） |
| 与旧实现逐字节对照 | vpsctl Namer / Pattern / create argv 对旧实现做活体交叉测试；③ 删旧实现后保留字面期望 | 抽象无漂移 | — |
| 真实环境只读 | `ciorch plan` / `status` 对 nms 跑端到端；抽象前后 plan 输出逐字节对比 | 配置、认证、vpsctl list 三件事 | — |
| shadow | `daemon -dry-run` 与在役原型并跑（11-20 起 24h；抽象后 83 分钟 134 轮） | 需求识别、反应时间、API 健壮性、真实形态的解析 bug | 看不到原型建的机器（标签/命名空间不同），机器生命周期只能靠沙箱 |
| 沙箱真机 | 私有沙箱仓、1–2 台机器、专用前缀/标签、验完立刻删并复查 | 机器生命周期、故障注入、事故管线 | 花钱（几美分到几毛钱）；只手动触发 |
| 门禁 | ciorch CI（托管 ubuntu-latest）：lint（gofmt、vet、`go mod tidy` 无漂移、**import 边界检查**）、test（`-race`）、build（`CGO_ENABLED=0`）；cirunner 自 #6 起有 CI，release 先测后发 | 依赖漂移、抽象被绕过 | — |
| 切换验证 | 首个 / 第二个 nms 全量 run | 端到端、覆盖率棘轮、成本 | 样本只有两个 run |

**核心教训**：**CI 结论必须看日志**，checks 全绿不等于行为符合预期（nms #622「走了 docs-only」一例）。

### 8.2 shadow 24h 结论（2026-11-20 10:25 → 11-21 10:25）

- **需求识别**：shadow 一开始 Q=7（job 粒度 + label 匹配），原型按 run 计数，Q 恒为 1。
- **反应时间**（比**决策时刻**，不比日志时刻——原型 `-wait` 阻塞返回后才打日志）：

| | run 1 | run 2 | run 3 |
|---|---|---|---|
| 触发 (UTC) | 10:30 | 11:13 | 11:58 |
| shadow 从看到排队到决定建机 | 2 秒 | 13 秒 | 8 秒 |
| 原型机器上首 job 开跑（排队后） | 2m43s | 3m01s | 3m24s |
| 结论 | 失败 | 通过 | 通过 |

  run id：44719916539、44725882488、44731941101。run 1 失败是 nms 的 `TestReconcilerExhaustionFinalRaise` 超时（`agent_deploys=2`，pitfalls 已有记录），run 2、3 同一份代码通过，与编排器无关；协调者要求不追查、不改 nms。
- **自我纠正**：原先预估「shadow 比原型早约 45 秒」，实测**冷启时没有这个优势**（两者空闲档都是每 60 秒看一次，shadow +2/+13/+8 秒只是轮询相位运气）；真正的差别在活跃后（shadow 约 19s 一轮，原型 60s）。要消除这段等待靠 webhook（阶段 4；切换首跑实测 run 创建→建机 14 秒、webhook 送达→建机 8 秒），要压缩大头（建机+装环境，约 2m40s～3m20s）靠阶段 7 快照。
- **健壮性**：自 11-20 10:44:43 最后一次重启起不重启、无 panic；稳态约 113 次 GitHub 请求/小时，约一半是 304；NRestarts=0。原型 11-20 23:59:59 一次 gh 调用超时，下一轮自愈。
- **labels 对象 bug**：原型建的机器 10:33 注册后，ciorch 读 runner 列表连续 16 轮失败，整轮冻结生效，零动作、没误建；第 10 轮按设计报「观察失败」事故；10:38 修复重启后恢复。
- **盲区**：shadow 的 N 恒为 0，空闲回收与 A/B/C/D 机器生命周期逻辑这次验证不了。
- **V3 纠正**：写码 session 先根据 api-usage 推断「304 不占额度」，被指出这组数据推不出（额度按用户算，shadow、原型、手动 gh 共用 5000 次/小时，且整点重置）。教训：不要用被他人污染的计数器下结论。

### 8.3 V1–V8 待实测项（REDESIGN §11）

| # | 问题 | 结论 | 出处 |
|---|---|---|---|
| V1 | `DELETE /actions/runners/{id}` 对忙碌 runner 是否拒绝 | **拒绝（422）**，空闲时 204。Drain 的「注销失败则放弃删机」防竞态成立。**切换 nms 前的硬门槛**，11-21 沙箱实测（cirunner v0.1.3 后，机器 mc-verify，约 $0.08） | cirunner 阶段 3 验收；ciorch #24 |
| V2 | `vpsctl create -image <数字快照id>` 能否建机 | **未测**（属阶段 7 快照） | — |
| V3 | ETag 304 是否计入 rate limit | **不计入**。同 token 先 `GET /rate_limit` 记 `core.used`，同 URL 带 `If-None-Match` 连发 20 次全 304，再查 used 仅涨 1～3（3 组最小 Δ1），若计费应涨约 21 | ciorch #14 |
| V4 | DeepSeek JSON 模式约束 | **可用**。编排器上最小请求返回合法 JSON；`deepseek-chat` 现指向 `deepseek-flash`，config 维持 `deepseek-chat` | ciorch #41 |
| V5 | Caddy + sslip.io 能否签证书 | **顺利**。`168-144-104-79.sslip.io` HTTP-01 一次成功，TLSv1.3，无需自有域名 | ciorch #40 |
| V6 | `kill -STOP` 能否复现 P139 假活 | **复现的是场景 C（离线）不是场景 A**：STOP 后数分钟内转 `offline busy=false`，`kill -CONT` 后 ≤16s 恢复。P139 的 online 假活是另一种形态（Listener 在跑但轮询卡死）。A 阈值不变。冒烟 S3 中实测为 offline **busy=true**（与 V6 记录的 busy=false 不同，场景 C 判据不看 busy，仍落入 C） | ciorch #24；S3 |
| V7 | `workflow_job` webhook 排队到送达的延迟 | **REDESIGN 无结论**；切换首跑有一个样本：07:25:08 run 创建 → 07:25:14 送达（约 6 秒，n=1） | 切换验证 |
| V8 | WSL 出站到 DO 新机 22 的可达性 | **多数 IP 可达；个别新 IP 全 TCP 被丢**（4 个新 sgp1 IP 中 1 个，ICMP 通，Windows ssh.exe 也不通，power cycle 无效），删机重建换 IP。结论：22 直连可用，无需高位端口兜底；这修正了 nms P3「22 端口选择性阻断」的旧说法 | ciorch #40 |

说明：用户口头提过「V1～V6」，现行文档有 V1～V8，V7/V8 是后来补的。

### 8.4 沙箱冒烟 S1–S8（2026-11-21，约 6.2 小时，花费 $0.166）

执行顺序 S1 → S7（嵌在 S1 中段）→ S2 → S3 → S6 → S4 → S5 → S8。每条开始前确认 `/status` N=0、沙箱 runner 注册为 0、无 queued run。

| # | 目的 | 结果 | 发现的 bug / 偏差 | 修复 PR |
|---|---|---|---|---|
| S1 正常全流程 | 秒级判定、建满 max_runners、冷启约 2.5m、空闲回收一轮一台 | 通过（过程有波折）。16:37:01 `Q=8 actions=[create,create]`；第二次 dispatch run 8/8 success；17:44:48/17:46:11 回收，隔一轮 | ① 公钥文件尾换行被 cirunner 安全校验当危险字符，**建机全败**，3 次连败升级事故 I 开 issue #1（顺带实测场景 I）；② 同轮建 2 台**同名**（id 622006980/622006988），GitHub 拒绝重名注册，第二台成**僵尸机**继续计费，MachineState 以名为键被合并，场景 B 看不见；③ `global.tuning` 是死配置（无消费方）；④ 回收阈值实测 45m 而非 20m（LastActivity 每轮追加点，8 job 记 8 点） | ① #42；② #43；③ #44；④ #47 |
| S7 编排器重启 | 重启后从 state.json 恢复，不误删不重建 | 通过。重启前后 `/status` 完全一致（Q=5 I=0 P=0 N=2 Machines:2 breaker closed），首轮 `actions=[无]`；冒烟期间换二进制 6 次以上均无损 | — | — |
| S2 掉线（场景 C） | stop runner 服务，约 2 轮判掉线，替换加强删 | 核心自愈通过。17:54:01 停 -01，17:55:19 判 C 并强删（78s），配额满故降级「先拆后建」，17:55:43 建 -03；剩余排队 job 被接走 | **偏差**：smoke.md 原写「run 最终 success」不对——GitHub 不会把被删 runner 上正在跑的 job 重排，test job failure、run 整体 failure，这是设计边界不是缺陷 | #46 改正文档 |
| S3 假活（STOP Listener） | 验证 V6 | 通过，落入场景 C（印证 V6）。同仓 24h 内第 2 次掉线升级开 issue #2，**首次真实调用 DeepSeek**，选 replace_droplet，建出 -03 | LLM 要替换的 -01 在出结论前已被固定逻辑强删，校验用滞后一轮的观察快照所以放行；残留注册因忙碌 422 重试约 4 轮后自清（再次印证 V1） | #48 |
| S6 label 无人满足（场景 E） | 不建机，5 分钟后开 E 事故 | 通过。Q=0 不建机，20:04:05 开 issue #3；取消 run 遇网络抖动晚了，编排器 20:08:33 按设计重开 #3 | 操作注意：要**先取消 run，再关 issue** | — |
| S4 冷启失败 → 熔断 | sha256 置零，B 强删 + Fails++，熔断 open，开 issue + LLM，人工关 issue 复位 | 通过。每台 8m11s 超时判 B；20:26:39 熔断打开；恢复配置后熔断仍 open（状态恢复正确）；20:31:25/34 关 issue 后 closed | **同一次熔断开了两个 issue（#4/#5）**，按机器主体各一个；两台同轮失败，Fails 2→4 跳，实际第 2 轮就开 | #48 |
| S5 job 卡死（场景 D） | 阈值调 3m/5m | 通过。21:44:06 开 #6，21:46:02 强删，21:46:21 自动关；long-sleep 在 GitHub 侧 failure，其余 7 个 success | LLM 判「误报」建议 none，但固定逻辑先于事故管线、仍按 5m 击杀，符合设计 | — |
| S8 编排器宕机外检 | 停 ciorch，healthcheck 开 `ciorch-down` issue | 补齐 2 个缺口后通过，开出 ciorch#45 并 @维护者，恢复后关闭 | ① 部署漏配仓库变量 `CIORCH_HOST`，healthcheck 静默跳过却显示 success，**宕机报警从部署起一直空转**；② 脚本要打 `ciorch-down` 标签但没先创建，开 issue 失败 | #49 |

**四个真 bug**：#42、#43、#44、#47（均为单测假数据不够真实才漏）。另有部署漏配 `CIORCH_HOST` 与 healthcheck 缺标签两个部署类缺口。收尾：`vpsctl list -tag ciorch` 0 台，沙箱 runner 注册 0，配置与初始一致，测试 issue 全部关闭。

沙箱 issue 全量：#1 建机报错（S1）、#2 掉线（S3）、#3 label 不匹配（S6）、#4/#5 冷启失败（S4，双开 bug）、#6 job 卡死（S5）、#7 冷启失败 breaker（回归）、#8 label 不匹配 ci,self-hosted（07:30，**非冒烟，是切换后 JobCache 跨仓污染的误报**，PR #50 修）。

### 8.5 冒烟后 5 项修复与小回归（2026-11-22 00:15–01:25）

| 审查项 | 修复 | PR |
|---|---|---|
| 1 健康外检不能静默失效 | 缺 `vars.CIORCH_HOST` 红叉；开 issue 前先 `gh label create ciorch-down \|\| true`；runbook 写明必配并要求手动 dispatch 验证 | #49 |
| 2 一次熔断只开一个 issue | `scenarioB` 只在转入 open 瞬间发事故，主体改仓级 `breaker` | #48 |
| 3 LLM 动作目标须是现存且未 Bad 的本仓机器 | `actionableMachines`，取内存最新状态，不用滞后一轮的观察快照 | #48 |
| 4 诊断材料补齐 | `RecentProviderErrors`（含渲染前置错误）→ `recent_vpsctl_errors`；E 场景加 `unmatched_queued` | #48 |
| 5 文档纠正 | smoke.md S1：单 run 按设计走 20m | #49 |

**小回归**（只重跑 S4 + 手动 healthcheck；临时 `idle_timeout`/`idle_timeout_busy` = 5m 提速）：00:48:26 熔断 open，**只开 1 个 issue #7** ✓；00:53:20 关 issue 后 breaker closed ✓；healthcheck run 44971315057（01:24:27）success 且日志里真有 curl 的 `ok` ✓。插曲：关 issue 时 sha 还没还原，立即触发的建机用的仍是坏 sha，2 台 8m 超龄被强删一轮，多花约 $0.006 与 8 分钟。**教训：先还原配置，再关 issue。** 回归花费 $0.025；总计不到 $0.25。

### 8.6 DeepSeek 诊断表现

| issue | 场景 | 诊断要点 | 评价 | 原因 |
|---|---|---|---|---|
| #1 | I（建机连续报错） | 判为「vpsctl/DO 调用异常」，让人查余额/配额/token；动作暂停建机 30 分钟（置信度 0.72） | **错** | 真因是 ciorch 自己的公钥换行 bug（发生在调 DO 之前）；材料里**没有建机错误原文** |
| #2 | C（同仓第 2 次掉线） | 判为「机器失联（SSH 255），非 P139」，`replace_droplet` -01，实际建出 -03 | **部分错** | 机器是被 STOP 了 Listener、本身可达；SSH 255 很可能是因为 -01 诊断前已被强删（推断）；动作指向已删除的机器 |
| #3 | E（label 不匹配） | 结论正确：配置错误，人工检查 `runs-on`，动作 none；但把「Q=0」与「排队超 5m」当矛盾、称「误报」 | **结论对，推理有瑕疵** | `UnmatchedQueued` 不在证据里（#48 补） |
| #4/#5 | B（熔断） | 证据读取准确，指向 cloud-init / cirunner 安装，未精确到 sha256 校验失败；动作 none，给 clear_breaker 指引 | **方向对，不够精确** | 冷启失败发生在机器上，编排器 SSH 不上（255），拿不到 cloud-init 日志 |
| #6 | D（job 卡死） | 读到机器侧 worker 日志 `Processing step: ... sleep 7200`，判「误报」，建议 none | **准确，质量高** | 机器可达，`machine_diag` 拿到完整 runner/worker 日志 |
| #7（回归） | B（仓级熔断） | 机器超龄 8m（基线 2.5m）、`runners: null`、SSH 255、`create_fail_streak: 0`（据此排除建机 API 层，判失败在引导/注册阶段） | **方向对** | `recent_vpsctl_errors` 为空是如实的：sha 注入不属于 provider 错误 |

**结论**：诊断质量取决于证据面。材料缺 provider 错误原文与 UnmatchedQueued 时会误判（#1、#3）；观察滞后时动作会指向已删除的机器（#2）；机器侧日志拿得到时非常准（#6）。#48 补的就是这三个缺口。

**遗留不确定**：S4 与回归中 runner 机器的 SSH 都是 255。sha 错误按理只让 agent 下载校验失败（cloud-init exit 1），编排器却也 ssh 不上；可能 cloud-init 在校验失败后没执行到写入编排器公钥或 sshd 相关步骤。**没有查证**【不确定】。

### 8.7 切换验证（2026-11-22）

| 项 | 首跑 run 44998142282 | 第二次 run 45002087281 |
|---|---|---|
| 触发 | 07:25:08（workflow_dispatch，main） | 08:17:09 |
| webhook 送达 | 07:25:14（App 投递 200） | — |
| 建机决策 | 07:25:22/26（约 14s 内），建 `-01/-02` | — |
| 首 job 开跑 | 07:27:31（建机后 2m09s；run 创建后 2m23s） | — |
| 结果 | 7 job 全 success；test 在 -02（07:27:50–07:39:33）；其余 6 轻 job 串行在 -01；覆盖率 89.3% ≥ 89.1% | 7 job 全 success；test 约 13m |
| 完成 | 07:39:34，总 14m26s | 08:34:26，总 17m17s |
| 机器数 | 全程 ≤2 | — |
| 空闲回收 | 07:52（-01）、07:59（-02）一轮一台，之后 droplets=0、runner 注册=0 | — |
| 其他 | 花费约 $0.15 | sandbox 零误报事故（#50 已部署） |

对比原型时代约 23 分钟（`-p 4` 之前更久）：首跑提速约 38%。**样本仅两个 run，两次耗时差（14m26s vs 17m17s）来源未拆解**【不确定】（test 11–13m 的波动、冷启与 job 分配均可能贡献）。

### 8.8 切换后 #50–#54 的 TDD 与部署

| PR | 问题 | 测试 | 部署二进制 |
|---|---|---|---|
| #50 | JobCache 全局一份且无锁 | `TestWebhookCacheScopedToRepo`、`TestJobCacheConcurrentAccess`；变异测试证明各自能抓对应 bug | `e40767506aee`（08:16） |
| #51 | `githubapp.Client` ETag 缓存 map 无锁，daemon 每仓 goroutine 共用 Client | 先红后绿；加 `etagMu` | `5761d103e977` |
| #52 | `plan`/`status`/`daemon -dry-run` 事故模块未置 DryRun | 先红后绿；wiring 增加 readOnly | `d5ace1280b26`（与 #53 同批） |
| #53 | `cirunner diag` exit 1 但 stdout 为完整 JSON，ciorch 丢弃输出 | 先红后绿；exit 1 且 JSON 合法时保留 | 同上 |
| #54 | `ciorch stats` 只走 gh 兜底，编排器无 gh 失败 | 抽出 ghClient；编排器上实测通过 | `f652ebce1b48` |

均 CI 全绿后合并。

### 8.9 真实 PR 场景验证（2026-11-22 13:45–16:03，代替被动等待）

维护者判断"等协作者自然推 PR"等不及，要求另开 session 造测试（`sess_4a544768…`，报告 `/tmp/ciorch-citest-report.md`，协调者已用 gh/vpsctl 独立复核 PR 状态、run 结论、机器与注册清零、无事故 issue、main 未动）。5 个 draft 测试 PR（nms #627–#631，均已关闭未合并、分支已删），7 个 run / 8 次 attempt：

| 场景 | run | 结果 | 要点 |
|---|---|---|---|
| S1 docs-only（#143） | 45029806937 | success，5m43s | test 日志 `docs_only=true`，test 1m47s；run 创建→建机 15–20s，→首 job 2m25s；13:53:23 一次「场景 F 全局配额 3/3」瞬态（2 台实机 + 1 个未过 TTL 的占坑），下一轮自愈 |
| S2 代码 PR 全量 | 45030705550 | attempt 1 test 失败（P145 偶发），`rerun --failed` 后 success | S1 的热机 3 秒接活，零建机等待；覆盖率 artifact 已上传 |
| S3 并发 2 PR + 第 3 个 | 45033783948 / 45033828909 / 45033977076 | b 失败（同一 P145 偶发）、c success、d 排队 | 21 个 job 在场时机器恒为 2 台，未建第 3 台 |
| S4 同分支连推 | 45033977076→45035675125 | 旧 run cancelled，新 run success | 取消前后日志 `actions=[无]`，无 Bad 判定、无误删 |
| S5 空闲回收 | — | 15:45:59 回收 -01、15:55:43 回收 -02 | 近 2h 不同 RunID ≥2 → 45 分钟档；一轮一台；之后 droplets=0、runner 注册=0 |
| S6 事故 issue | — | 两仓零新增 | `gh issue list --author 'ciorch[bot]'` |

机器峰值 2（≤ max_runners 2、≤ 全局 3）；Breaker 全程 closed；本次增量花费约 $0.55（当日累计 $0.98）。两次失败都是 nms 测试自身的偶发（同 run 其余 6 job 绿、机器正常），已记为 nms pitfalls **P145**：夹具时间写死 2026-11-11 + migration 0022 的 24h retention，bgw 在插入与查询之间删 chunk。

测试方另报两点环境现象：WSL 出网间歇大面积丢包（gh 超时，经编排器 SOCKS 隧道绕过；编排器自身不受影响）；从 WSL 访问外部 healthz 偶发超时，协调者 16:07 复测为 200。

## 9. 踩坑大全

> 格式：现象 → 根因 → 解法 → 出处。分四类：产品代码、部署运维、GitHub/DO 平台、多 session 协作与工具。nms 侧通用 CI 坑见 nms `docs/engineering/pitfalls.md`（动手前先查，别踩已归档的坑）。

### 9.1 产品代码

| # | 现象 | 根因 | 解法 | 出处 |
|---|---|---|---|---|
| 1 | 原型 job 永久卡死、不再建机 | 排队信号只数 queued run，in_progress run 里仍在排队的 job 不可见；runner 挂掉被当空闲删 | 排队信号 = queued run 数 + in_progress run 内 queued job 数；ciorch 全面改 job 粒度 | cirunner #1；REDESIGN §1.4 #2 |
| 2 | 原型冷启失败后永久停建 | 计数只在出现活跃 runner 时清零，停建后不会再出现 | 冷却 30m 试探；零值时刻视为允许一次试探（否则已卡死的现场永远不自愈）；ciorch 换熔断器 | cirunner #1；REDESIGN §3.6.5 |
| 3 | ciorch 读 runner 列表连续 16 轮失败 | GitHub runners API 的 labels 是 `{id,name}` 对象数组；零 runner 时空数组侥幸通过；单测夹具也按字符串写，一直绿 | `[]RunnerLabel` + 真实形态回归测试 | ciorch #12 |
| 4 | 建机全部失败（渲染 cloud-init 报「公钥含引号/换行等危险字符」） | `.pub` 文件读出带尾换行，引擎没 trim；cirunner 的校验是对的；单测夹具公钥无尾换行 | 按行拆分 + TrimSpace + 跳过空行 | ciorch #42 |
| 5 | 同轮建 2 台同名，第二台注册被 GitHub 拒绝，成了不被管理的僵尸机继续计费 | `execute()` 同一轮多个 ActCreate 基于同一份 obs 快照取名；MachineState 以名为键，两台合并成一条，场景 B 看不见 | execute 累积本轮已建名传给取名；单台替换路径传 nil | ciorch #43 |
| 6 | `global.tuning` 配了不生效 | 配置里定义了，全仓没有消费方 | 新增 `decide.ApplyTuning`，15 个决策键严格校验，未知键/坏值首轮即报错 | ciorch #44 |
| 7 | 空闲回收一律等 45 分钟（冒烟 S1/S2 各干等约 45 分钟） | `recordActivity` 的 `byRun` 只在单轮 obs.Jobs 内按 RunID 去重，job 错峰开跑每轮追加一个点，一个 8 job 的 run 记 8 点 | `RepoState.RecordedRuns`，run 首见记一次、之后不改 | ciorch #47 |
| 8 | webhook 部署后所有请求 401（评审阶段抓出，未上线） | `openssl rand -hex 32 >` 生成的文件末尾带 `\n`，cmd 读 secret 没 TrimSpace | TrimSpace + 去空白后 <16 字节拒绝启动 | ciorch #39 |
| 9 | 人工关 issue 后熔断不清（评审抓出） | `OnIssueClosed` 与 `HumanResolved` 都有，cmd 没接线 | cmd 接上 + 端到端测试 | ciorch #39 |
| 10 | 编排器自己恢复后关 issue，熔断/退避/pause 被误清 | 自关 issue → GitHub 回发 `issues.closed` → 被当人工关闭 | sender 为 `<app_slug>[bot]` 则忽略 + 只处理非 resolved 事故 | ciorch #39 |
| 11 | 熔断一次开两个 issue | B 场景按机器主体各发一个事故 | 主体改仓级 `breaker`，只在转入 open 瞬间发 | ciorch #48 |
| 12 | LLM 的 `replace_droplet` 目标是已被强删的机器仍被放行 | 校验用滞后一轮的 lastObs | `actionableMachines` 取内存最新状态，排除 Bad / replacing / draining | ciorch #48 |
| 13 | DeepSeek 把 ciorch 自身 bug 判成 DO 侧问题 | 诊断材料缺建机错误原文 | `recent_vpsctl_errors`（含渲染前置错误）、`unmatched_queued` | ciorch #48 |
| 14 | 多仓并发崩进程风险（`fatal error: concurrent map writes`） | 阶段 2：`Reconcile` 里拷贝的是浅结构体，Machines map 与切片仍共享；测试夹具没机器所以 `-race` 抓不到。切换后：JobCache 与 ETag 缓存各一个无锁 map | Decide 入口深拷贝 + 不改入参测试 + 有机器的两仓 ×200 轮 `-race`；JobCache 加锁；ETag 加 `etagMu` | ciorch #11、#50、#51 |
| 15 | 多仓托管后 sandbox 误开 label 不匹配事故 #8 | JobCache 全局一份，合并时不按仓过滤 | `JobCacheEntry.Repo` + `Snapshot` 按仓过滤 | ciorch #50 |
| 16 | `plan`/`status`/`daemon -dry-run` 会真开 issue、真调 LLM | 事故模块未置 DryRun，engine dry-run 分支仍 `fireIncident` | wiring 增加 readOnly | ciorch #52 |
| 17 | 机器不健康时事故材料只剩 `exit status 1` | `cirunner diag` 在 health 不 OK 时 exit 1 但 stdout 是完整 JSON；ssh 封装遇非零退出就丢输出 | exit 1 且输出为合法 JSON 时保留 | ciorch #53 |
| 18 | 编排器上 `ciorch stats` 直接失败 | stats 只走 `gh auth token` 兜底，编排器无 gh；App 与 `token_file` 互斥 | 抽出 ghClient，stats 与 daemon 一样优先 App 认证 | ciorch #54 |
| 19 | 阶段 1 冷启代理样本只有 2 个 | 名字复用（`cirunner-team2-ci-sgp1-01` 下 14 个不同 runner_id） | 按 runner_id 判新机器 | ciorch #1 |
| 20 | 排队等待出现负值；重跑 run 总时长虚高 | `filter=latest` 下未重跑的 job 保留旧 attempt 的 started_at，created_at 却刷新；run.created_at 起算含两次 attempt 间隔 | 排除 started_at<created_at 的 job 并单列；改用 run_started_at | ciorch #2 |
| 21 | `ciorch -config x`（无子命令）panic | 去掉 -config 后 rest 为空没判断 | 抽 parseArgs + 测试 | ciorch #2 |
| 22 | 新配置加载报「未知 provider（已注册：[]）」 | 配置改为经 `machine.New(kind)` 动态构造后，cmd 不再引用 provider 包，init 注册不执行 | cmd blank import | ciorch #23 |
| 23 | `cirunner act <name> --json` 报用法错误 | Go flag 遇到第一个位置参数就停止解析 | 手工分离 flag 与位置参数 | cirunner #5 |
| 24 | 机器上取不到 runner 版本 | `.runner` 文件没有 version 字段 | `config.sh --version` | cirunner #4 |
| 25 | health 磁盘/进程判断错 | df 取了 Capacity 列而非 Available；pgrep 空输出被误判为在运行；dash 下 `return` 在函数外 | 修解析 + `internal/sys` 用真实输出样本测试；`return`→`break` | cirunner #6 |
| 26 | `pgrep -f Runner.Listener` STOP 后 ssh 断开 | 匹配到会话 bash 自己的命令行，STOP 也打到 bash | 人工 ssh 命令里用 `[R]unner.Listener` 写法（机器侧 Go 代码直接 exec `pgrep -f`，不受影响） | ciorch #24、#26；REDESIGN §5.4（其「机器侧用 pgrep -x」说法与代码不符） |
| 27 | 原型 stats 与 shadow 对比口径被污染 | 额度共用、整点重置 | 用 `/rate_limit` 前后 `core.used` 差测 | ciorch #14 |
| 28 | **切换后场景 A 误杀在跑的 job**（11-22 17:00，nms main run 45046644413：build/test 跑到 3 分钟被 `restart-runner` 打断；16:44 同类误判因 ssh 255 未得手） | ① 两台 job 开跑后约 3 分钟内，ciorch 每轮都把两台 runner 读作 busy=false（日志 `I=2`，I 是 online 且空闲的 runner 数）；同期 Q 由 7 降到 5，说明 jobs 一路已看到两个 job 离开排队。17:14 对同一 run 的 rerun 同条件下 ciorch 读数正确（`I=0`），我方无缓存逐 5s 复测亦正确，未复现；来源未定（GitHub busy 滞后或 ETag 304 复用空闲期缓存）；② 判据用**队列**年龄 QAge，满载排队 12m 后机器刚跑完 job 的交接间隙（实测 77s 才接下一个）也被当假活 | busy=false 须有 jobs 佐证（本机 runner 无 in_progress job）；另要求本机 `IdleSince` 持续 ≥ `stall_queue_age`；回归测试 + 变异测试 | ciorch #56 |

### 9.2 部署运维

| # | 现象 | 根因 | 解法 | 出处 |
|---|---|---|---|---|
| 1 | `vpsctl create -authorized-keys` 报「无 ssh_password 且未提供 --user-data/--ssh-keys」 | team2-ci 是无密码账号；`-authorized-keys` 只在 vpsctl 为**密码账号**自动生成 cloud-init 时才合并，给了 `-user-data` 就原样透传 | 自带 `-user-data`（cloud-config 的 `ssh_authorized_keys`），不用 `--allow-bare` | runbook §二 |
| 2 | 密钥认证成功但 PAM 强制改密，进不了 shell（`Password change required but no TTY`） | 建机请求没带 DO 账号级 SSH key，DO 生成随机 root 密码并标记 expired | user-data 里 `runcmd: chage -d "$(date +%Y-%m-%d)" root`；`chpasswd: {expire: false}` **没用**（没有被设置过的密码）；`vpsctl rebuild` 不支持 user-data，只能删机重建 | runbook §二；cirunner 坑 7 |
| 3 | 新机 active、ICMP 通，但 22/80/443/2222 全超时 | 新分到的 IP 在 WSL 出口路径上被**按目标 IP** 整体丢 TCP；Windows ssh.exe 同样不通；power cycle 无效 | 删机重建换 IP；不要改 ssh 端口或折腾 ssh.exe | REDESIGN V8；runbook §二 |
| 4 | 部署后 journal 看不到 webhook ping | server 的 ping 分支静默返回 200，不打日志 | 用 App 私钥铸 JWT 查 `GET /app/hook/deliveries`；或用 secret 自签一个 ping | 04 §2 #5 |
| 5 | healthcheck run 显示 success 但什么都没探测 | 部署漏配仓库变量 `CIORCH_HOST`，当时写法是变量缺失就 `::notice` 跳过 | 补配变量；缺变量改为 `::error` + `exit 1`；runbook 写明必配并要求手动 dispatch 后核对日志里真有 curl 输出 | ciorch #49 |
| 6 | healthcheck 开 issue 失败：`could not add label: 'ciorch-down' not found` | 仓里没有预建 label | HC 开 issue 前 `gh label create ... \|\| true` | ciorch #49 |
| 7 | `systemd --user` 服务取不到 GitHub token | systemd 环境的 PATH 里没有 `gh` | 用 token 文件（0600） | shadow 启动（11-20 10:25:27） |
| 8 | 目录权限口径不一致 | 仓内 `deploy/install.sh` 写 `install -d -m 0750 /etc/ciorch`，线上 0700（部署脚本手工建） | **未修**；直接跑 install.sh 会把目录放松到 0750（凭据文件仍 0600，不影响其检查） | §12.1 |
| 9 | 示例配置照抄后 `/status` 永远 401 | 示例配置与 runbook 都没列 `status_token_file` | **未修**，见 §12.1 R8 | — |
| 10 | S4 关 issue 后立即建机仍用坏 sha | 关 issue 时配置还没还原 | **先还原配置，再关 issue** | 回归 |
| 11 | S6 取消 run 前关了 issue，被编排器重开 | job 仍在排队，同键事故 24h 内复发 | **先取消 run，再关 issue** | S6 |
| 12 | 一次冒烟 S1/S2 各多等约 45 分钟 | LastActivity bug（#47），不是临时参数问题 | 修 #47；并非靠调阈值提速 | 04 §5 |
| 13 | 消息发给长任务中的 session 要等对方本轮结束才被读到 | SendToSession 进队列 | 用后台 `sleep` 计时器 + `WaitSessions`；不值得打断时不打断 | 02 §8.2 |
| 14 | 切换后 nms build/lint 由约 1 分钟变为 9–10 分钟，main dispatch run 45051671228 的 build 在 10m06s 被取消 | nms 的 setup-go 缓存 key 恒定（Go 版本 + go.sum），命中不重存：main 作用域那份是 11-15 存的 534MB；当日 DO sgp1 到 GitHub 缓存服务降到 1.0MB/s（15:00 时 12.8MB/s），534MB 恢复要 9 分钟，撞 `timeout-minutes: 10`。**与 ciorch 无关**（该 run 无任何编排动作） | 删 main 旧缓存，下个 main run 自动重存 63MB；验证 run 45054284248 7/7，build 22s、lint 2m12s、test 7m46s，整 run 约 9 分钟。用时再逼近超时先查 setup-go 步骤的 `Cache Size` 与速率 | nms P146（nms#635） |

### 9.3 GitHub / DO 平台

| # | 现象 | 根因 | 解法 | 出处 |
|---|---|---|---|---|
| 1 | run 的 `status` 聚合对自建 runner 滞后，会「停在 queued」；`?status=in_progress` 可能返回 0 | GitHub API 聚合怪象族（nms pitfalls 2026-11-15，**该条无编号**） | 判据一律用 jobs 接口；客户端自己过滤 `status != completed`。**编号说明见 §13.4（P141 编号错位）** | PF:913；REDESIGN §1.3 |
| 2 | 自建 runner 静默停摆：online、busy=false、journal 零日志，重启即愈 | P139（Listener 在跑但轮询卡死） | 判据必须用 GitHub 侧信号；场景 A | PF P139 |
| 3 | `DELETE /actions/runners/{id}` 对忙碌 runner 返回 422 | V1 | Drain 先注销后删机，失败则放弃；残留注册等 job 完成后自清（S3 约 4 轮重试） | REDESIGN §11 V1 |
| 4 | `kill -STOP` Listener 不复现假活 | V6：表现为 offline（S3 实测 busy=true） | 场景 C 处理，A 阈值不变 | V6；S3 |
| 5 | 强删机器上进行中的 job 不会被 GitHub 重排 | 平台行为 | 设计边界：只保证剩余**排队** job 被替换机接走；被中断的 job 以失败告终 | smoke.md S2 |
| 6 | runner 2.328.0 注册 404 | GitHub 强制 ≥2.329.0 | cloud-init 一律装 latest（另有版本号格式守卫） | nms runbook；cirunner 坑 2 |
| 7 | `svc.sh install` 后 unit 找不到 | 需要 daemon-reload；unit 名里仓名用连字符（点号会 Unit not found） | `daemon-reload` 后再 start；用 `owner-name` 形式 | cirunner 坑 3、4 |
| 8 | unattended-upgrades 开机约 20 分钟后打断正跑 job | 自动升级重启服务 | cloud-init 设 `Unattended-Upgrade "0"` | nms runbook；cirunner 坑 1 |
| 9 | user-data 里中文注释被毁成 C1 控制字符 | DO user-data 管道非 ASCII 问题（nms P75） | 渲染后强制纯 ASCII 校验；ASCII 守卫在开发当天两次抓到作者自己写的中文注释 | PF P75；cirunner |
| 10 | `vpsctl` 带 ssh_password 的账号与 `--user-data` 互斥 | vpsctl 硬拒（nms P70） | 为 CI 新增无密码账号条目 `team2-ci` | PF P70 |
| 11 | team2 与 team2-ci 同 token，不加 `-only` 同一台机列两遍；漏 `-only` 建机会全账号各建一台 | 同 token 多账号 | 必须 `-only` 单账号并按 droplet id 去重；归属只靠 tag 圈地 | RB:35；REDESIGN §3.7 |
| 12 | DO 关机仍计费 | DO 保留 CPU/内存 | 只有删机才停止计费 | S-14a3 |
| 13 | vpsctl `list` 的 droplet `id` 是字符串；vpsctl 命名删机重建后从 01 重新计数 | 契约细节 | 按字符串处理；按 runner_id 而非名字统计 | cirunner 47221cc；ciorch #1 |
| 14 | 个人 token 额度与原型、shadow、手动 gh 共用 5000 次/小时 | GitHub 额度按用户算且整点重置 | 上线后改用 App installation token（独立额度） | ciorch #27；S-a68a |
| 15 | GitHub cron 稀疏；60 天无活动停用 schedule | 平台行为 | 长期无 PR 时定期手动 dispatch healthcheck；不要依赖它做分钟级告警 | runbook §三；04 §9.6 |
| 16 | cloud-init `set -eux` 会把 1h registration token 打进 `/var/log/cloud-init-output.log` | 模板写法 | 维护者明确不做脱敏，token 1 小时失效可接受 | REDESIGN §2.3 |
| 17 | 私钥入口找不到 | GitHub 把入口从 General 页挪到左侧栏 **Credentials → Key pairs → New key**，协调者给的步骤过时 | 以官方当前页面为准 | 02 §6.2 |

### 9.4 多 session 协作与工具

| # | 现象 | 根因 | 解法 | 出处 |
|---|---|---|---|---|
| 1 | **SendToSession 报 `sessionNotFound`**（11-21 15:38 对 S1、S2；11-22 07:14 对冒烟 session） | **根因不确定**：猜测有「上下文过大加载不了」与「会话控制通道临时故障」两种，未证实。ListSessions 当时仍显示原 session idle 且 availableActions 含 sendToSession；ReadSession 读取正常、只是发送失败 | 11-21：两个旧 session 的活已交付审完，新开部署 session 并在开工说明里写全背景；11-22：协调者亲自执行切换。**经验：派工前不要假设对方 session 可达；开工说明自包含，不依赖对方的上下文** | 02 §8.1；协调者 11-22 记录 |
| 2 | **原协调 session 被 content-filter 拦截**（11-22 07:14 起每次模型调用 `finishReason=content-filter`、仅输出 1 token，过滤器不给原因；上下文约 78.5 万 token） | **触发原因不确定**。拦截发生在输出侧；该 session 累积了大量凭据相关内容（明文 DeepSeek key、App 私钥路径、webhook secret 路径、ssh/删机/读密钥命令）。以上只是背景事实，不能当因果结论。用户 07:16 的「这个文件只是个人使用」解释没有改变结果 | 归档；新开协调 session，只读 REDESIGN / PROVIDER-ABSTRACTION / runbook / smoke 与冒烟 session 最后报告即可接手。**经验：长任务定期换新 session，不要把凭据贴进聊天** | 协调者 11-22 记录；02 §8.3 |
| 3 | **凭据出现在聊天记录**：DeepSeek key 明文（用户 11-21 16:21 贴出，协调者实测后提醒可在冒烟后作废重建，用户是否作废**未见记录**）；webhook secret 在部署 session 里被明文贴出（维护者要求查看以填进 App 设置页） | 维护者要把值填进外部页面 | **建议轮换两者**，见 §10.9；不要再把凭据贴进任何 session | 02 §6.3；04 §2 #8 |
| 4 | 协调者「凭印象打勾」被抓：阶段 2 自查写「`Complete=false` 不产生删除有测试 ✓」，实际只有空闲缩容判断了 Complete；nms #622 「走了 docs-only」不属实 | 没逐条对照代码 / 没看 job 日志 | 要求自查逐条对照代码并写出行号；结论前先看日志；开新 session 时在开工说明中特别强调 | 02 §4.2；03 §5 |
| 5 | `git add -A` 把未跟踪的 `docs/REDESIGN.md` 推到 cirunner 公开仓远端分支约 1 分钟 | 偷懒 | 立即删除远端分支、重建干净提交；之后一律显式列路径提交。判定不构成泄漏（ciorch 已公开更新版本） | 03 §6.2 |
| 6 | #37 首次推送是坏的（只含两个新文件，构建必红） | stash 后 checkout 失败（main 被另一个 worktree 占用），rebase 没执行，stash pop 也失败 | reset → 恢复 stash → 从远端坏提交取回两个新文件 → 完整重新提交并 force-push；CI 绿才合并，main 未被污染 | 03 §6.2 |
| 7 | 主 checkout 里有别的 session 未提交改动；两 session 共用 `~/ciorch` | 并行开发 | 独立 worktree `~/ciorch-s5`（**至今仍在**，分支 `fix/incident-review-round1`，已合并，见 §12.3） | 03 §3.3 |
| 8 | python 字符串替换静默失败（多次）；zsh heredoc 吃掉反引号致 PR 正文受损；`grep -v` 无匹配退出码 1 链式命令中断；`gh pr merge` 后续命令失败被误读为没合并 | 脚本无 assert / shell 细节 | 改用 Edit 工具；PR 正文先写文件再 `--body-file`；合并后用 `gh pr view --json state` 核对 | 03 §6.2 |
| 9 | #30 串了 githubapp 的提交（多分支并行） | 分支管理失误 | #30 回退到只含 llm，githubapp 改成堆叠分支另开 #33 | 03 §6.2 |
| 10 | 状态文件/cron 类定时检查被长任务 session 吞消息 | 消息排队 | 同 §9.2 #13 | — |

## 10. 运维手册摘要

> 摘要版。完整步骤以 `ciorch/docs/runbook.md` 与 `ciorch/docs/smoke.md` 为准。注意 runbook 与 smoke.md 仍引用不存在的子命令（runbook 的 `check-config`、`up`/`down`，smoke.md 的 `reset`；`bake` 只在 REDESIGN），见 §12.1。以下命令均在编排器机器（root 或 sudo）上执行，除非另注。

### 10.1 日常检查

| 检查 | 命令 | 期望 |
|---|---|---|
| 存活 | `curl -s localhost:8080/healthz`；外部 `curl -s https://168-144-104-79.sslip.io/healthz` | `ok`（HTTP 200）。503 表示 `lastOK` 距今超过 3×`poll_idle`（约 180s） |
| 服务状态 | `systemctl status ciorch` | active (running) |
| 状态 | `curl -H "Authorization: Bearer $(cat /etc/ciorch/status.token)" localhost:8080/status` | 各仓 `Q/I/P/N/QAge/Complete/Breaker/Machines`；空闲时 `N=0`、`breaker=closed`、`Complete=true`。**不带 token 一律 401**；`/status` 没有机器列表、未结事故、今日花费 |
| 日志 | `journalctl -u ciorch -f` | 每轮一行摘要 `[owner/name] Q=.. QAge=.. I=.. P=.. N=.. actions=[...] complete=true`；空闲约 62s 一轮，活跃约 15–20s 一轮；每小时一行 `api-usage 1h:` |
| 干跑决策 | `ciorch -config /etc/ciorch/ciorch.json plan` | 对每个托管仓打印 `complete=true`、`Q`、动作；**不执行**（#52 之后真正只读） |
| 实时观察 | `ciorch -config /etc/ciorch/ciorch.json status` | Q/QAge/runner/机器/unmatched/pending/pool |
| 近期统计 | `ciorch -config /etc/ciorch/ciorch.json stats -repo hoshinojian/nms -since 24h` | 排队等待 p50/p95/max、run 总时长、冷启代理（#54 之后编排器上可用，走 App 认证） |
| 机器清单 | `vpsctl list -tag ciorch -only team2-ci`（编排器上以 ciorch 用户跑，用 `-accounts /etc/ciorch/vpsctl-accounts.json`） | 空闲时 0 台；非空闲时 ≤ `global.max_machines` |
| 编排器自身 | `vpsctl list -tag ciorch-host -only team2-ci` | 恰好 1 台 |
| runner 注册 | `gh api repos/hoshinojian/nms/actions/runners` | 空闲时 `[]` |
| 外部健康检查 | ciorch 仓 Actions → healthcheck | cron 触发稀疏，60 天无活动 GitHub 停用 schedule；长期无 PR 时手动 dispatch 一次，并点开步骤日志确认**真有 curl 的 `ok` 输出** |
| 事故 | nms 仓与 ciorch 仓里带 `ciorch` 标签的 open issue | 理想为 0 |
| 花费 | DO 账单；`state.json` 的 `budget.spent_today`（UTC 日） | 单日远低于 $8 |

排查顺序建议：healthz → status → journalctl → plan → `vpsctl list` → GitHub runner 注册与 job 状态。判据只信 GitHub 侧信号，不信机器日志静默（P139）。

### 10.2 升级二进制

前提：**无活动 run、`N=0`**（有机器在跑时替换重启虽然状态可恢复，S7 与冒烟中多次实测无损，但仍应避开跑 CI 的时刻）。

1. 本地构建（linux/amd64 静态）：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o ciorch.new ./cmd/ciorch`；记下 `sha256sum ciorch.new`。
2. 确认无活动 run：`gh run list -R hoshinojian/nms --status in_progress` 与 `/status` 的 `N=0`。
3. `scp ciorch.new root@168.144.104.79:/usr/local/bin/ciorch.new`。
4. 在编排器上备份旧版：`cp -p /usr/local/bin/ciorch /usr/local/bin/ciorch.bak-<旧sha前12位>`（沿用已有约定；现有备份 `a67b45f677b4`、`e40767506aee`、`5761d103e977`、`d5ace1280b26`、`f652ebce1b48`）。
5. 替换并重启：`mv /usr/local/bin/ciorch.new /usr/local/bin/ciorch && systemctl restart ciorch`。
6. 验证：`systemctl is-active ciorch`、日志出现各仓 `complete=true`、`curl localhost:8080/healthz` 为 `ok`、`ciorch plan` 输出合理；本机 sha 与远端 `sha256sum` 一致。
7. 回滚：`cp -p /usr/local/bin/ciorch.bak-<sha12> /usr/local/bin/ciorch && systemctl restart ciorch`。

当前线上二进制：`1e3b1bacda83`（11-22 17:42 部署，main `17f0bab`，含 #50–#54 与 #56）。

### 10.3 修改配置

1. 备份：`cp -p /etc/ciorch/ciorch.json /etc/ciorch/ciorch.json.bak-<说明>`（切换前备份名为 `ciorch.json.bak-pre-nms`）。
2. 写候选文件（不覆盖在用文件），如 `/etc/ciorch/ciorch.json.new`，权限 0600、属主 ciorch。
3. **用候选文件校验**：`ciorch -config /etc/ciorch/ciorch.json.new plan`。要求：`complete=true`、预期的 Q、无意外动作；配置错误会在加载时以中文报字段路径（`DisallowUnknownFields`，拼错键名直接拒绝）。
4. 确认无活动 run 后 `mv` 替换 + `systemctl restart ciorch`。
5. 验证同 §10.2 第 6 步。
6. 回滚：把备份文件 `mv` 回去并重启（config 错误会在启动时校验失败退出，journal 有中文错误与字段路径）。

注意：

- **绝不**改 nms 的 `size`/`region`（`c2-4vcpu-8gb` / `sgp1`）。
- `global.max_machines` 须 ≥ 任一仓的 `max_runners`。
- 改 `global.tuning` 仅用于实验，测完删掉再重启，并还原配置后再关事故 issue（见 §9.2 #10）。
- 有 `pause_repo` / `switch_pool` 内存态覆写时，重启即丢。

### 10.4 新增托管仓

1. **GitHub App 安装**：`https://github.com/settings/apps/ciorch` → Install App → 选新仓（Only select repositories）。App 权限须含 Administration / Actions / Issues 读写，订阅 Workflow job 与 Issues。若要把事故 issue 改投到别的仓（`issue_repo` 非空），App 也必须装在那个仓。
2. 确认 installation：用 App JWT 查 `GET /repos/<owner>/<repo>/installation` 返回 200（`ciorch plan` 观察到该仓也等价）。
3. 决定规格：**非 nms 项目**若无 `-race` 时敏用例，可选 s 系共享核（约便宜 8 倍），但覆盖率口径与测试稳定性须自行评估；需要专用核就复用 `do-sgp1-c2` 池。
4. 修改配置（§10.3）：
   - `pools`：池已存在则复用；新池须填 vpsctl 私有配置且 `hourly_cost_hint > 0`；
   - `profiles`：`pools` 长度必须为 1；`labels` 非空（空 labels 会使建机在渲染阶段失败）；`runners_per_droplet` 保持 1；
   - `repos`：加 `{repo, profile, max_runners, issue_repo}`；
   - 视需要上调 `global.max_machines`（须 ≥ 任一仓 `max_runners`）与 `daily_budget_usd`。
5. 该仓的 workflow 的 `runs-on` 要包含 profile 的 labels（如 `[self-hosted, ci]`）；每个 job 加 `timeout-minutes`（默认 6 小时会让卡死 job 占机 6 小时）。
6. 候选文件 `plan` 校验，替换重启，验证：先在该仓手动触发一次 workflow，观察建机、开跑、回收；检查 App 设置页 Recent Deliveries 有 `workflow_job` 且为 200。
7. 多仓托管后留意：#50 已使 webhook 缓存按仓隔离；若某仓 job 出现在别的仓的观察里，先查是否部署了 #50 以后的二进制。

### 10.5 事故 issue 怎么处理

- ciorch 开的 issue 带 `ciorch` 与 `ciorch:<场景码小写>` 标签，标题 `[ciorch] <场景>：<主体>（<repo>）`；需要人时加 `needs-human` 并 @`maintainer_login`。
- **人工关闭 issue 的语义**：webhook `issues.closed` 且带 `ciorch` 标签、sender 不是 `ciorch[bot]` → 该事故 resolved，并对托管仓**清熔断（`Fails=0`）、`CreateFailStreak=0`、清退避、清 pause、清 switch_pool**。所以：
  - 熔断打开（B 场景）时，**修好根因后再关 issue**，否则关闭会立即恢复建机并重新踩同一个坑；
  - 先还原配置/注入再关 issue（回归教训：关 issue 时坏 sha 未还原，白烧约 $0.006 与 8 分钟）；
  - E 场景（label 无人满足）要**先取消 run，再关 issue**，否则 job 仍排队，24h 内同键复发会被重开；
  - 编排器自己恢复时关 issue 不会被当作人工关闭（bot 过滤 + 状态防护）。
- 事故不会随时间自动处理的情况：`waiting-human`（G、LLM `needs_human`、结论不合法、限额已满）不会自动关，必须人处理并关。
- **R4 风险**：B（熔断）、I（streak=3）、F 只在转换瞬间发一次，若同一轮 Tick 未进入 waiting-human（没配 LLM key，或 LLM 给了合法动作进入 verifying），约两轮不复发就会被「安静恢复」自动关 issue，**而熔断可能仍是 open**。看到 issue 被自动关但仍不建机时，查 `/status` 的 `Breaker`。
- 手动复位熔断：没有 `ciorch reset` 命令。可行办法：关对应 issue；或停服务后手改 `state.json` 的熔断字段再启动；LLM 的 `clear_breaker`。
- 诊断结论仅供参考：材料缺证据时会误判（§8.6）。**先看 issue 里的 timeline 与 `machine_diag`，再看 LLM 评论**。

### 10.6 费用与限额

- **常驻**：编排器 $4/月。
- **按需**：约 $0.14/h/台；一次完整 nms run 约 $0.15。
- **硬上限**：`global.max_machines=3`、nms `max_runners=2`、c2 池容量 2、`daily_budget_usd=8`。投影按「新机 1 小时」，真实花费以 `RecordDeletion` 入账。
- **月成本**要等一个完整月的 DO 账单；本文数字为预估，见 §1.4。
- DeepSeek：每事故最多 3 次、每天最多 20 次（默认值，配置项未接线）；建议在 DeepSeek 后台设月度上限。
- 成本异常信号：`vpsctl list -tag ciorch` 在无 run 时仍有机器（空闲回收 20/45 分钟，硬寿命 8 小时）；日志里反复出现 `已建机` 但无 job 开跑；`state.json` 的 `spent_today` 接近 $8。

### 10.7 回滚到原型（完整步骤）

触发条件（协调者切换方案）：建机连续失败熔断、droplet 超 `max_machines`、成本异常、nms CI 因机器问题连续失败两次。

1. 备份当前状态：`cp -p /etc/ciorch/ciorch.json /etc/ciorch/ciorch.json.bak-rollback`。
2. **从 ciorch 配置去掉 nms**：用 `ciorch.json.bak-pre-nms` 还原（该备份是 07:23 切换前的配置，只托管沙箱），或手动删除 `repos` 里的 nms 条目；还原后 `global.max_machines` 回到 2、`daily_budget_usd` 回到 3。
3. 确认 nms 无活动 run（`gh run list -R hoshinojian/nms --status in_progress`）。
4. `systemctl restart ciorch`，确认日志里已不再出现 nms 的轮询行。
5. **清掉 ciorch 名下的 nms 机器**：`vpsctl list -tag ciorch -only team2-ci` 中 nms 的机器（名字形如 `ci-nms-team2-ci-sgp1-NN`、tag 含 `ciorch-r-hoshinojian-nms-…`）应为 0；若仍有残留，**先问维护者**再 `vpsctl delete -ids team2-ci/<id> -confirm 1`，并清 nms runner 注册（`gh api -X DELETE repos/hoshinojian/nms/actions/runners/<id>`，忙碌的返回 422，等空闲）。**绝不碰 `ciorch-host`。**
6. **再启原型**（WSL，维护者机器）：`systemctl --user enable --now cirunner`；`systemctl --user status cirunner`、日志里应出现 nms 的轮询行。回滚前提：以下三项仍在——`~/.local/bin/cirunner-proto`、`~/.config/systemd/user/cirunner.service`、`~/cirunner` 仓（含 `~/.config/cirunner/cirunner.json` 与 `states.json`）。
7. 两者**不能同时建机**：必须先确认 ciorch 名下 nms 机器为 0，再启原型；原型只认 `cirunner` / `repo:nms` 标签与 `cirunner-team2-ci-sgp1-NN` 命名，看不见 ciorch 的机器，也不会回收它们。
8. 验证：手动触发一次 nms CI，`vpsctl list -tag cirunner -only team2-ci` 出现机器，7 job 全绿，空闲回收后清零。

注意：原型二进制已核实是带热补丁的构建：`go version -m ~/.local/bin/cirunner-proto` 显示 `vcs.revision=508a0b0…`、`vcs.modified=true`，文件 mtime（11-20 04:39:47）比热补丁提交 53be365（04:39:56）早 9 秒，且二进制内含 53be365 新增的字符串（如「观察 in_progress run 内排队 job」）——即从提交前的脏工作区构建；原型仍有已知的 13 个缺陷（尤其「run 粒度」与「无 label 过滤」），回滚期间要接受。**回滚到原型后若在观察期结束前收尾 PR 已删除原型代码，路径将不再可用**——因此收尾 PR 须在观察期结论之后。

### 10.8 编排器自身故障与恢复

- 编排器宕机**不影响在跑的 CI**（job 派发不经过它）；只是不再自动建删机。healthcheck workflow 会开 `ciorch-down` issue（但 cron 稀疏，见 §10.1）。
- 恢复：`systemctl restart ciorch`；日志有中文错误；`state.json` 缺失则冷启记忆清零，但 droplet `CreatedAt` 仍是判龄依据，已在线机器不会被误判冷启超时。
- 机器丢失：重建流程见 `docs/runbook.md` §二，**要点**：`-only team2-ci`、自带 `-user-data`（公钥 + `chage -d`）、tag 只用 `ciorch-host`；新机若全 TCP 丢包，删机重建换 IP。
- 彻底退役：停服务、Uninstall App 或删 webhook、`vpsctl` 删 `ciorch-host` 机器，state 目录归档后删。

### 10.9 凭据轮换建议

**素材显示以下两项曾明文出现在聊天记录，建议轮换**：

| 凭据 | 现状 | 轮换步骤（摘要） |
|---|---|---|
| **DeepSeek API key** | 用户 11-21 16:21 明文贴入协调 session；协调者实测后提醒冒烟后作废重建；用户是否已作废**未见记录**。同时出现在分叉 session（sess_55ccca3a…）中 | 在 DeepSeek 后台作废旧 key、建新 key；用 stdin 写入 `/etc/ciorch/deepseek.key`（0600，属主 ciorch，不落到命令行参数或聊天）；`systemctl restart ciorch`；日志中「诊断禁用」提示不应出现；顺便设月度上限 |
| **webhook secret** | 部署 session 里被明文贴出（维护者要求查看以填入 App 设置页） | 机器上 `openssl rand -hex 32 \| tr -d '\n' > /etc/ciorch/webhook.secret`（0600 ciorch），**同时**在 GitHub App 设置页改为相同值；重启 ciorch；验证 App 设置页 Recent Deliveries 出现新的 200（401 说明两边不一致）。必须两边口径一致，轮换期间 webhook 会短暂失效，轮询兜底继续工作 |

其他凭据（本次素材未显示曾外泄，但属同一类风险，评估是否轮换）：

- **GitHub App 私钥**：线上 `/etc/ciorch/app.pem`，WSL `~/.config/ciorch/app.pem` 有副本（R9）。轮换：App 设置页 Credentials → Key pairs → New key；更新两处文件；删除旧 key。
- **DO token**（team2-ci，来自 vpsctl 账号文件）：同 token 也在 team2 条目；轮换需同步更新维护者本机与编排器的账号文件。
- **status token**、**编排器 ssh 私钥**：编排器上可随时重新生成；ssh 私钥更换后须重建所有 runner 机器才能让新公钥进入 authorized_keys（`ssh_public_key_files` 随 cloud-init 写入，存量机器不更新）。
- WSL 上 `~/.config/ciorch/shadow.token`（个人 PAT，shadow 期用）：shadow 已停，确认后可作废。

**绝不**把任何凭据内容贴进聊天、PR、issue 或文档。

### 10.10 观察期脚本与唤醒条件

脚本：WSL `~/.local/state/ciorch-observe/observe.sh`，日志同目录 `observe.log`；每 30 分钟记录：healthz、`/status`（nms Q/N/机器数/熔断）、ciorch 名下 droplet 数、当日花费、观察窗内 `ciorch[bot]` 开的 issue、新完成的 nms run 结论。**任一异常即退出唤醒协调者**：healthz 连续 2 次非 200、熔断非 closed、机器 >3、出现 ciorch 事故 issue、nms run 非 success/cancelled。实际：14:49 因 run 45033783948（P145 测试偶发）触发退出，经核实非执行面问题，未再重启；结论见 §12.2。

## 11. 多 session 协作模式回顾

> 本节回顾这套系统是怎么被「多个大模型 session + 一个维护者」做出来的。目的：日后再做类似的长链路工程时，可以直接复用分工与审查方法。出处主要是 02（协调）、03（实现）、04（部署冒烟）三份素材。

### 11.1 角色与 session 一览

| 简称 | session id（前缀） | 职责 | 时间（UTC） | 备注 |
|---|---|---|---|---|
| 协调（原） | `sess_a68a5090…` | 设计、审查、派工，**不写业务代码** | 11-19 16:13 → 11-22 07:54 | 598 条消息，后期单轮输入约 78 万 token；11-22 07:14 起被 content-filter 拦截，已归档 |
| 协调（新） | `sess_0afeb411…` | 接手后亲自执行 nms 切换、修隐患、起草本文 | 11-22 07:14 之后 | 向冒烟 session 转发切换指令同样被 `sessionNotFound` 拒收，故亲自执行 |
| 协调分叉 | `sess_55ccca3a…` | 原协调 session 的前缀副本（前 414 条一致，之后无独立内容） | 11-19 16:13 → 11-21 15:36 | 对交接无新增信息，可忽略；**同样含明文 DeepSeek key** |
| 写码 A（S1） | `sess_2e223496…` | 阶段 0a/0b/0c、1、2、shadow、3（cirunner 机器侧）、渲染器接入、阶段 4 代码、冒烟准备 | 11-20 04:36 → 11-21 14:05 | 11-21 15:38 起对其 `SendToSession` 报 `sessionNotFound`，读取正常 |
| 写码 B（S2） | `sess_60855587…` | Provider 抽象、阶段 5 事故管线、#39 审查修复 | 11-21 10:46 → 14:09 | 同样 15:38 起 `sessionNotFound` |
| 部署（D） | `sess_2c920fad…` | 建编排器机器、装机、App、webhook、启动、沙箱冒烟准备 | 11-21 15:39 → 16:26 | 因 S1、S2 发送失败而新开 |
| 冒烟（S） | `sess_1f08eb65…` | S1–S8、冒烟中修复、回归；11-22 被派发切换任务但消息未送达 | 11-21 约 16:25 → 11-22 01:25 | 创建时刻按文本推断（部署 session 派生）【不确定】 |
| 场景测试（T） | `sess_4a544768…` | 切换后造 5 个测试 PR 验证真实 PR 场景（§8.9），只取证不改配置 | 11-22 13:45 → 16:03 | 由新协调创建；报告经协调者独立复核 |

维护者本人：只做决策与少量 GitHub 网页操作（创建 App、生成私钥、安装 App、填 webhook、贴 DeepSeek key），并在早期把写码 session 的交付以 `.zcode-session` 文件路径转给协调者。

### 11.2 分工的切法

1. **协调者只出 plan 与审查，不写业务代码**（用户原话：「你只用负责列 plan，干活的话我会找其他人的」）。协调者全程只有两次 Edit、两次 Write，用于固定原型二进制和文档。
2. **设计文档 = 唯一依据**。把「动机 + 思路 + 所有写码部分」写进 REDESIGN.md（约 950 行），让换了模型的写码 session 照着干，写完交回审查。文档里明确：「文中没写的设计结论不要自己发明，拿不准的先在 PR 描述里列『待确认』」。
3. **按仓切，两个 session 不同时改同一处**：
   - 11-21 10:46 的分工：S2 只在 ciorch 做提供商抽象；S1 只在 cirunner 做阶段 3（外加三仓 LICENSE）。
   - 唯一冲突点提前隔离：阶段 3 最后一步「cloud-init 渲染器接入 ciorch」与抽象都要改 `engine.go`，让 S1 等 S2 的 PR ③（#20）合并后再接；REDESIGN 文档也划范围（S1 只改 §5，S2 只改抽象计划列出的节）。
   - 阶段 4/5 并行：S1 写阶段 4（githubapp 认证、server、deploy/、healthcheck、runbook），S2 写阶段 5（incident、llm、githubapp 里**新开** issues.go 不动 client.go）。唯一交接点是 `HumanResolved(repo, issueNumber)`，由协调者对接口。
4. **上下文干净的新 session 适合大面积改名重构**（S2），**熟悉来龙去脉的老 session 适合续做**（S1 做阶段 3）。用户 11-21 10:45：原 session 上下文已经很大，分工「由你来决定」。
5. **每阶段有验收标准**：REDESIGN §9 的阶段表（0a–7）每一行都带验收；协调者以验收为准放行或打回。
6. **真实环境测试只用沙箱**：私有沙箱仓 + 手动触发 workflow，不在 nms 上做故障注入。
7. **协调者定期「巡检」**：用户 11-21 10:49 要求「每一小时看一下这俩 session 的工作情况，然后继续推」；实际做法是后台 `sleep` 计时器或 `WaitSessions`，约每 50–60 分钟读一次对方的交付、复跑测试、看关键代码，没问题直接给下一步指令。协调者只在「设计方向变化、花钱明显超预期、不可逆删除」时才找用户。

### 11.3 派工指令的要素

稳定结构（02 素材 §10.4）：**「审查结论 → 必须修（带复现）→ 顺手修 → 下一步 → 红线 → 交回方式」**。每条派工里应包含：

| 要素 | 内容 | 例子 |
|---|---|---|
| **背景** | 为什么做、做到哪了；对方 session 可能联系不上时必须自包含 | 11-21 15:39 新开部署 session 时写全背景，因为原 session「目前联系不上」 |
| **范围** | 只做什么、不做什么 | 「只托管沙箱仓，nms 不装 App」；「只做阶段 3，不碰 ciorch 代码」 |
| **先读什么** | 具体文件 | REDESIGN.md、PROVIDER-ABSTRACTION.md、docs/runbook.md、docs/smoke.md |
| **红线** | 不碰在役原型；不在 nms 上冒烟；不碰 vpsctl；不提交凭据；不在 DO 建机（需要时限定 1 台、注册到沙箱仓、用专用前缀、验完立刻删）；显式列路径提交（不用 `git add -A`）；draft PR + 五段描述 + CI 绿后可自行合并 + `git branch --contains` 核对 | §0.4 |
| **验收** | 可以对照检查的条目 | 「V1 先测」；「golden 完全一致」；「回归只重跑 S4 + 一次 healthcheck」 |
| **交回方式** | 哪些东西要带证据 | 命令 + 输出 + 时间戳；自查清单**逐条对照代码**并写出行号 |
| **回滚条件**（对切换类任务） | 什么情况下停 | 建机连续失败熔断、droplet 超限、成本异常、nms CI 因机器问题失败两次 |

### 11.4 审查方法

1. **对照代码核实，不凭印象打勾**。
   - 阶段 2 自查写「`Complete=false` 不产生删除有测试 ✓」，协调者读代码发现只有空闲缩容判断了 `Complete`。此后每次派工都强调「自查必须逐条对照代码」，并在新 session 的开工说明里点名「前一个 session 在这点上被打回过两次」。
2. **亲手复现**。阶段 2 审查时协调者写了 6 个临时复现测试（R1–R6），跑完删除；复审时把这 6 个场景搬到新代码重跑；还发现「修了一半」的浅拷贝竞态（用有机器的两仓 ×200 轮 `-race` 才能暴露）。
3. **看 CI 日志，不只看 checks 颜色**。nms #622「走了 docs-only」一例：checks 全绿，日志里 `docs_only=false`、test 跑了 11m25s；根因是 `core.quotepath`。
4. **golden 未重生成检查**。
   - 抽象 PR ③ 之前 golden 单独合入（#19），③ 之后协调者核对 golden **从创建后没被改过**。
   - 事后因加 `Scenario` 字段（#37）/`RecordedRuns`（#47）/`RecentProviderErrors`（#48）重生成时，协调者核对差异只是新增字段、决策输出无变化（#37 用归一化脚本证明）。凡是 `-update` 过 golden 的 PR，审查必须看 diff 而不是只看测试绿。
5. **用真实环境独立核验**。部署后协调者独立外部核验：healthz 200 + Let's Encrypt 有效；不带签名的伪造 webhook 返回 401；无 token 访问 `/status` 返回 401；ufw 只开 22/80/443；`/etc/ciorch` 0700、凭据 0600；runner 机器 0 台；只托管沙箱。
6. **「小回归」代替全量重跑**：冒烟后 5 处修复只重跑 S4 + 一次 healthcheck，而不是 S1–S8 重来。
7. **纠正自己**：协调者多次纠正自己的结论并留痕——V3（api-usage 推不出 304 是否计额度）、shadow「早 45 秒」预估（冷启并无优势）、App 私钥入口步骤过时、smoke.md 的「45m 符合设计」与「S2 最终 success」两处不实说法。**审查者自己的结论也要被证据挑战**。
8. **阶段性放行带限定条件**：阶段 2 复审「基本通过」时只放行 shadow，且限定：只托管 nms、`daemon -dry-run`、独立 systemd 用户服务、跑够 24h、不碰原型。
9. **优先在真实部署前抓问题**：阶段 4/5 提前审查（11-21 13:58–14:15）在部署前抓出 3 个必修（webhook secret 换行致验签必败、`issues.closed` 没接线、自关 issue 被当人工关闭），这三个都是测试发现不了、部署后才会爆的。

### 11.5 被审查打回的记录（汇总）

| 轮次 | 被审对象 | 结论 | 主要问题 |
|---|---|---|---|
| 0a 热补丁 | S1 | 通过 | 分支只在本地、没推送也没开 PR |
| 0b/0c nms PR | S1 | 通过但更正 | 报告说 #622 走 docs-only 不属实；#150 撞号 |
| 阶段 1 | S1 | **打回** | 重跑致负排队/总时长虚高；无子命令 panic；配置格式不符 |
| 阶段 2 | S1 | **不通过** | 假 dry-run、观察不完整误删误建、卡死判据错、非幂等、半开多建、并发崩溃、预算没接、自查虚报 |
| 阶段 2 复审 | S1 | 基本通过 | 并发修一半；小问题 4 个 |
| shadow 报告 | S1 | 通过 | V3 推论不成立（已纠正）；故障期事故每轮重发 |
| 阶段 3 | S1 | 通过 | sh `return 0`、`internal/sys` 无测试、cirunner 仓没 CI、第三方 action 未钉 SHA、restart-runner 双实例判断；误推 REDESIGN.md |
| 提供商抽象 | S2 | 通过 | 无逻辑变化；自己在 shadow 前真实验证中抓到 provider 未注册 bug（#23） |
| 阶段 4、5 | S1 / S2 | 通过但有 3 处必修 | webhook secret 换行、人工关 issue 未接线、自关 issue 被当人工 |
| 部署 | D | 通过 | 踩 3 个坑，删建 3 次约 $0.02 |
| 冒烟 | S | 通过 | 真环境抓 4 个 bug + 部署漏配 `CIORCH_HOST` + healthcheck 缺标签；DeepSeek 诊断质量参差；一次熔断开两个 issue；LLM 动作指向已删机器 |

协调者正面认可的做法（值得保留）：#626 用日志证实结论；复审自查逐条对照代码；shadow 抓到 labels bug 且观察冻结在真实故障下按设计生效；S2 在 shadow 验证里自己抓到 provider 没注册；#37 stash 事故如实报告。

### 11.6 教训

1. **单测假数据不真实，bug 在真环境才暴露**：labels 对象、公钥尾换行、webhook secret 尾换行、LastActivity 跨轮去重、同轮建机撞名、healthcheck 变量缺失——都是 shadow / 冒烟才抓到。**所以「先只观察的 shadow，再沙箱冒烟，再切真仓」这条链值得长期保留**。
2. **写码 session 的自查清单不能凭印象打勾**，协调者必须对照代码抽查。
3. **共用 GitHub 额度会污染度量**（V3）；不要用被他人污染的计数器下结论。
4. **监控自身要能红叉**：healthcheck 变量缺失时曾静默跳过却显示 success，宕机报警从部署起空转到 S8 才发现。凡「检查类」功能，没配好时必须报错，不要静默通过。
5. **大模型诊断质量取决于喂给它的材料**：S4 的误判是因为材料里没有建机错误原文。排障 LLM 的价值首先是证据面，其次才是提示词。
6. **session 不可靠，文档要自包含**：`sessionNotFound`（11-21、11-22 各一次）、content-filter（原协调 session）、长任务 session 吞消息——任何一个 session 都可能随时联系不上。派工指令与设计文档要自包含，接手者只需读 REDESIGN / PROVIDER-ABSTRACTION / runbook / smoke 与最后一份报告。这正是本手册存在的原因。
7. **凭据不要贴进聊天**：DeepSeek key、webhook secret 曾明文入聊天记录；content-filter 的触发原因虽不确定，但「上下文里堆满凭据与高危命令」至少是一条值得避免的风险。
8. **上下文过大本身就是风险**：原协调 session 到后期单轮输入约 78 万 token；长任务到一定程度应主动换新 session，并留好交接文档。
9. **合并与操作留证据**：显式列路径提交；PR 正文先写文件再 `--body-file`；合并后用 `gh pr view --json state` 核对；`git branch --contains` 核对 main 上确有该提交。
10. **「删」类操作先确认**：删 sgp2 前协调者单独求确认，用户明确说「删」才执行；回滚时清机器前先问维护者。

## 12. 已知问题与后续工作

> 来源：05 素材 §9（代码与文档不一致清单 + 风险 R1–R11）、协调者 11-22 记录（#50–#54 已修）、各素材的「不确定点」。状态列的「已修」均有 PR 号；「未修」表示本文起草时仍在代码里。风险项 R* 大多为【推断】（读代码得出，未实测）。

### 12.1 已知问题汇总（含状态）

#### A. 已由 #50–#54 修掉

| 问题 | 影响 | 状态 | 说明 |
|---|---|---|---|
| webhook JobCache 全局一份、不按仓过滤；且 map 无锁 | 多仓托管后他仓 job 混入观察，误开事故（sandbox#8）或误建机；并发写 map 崩进程 | **已修 PR #50**（部署 `e40767506aee`） | 变异测试证明两条回归测试各自能抓住对应 bug |
| `githubapp.Client` ETag 缓存 map 无锁（原 R2） | 托管 ≥2 仓后可能 `concurrent map read and map write` 崩进程 | **已修 PR #51**（部署 `5761d103e977`） | 加 `etagMu`；etags map 无界增长仍在 |
| dry-run 不是只读：事故模块未置 DryRun（原 R1） | `ciorch plan` / `daemon -dry-run` 可能真开 issue、真调 DeepSeek、真执行 LLM 动作（`status` 不受影响） | **已修 PR #52**（部署 `d5ace1280b26`） | 历史上 shadow 期间是否因此开过 issue **未核实**【不确定】 |
| `cirunner diag` 在机器不健康时 exit 1，ssh 封装丢弃 stdout（原 R3） | 最需要诊断的场景里 `machine_diag` 只剩 `exit status 1`；`act` 失败的 detail 同理 | **已修 PR #53**（同批部署） | exit 1 且输出为合法 JSON 时保留 |
| `ciorch stats` 只走 `gh auth token` 兜底 | 编排器上无 gh，stats 直接失败 | **已修 PR #54**（部署 `f652ebce1b48`） | 编排器上实测通过 |

#### B. 未修：代码与文档不一致

| # | 问题 | 影响 | 状态 | 建议 |
|---|---|---|---|---|
| 1 | CLI 只有 `stats`/`plan`/`status`/`daemon`；`up`/`down`/`reset`/`check-config`/`bake` 不存在；仍被引用（实测引用处：runbook.md:110 `check-config`、runbook.md:120 `up`/`down`、smoke.md:168 `reset`；`bake` 只出现在 REDESIGN.md:806-808、898；REDESIGN.md:439、642-646 另有 reset/check-config/up/down） | 复位熔断只能关 issue、LLM `clear_breaker` 或手改 state.json；按文档操作会失败 | **未修** | 先把文档改对（runbook、smoke.md、REDESIGN §3.13 标注未实现），再决定是否实现 `reset`（最有价值） |
| 2 | 场景 J（孤儿机器）未接线：`engine.ScanOrphans` 无生产调用 | 带 `ciorch` 公共标签却无仓标签的机器不会被发现、也不报警；成本泄漏盲区 | **未修** | 接入 daemon 周期调用，只告警不删 |
| 3 | 场景 F「腾配额」未实现；`budget.Allocate` 只在测试里用；无「QAge≥10m 先腾」档 | 多仓争配额时不会回收别仓空闲机；只在 QAge≥15m 告警 | **未修** | 现在 nms+沙箱 配额不紧；多仓增加时再做 |
| 4 | 场景 H：文档「连续 40 轮升级」，代码 ≥10 轮每轮都发 | 观察失败期间事故每轮重发（靠 incident 去重只开 1 个 issue） | **未修** | 统一口径（改文档或代码） |
| 5 | A 场景「重启失败且无配额」不升级，SSHAct 失败只记日志 | 假活修不好时可能无人知晓 | **未修** | 补升级条件 |
| 6 | Create/SSHAct 同步执行；SSHAct 无 60s 超时 | 一个仓建机最长阻塞本仓约 90s；ssh 挂住只受 ctx 约束 | **未修** | 给 SSHAct 加超时（优先级高于异步化） |
| 7 | 限流余量 <10% 时轮询间隔 ×4 未实现（`RateRemaining()` 无调用方）；**限流降频未实现** | App 额度独立，当前稳态约 113 请求/小时，风险低；托管仓增多后需关注 | **未修** | 仓数 >5 前补上 |
| 8 | `/status` 缺机器列表、未结事故、今日花费；token 比较非常量时间 | 排障要靠日志与 state.json | **未修** | 补字段；改 `subtle.ConstantTimeCompare` |
| 9 | `llm.max_calls_per_incident / max_calls_per_day` 配置项**未透传**（`wireIncidents` 不传） | 改配置无效，永远 3/20 | **未修** | 接线，或删配置项避免误导 |
| 10 | 状态机 `verifying` **不回 diagnosing**；`acting` 未用 | 判据每轮复发时事故卡在 verifying，既不再诊断也不转人工；「每事故 3 次」只在 LLM 调用失败时才耗尽 | **未修** | 补「未恢复且轮次<3 → 带新证据再诊断」转换 |
| 11 | 编排器自身问题（vpsctl 不可用、App 认证失败）的 issue 不会开到 ciorch 仓 | 控制面自身故障只能靠 healthcheck 感知 | **未修** | 低优先级 |
| 12 | `state_dir/cloudinit/` 遗留 user-data 不做 7 天清理 | 含 1h 失效 token，磁盘/信息残留（量小） | **未修** | 加清理 |
| 13 | webhook 无 2s 防抖，只合并触发 | 无实际影响 | **未修**（以代码为准） | 改文档 |
| 14 | `/healthz` 在**任一仓**观察完整时即健康，文档写「全仓」 | 某仓长期观察失败时 healthz 仍 200 | **未修** | 改为全仓或分仓报告 |
| 15 | `ciorch/README.md:13-29` 与 `main.go` usage 停留在「阶段 2」；`cirunner/README.md` 只描述原型 | 误导新读者 | **未修**（本文与 REDESIGN/runbook 为准） | 更新 README，指向本手册 |
| 16 | `deploy/install.sh` `install -d -m 0750 /etc/ciorch`，线上是 0700 | 直接跑 install.sh 会放松目录权限（凭据文件仍 0600） | **未修** | 脚本改 0700 |
| 17 | 示例配置与 runbook 都漏了 `status_token_file`（R8） | 照抄部署则 `/status` 永远 401 | **未修** | 补字段与说明 |
| 18 | healthcheck schedule 实际触发稀疏（11-21 20:26–11-22 01:07 仅见 2 次）；60 天无活动停用 | 宕机外检不是分钟级 | **未修**（平台行为） | 考虑在编排器之外再加一个独立探测，或接受 |
| 19 | 轮询档位缺「有未结事故」一条 | 无实际影响 | **未修** | 改文档 |
| 20 | `create_backoff_max` 的 tuning 不生效：`RecordCreateResult` 被传入空 `decide.Params{}`（engine.go:277、294） | 建机失败退避封顶恒为 15m，改配置无效 | **未修**（代码问题） | 把 engine 的实际 params 传进去，补测试 |
| 21 | REDESIGN §5.4 称机器侧用 `pgrep -x`，实际是 `pgrep -f`（无 shell 包裹，不自匹配） | 文档误导 | **未修** | 改 REDESIGN 注记 |
| 22 | `cirunner diag --lines 200`（空格形式）退出码 2，只有 `--lines=200` 可用 | 人工排障易踩 | **未修** | 改参数分离逻辑或文档 |
| 23 | SSHAct 偶发 `ssh … exit status 255`（11-22 16:44 对 10.104.0.22 一次） | 当次补救未执行；16:59 同两台机器的 `restart-runner` 与事故证据 `machine_diag`（nms#633）均经 ssh 成功，属偶发连接失败 | **观察中** | 复发再查机器侧 sshd 日志（是否公网扫描占满 MaxStartups）；SSHAct 失败目前只记日志（见 #5） |

#### C. 未修：风险（读代码发现，均【推断】）

| # | 风险 | 影响 | 状态 | 建议 |
|---|---|---|---|---|
| R4 | **一次性事故被自动关闭**：B（熔断）、I（streak=3）、F 只在转换瞬间发一次；若同一轮未进入 waiting-human（没配 LLM key，或 LLM 给了合法动作进入 verifying），约两轮不复发就「安静恢复」关 issue，**而熔断可能仍 open** | issue 被关但仍不建机，人以为恢复了 | **未修** | 熔断 open 期间不应安静恢复；或把关闭判据绑定到真实状态（breaker closed） |
| R5 | Gate 占坑句柄失效：`Reservation.id` 是切片下标，下一次 Reserve 的清扫压缩切片 | 多仓并发时 Commit/Abort 可能作用到别的占坑，影响限于 2 分钟 TTL 内的计数偏差 | **未修** | 改用唯一 id |
| R6 | webhook 缓存造成幻影排队：若某 job 的 `completed` webhook 丢了，缓存里残留的 `queued` 条目最长 2h 被当作 Q>0 | 空闲回收被阻止，还可能触发场景 A 重启/回收 | **未修**（#50 只解决跨仓） | 缓存条目以轮询为准做校正，或缩短 TTL |
| R7 | PendingJobs 统计偏大（把 in_progress run 中已 completed 的 job 也算） | 只影响 status 展示 | **未修** | 低优先级 |
| R8 | 见 B-17 | — | — | — |
| R9 | 凭据副本在 WSL：`~/.config/ciorch/app.pem`（0600）、`shadow.token`（0600） | 维护者本机多一份私钥与 PAT | **待维护者决定** | 确认是否仍需要，不需要则删；PAT 作废 |
| R10 | `pause_repo` / `switch_pool` 只在内存里，重启即丢 | 重启后暂停失效 | **未修** | 持久化 |
| R11 | LLM 的 `drain_droplet` 不先确认注销成功就删机 | 与固定逻辑 Drain 口径不同；GitHub 对忙碌 runner 返回 422，注册删不掉但机器照样被删 | **未修** | 对齐固定逻辑 Drain |
| 其他 | `JobCache`、`etags` map 无界增长 | 量级小 | **未修** | 低优先级 |
| 其他 | 沙箱 issue、事故 issue 全部不脱敏、私有仓 | 若将来某托管仓改公开则 issue 公开 | **设计决定** | 改公开前先重评脱敏 |
| 其他 | `runners_per_droplet=2` 代码路径写了但从未真机验证 | 默认关；打开前须在沙箱对比 ≥20 个 run | **未验证** | 维护者决定 |
| 其他 | 多池调度未实现：`profile.pools` 强制长度 1；`switch_pool` 不可用 | 只有一个池可选 | **设计限制** | 见 §12.5 |

### 12.2 观察期结论

观察窗口原定 **2026-11-22 08:16 UTC → 2026-11-23 08:16 UTC**（机制见 §10.10），后改为主动场景测试收口（§8.9）。

  > **观察期结论（2026-11-22 16:10 UTC，协调者）：通过。**
  > - 被动观察：`observe.log` 08:17–14:49 共 14 次采样，healthz 全部 200、熔断全部 closed、机器峰值 2、ciorch 事故 issue 0。脚本于 14:49 因 run 45033783948 结论 failure 触发唤醒条件而退出——经核实是 nms 测试偶发（P145），非执行面问题；按维护者"测试没问题就可以"的决定，不再重启 24h 窗口。
  > - 主动验证：§8.9 六个场景全部符合预期（docs-only 快速放行、全量 7 job、并发上限与排队、cancel-in-progress、45 分钟档回收归零、零事故 issue）。
  > - 当日花费累计 $0.98（远低于 $8 上限）。
  > - 未覆盖：REDESIGN §9 阶段 6 原定"观察 3 天、无未处理事故"，本次以主动场景测试替代时长；后续真实 PR 若暴露问题，按 §10 处理，必要时按 §10.7 回滚。

### 12.3 收尾事项

| # | 事项 | 仓 | 状态 | 链接 |
|---|---|---|---|---|
| 1 | **nms runbook 更新为 ciorch 口径**：`docs/runbooks/gitrunner.md` 现仍写「cirunner 原型按需建机、守护在维护者 WSL」 | nms | 已提 PR | [nms#632](https://github.com/hoshinojian/nms/pull/632) |
| 2 | **nms 决策台账新增一条**：切换到 ciorch 托管。须按台账「取号说明」查末条按序取号，禁止预留编号；引用 #145、#151；说明规格钉死 c2-4vcpu-8gb/sgp1 不变；风险口径 | nms | 已提 PR（决策 #153） | [nms#632](https://github.com/hoshinojian/nms/pull/632) |
| 3 | **nms pitfalls 补记**：原型缺陷 #2（job 永久卡住）；以及 ciorch 侧值得记入的坑（§9）。同时考虑给 2026-11-15「GitHub API/rollup 显示怪象族」条补正式编号（见 §13.4） | nms | 部分完成：新增 P145（retention 夹具偶发）；原型缺陷与「怪象族」补号未做 | [nms#632](https://github.com/hoshinojian/nms/pull/632) |
| 4 | 修正 nms 中过时口径：`nmsdocs/12-大模型开发标准.md` §五（行 114、127）仍写 `gitrunner-sgp1`、机器掉线=CI 停摆、`./internal/...` 覆盖率范围；台账 #145 原文写 `gitrunner-sgp1`/s-2vcpu-4gb 未改 | nms | 部分完成：§五 执行面条目已改；行 114 覆盖率范围与 #145 原文按台账惯例不改（由 #151/#153 修订） | [nms#632](https://github.com/hoshinojian/nms/pull/632) |
| 5 | **原型调度代码退役**：在 cirunner 单独 PR 删除 `internal/{decide,engine,githubapi,vpsctlapi,config}`、`cmd/cirunner` 的 status/plan/up/down/daemon，只留机器侧；同时处理 `internal/cloudinit`（与 `pkg/cloudinit` 的两份） | cirunner | 已提 PR（含 internal/cloudinit、示例配置、systemd 单元） | [cirunner#7](https://github.com/hoshinojian/cirunner/pull/7) |
| 6 | 原型文件清盘（**维护者确认不再需要回滚后**）：`~/.local/bin/cirunner-proto`、`~/.config/systemd/user/cirunner.service`、`~/.config/cirunner/`、`~/.local/state/cirunner/`；注意这些是回滚路径 | 维护者 WSL | 待维护者决定 | — |
| 7 | 现场遗留清理：`~/ciorch-s5` worktree（分支 `fix/incident-review-round1`，已合并）；cirunner 仓 `docs/` 未跟踪的旧 REDESIGN.md（915 行，以 ciorch 仓副本为准）；`ciorch-shadow.service` 单元文件；WSL 上 `shadow.token` / `app.pem` 副本（R9） | 维护者 WSL | 待维护者决定 | — |
| 8 | REDESIGN §9 阶段 6 的收尾描述与本手册对齐；ciorch README 更新（B-15）；runbook/smoke 去掉不存在的命令引用（B-1） | ciorch | TODO | — |
| 9 | 凭据轮换（§10.9）：DeepSeek key、webhook secret | 编排器 + GitHub App | TODO，建议尽快 | — |
| 10 | `deploy/install.sh` 权限改 0700、示例配置补 `status_token_file`（B-16、B-17） | ciorch | TODO | — |

### 12.4 素材冲突与不确定点清单

本文写作时遇到、并如实保留的冲突或不确定点（不抹平）：

| # | 点 | 情况 |
|---|---|---|
| 1 | **P141 编号错位** | REDESIGN（§1.3、§2.2、§3.4.4）与派工里的「P141（run 级判据）」，实指 nms pitfalls 2026-11-15 那条**无编号**的「GitHub API/rollup 显示怪象族」；pitfalls.md 里真正的 P141 是 SNMP 条目，P140 也是 SNMP 条目；S-9997 又称它「P140 显示怪象」。引用时写「pitfalls 2026-11-15『GitHub API/rollup 显示怪象族』条（REDESIGN 记作 P141②）」 |
| 2 | **REDESIGN 副本** | `cirunner/docs/REDESIGN.md` 是未入库的旧副本（915 行，`git status` 显示 `?? docs/`），`ciorch/docs/REDESIGN.md`（951 行，`wc -l` 核实）是权威；以后者为准 |
| 3 | **原型二进制版本** | 已核实：systemd 单元注释仍写「钉死 cirunner@508a0b0」，但二进制 `vcs.modified=true`、mtime 比 53be365 早 9 秒、含 53be365 新增字符串 → 是热补丁工作区构建 |
| 4 | **11-16 `cirunner-team2-ci-sgp1-01` 消失** | 当时推测「用户成本清理删除」，但按原型逻辑更像是正常空闲回收；未查守护日志核实 |
| 5 | **sgp2 停/启沿革** | 11-15 05:25 冒烟时助手停了 sgp2 runner 服务；11-16 用户说「sgp2 是我下掉的」；11-16 10:35 助手重启；11-19 删。「谁下掉的」两种说法并存，不影响结论 |
| 6 | **冷启时长口径** | README/S-14a3 写 4–6 分钟；REDESIGN 实测约 2.3 分钟（建机返回→首 job），从 run 排队算约 3 分钟，切换首跑 2m09s（建机→首 job）。以实测为准 |
| 7 | **费用口径** | $94/月 ≈ $0.139/h（S-14a3 用 $0.14）；RD 预算用 `price_monthly/672`；按需 $28–31/月为 11-15 预估，**未见实际账单核对** |
| 8 | **#145 文本与现实不一致** | 台账 #145 原文写 `gitrunner-sgp1`、s-2vcpu-4gb；实际换成 c2 的 sgp2，runbook 换机沿革有记，台账没改 |
| 9 | **阶段 6 观察期长度** | REDESIGN §9 写「观察 3 天」；协调者切换方案实际用 24 小时；以维护者最终决定为准 |
| 10 | **ciorch / 沙箱可见性** | 已核实（gh，2026-11-22）：ciorch PUBLIC（2026-11-20 07:35:01Z 创建）、ciorch-sandbox PRIVATE（07:35:15Z）、cirunner/vpsctl PUBLIC |
| 11 | **LICENSE 署名** | 用户原话「署名就用 mit」措辞含糊；按 MIT + `Copyright (c) 2026 hoshinojian` 执行；是否明确确认真名还是用户名未见记录 |
| 12 | **「ciorch 公开、沙箱私有」是谁定的** | 助手 11-20 05:54 说「你已经定了 ciorch 公开」，沙箱私有是助手代为决定并留了否决口子；用户那句话不在协调 session 可见消息里，推测用户直接对写码 session 说的 |
| 13 | **`sessionNotFound` 与 content-filter 根因** | 均**不确定**，见 §9.4 |
| 14 | **冒烟 session 创建时刻** | 协调者工具调用里没有；依文本推断是部署 session 约 16:25–17:07 派生 |
| 15 | **S4 / 回归中 runner SSH 255 的原因** | sha 错误按理只让 agent 下载校验失败，编排器却也 ssh 不上；可能 cloud-init 在校验失败后没执行到写入编排器公钥或 sshd 步骤；未查证 |
| 16 | **issue #2 的 SSH 255** | 推断是 -01 已被强删，时间线吻合，未证实 |
| 17 | **healthcheck run 44957950551 / 44958144950 的触发者** | 不在冒烟 session 记录里，推测是协调者并行触发，未核实 |
| 18 | **vpsctl 宿主机与编排器的关联** | 素材未核实 |
| 19 | **V7（webhook 延迟）** | REDESIGN 无结论；切换首跑仅一个样本（约 6 秒，n=1） |
| 20 | **V2（快照建机）** | 未测 |
| 21 | **线上 `ciorch.json` 实际内容** | 本文拼自 04 素材与协调者记录，未读线上文件；`status_token_file`、沙箱池名与容量等需核对 |
| 22 | **DeepSeek key 是否已作废** | 提醒过，用户回复不在可见范围；key 明文仍在两个旧协调 session 记录里 |
| 23 | **PR #28 谁合并的** | 由 hoshinojian 账号 13:21:16Z 合并；S2 说「另一 session 顺带处理了」，两个 session 用同一账号，无法判定谁触发 |
| 24 | **nms #622–#626 合并 commit / vpsctl #32** | 只有 session 自述 + ZCode 确认，未用 gh 单独核对 |
| 25 | **切换首跑 vs 第二跑耗时差** | 14m26s 对 17m17s 来源未拆解 |
| 26 | **素材 04 §0 把沙箱查询时间写作「2026-11-22 08:2x」**，而素材 02/03 把 sandbox issue #8 归在切换后；04 自己标「不在本切片」 | 已在 §8.4 按协调者核实的事实归因（PR #50） |

### 12.5 远期方向

| 方向 | 说明 | 前置 / 风险 |
|---|---|---|
| **阶段 7：快照** | `ciorch bake` + `cirunner` bake 模式，冷启 2.5m → ≤1.5m；约 $0.3/月存储 | 须维护者批准改 vpsctl（snapshot / `images -private` / `image-delete`）；先验 V2；`/var/lib/cirunner/baked` 分支已在 cloud-init 模板里预留；**快照烘焙未实现** |
| **自有机器 Provider**（Incus/Proxmox 等） | 实现新的 `machine.Provider`，契约套件 `machinetest.Run` 必须通过 | **CI 虚拟机必须绑独占 CPU**（`-race` 时敏）；nms 换测量环境后按 #148 口径同口径三连最小值复测覆盖率基线；编排器仍建议留在 DO 小机（持有凭据、需要公网 webhook 入口，不与跑 PR 代码的机器同宿主） |
| **多池调度** | `profile.pools` 长度 >1：「本地优先、DO 溢出」；`switch_pool` 动作才真正可用 | 语义与测试要重新设计；当前强制长度 1 |
| **自有域名** | 替换 sslip.io | V5 已证 sslip.io 可用，非必需 |
| **`runners_per_droplet=2`** | 不加钱翻倍并行 | 须沙箱 `ciorch stats` 对比 ≥20 个 run 稳定性；nms 的 test `-race -p 4` 与 lint/build 抢 CPU 可能击穿时敏用例 |
| **`/ciorch <命令>` issue 评论命令** | REDESIGN 明确「不在本期」 | — |
| **成本复核** | 积累满一个月 DO 账单后核对 §1.4 预估 | — |
| **独立外部探测** | 缓解 healthcheck cron 稀疏 | 如另一家托管 / 其他云上的定时探测 |
| **DeepSeek 以外的模型 / 诊断质量评估** | 目前仅 7 次事故样本 | 材料面是关键（§8.6） |

## 13. 索引

### 13.1 仓库与关键文件路径

**ciorch**（`~/ciorch`，`hoshinojian/ciorch`）

| 路径 | 用途 |
|---|---|
| `docs/REDESIGN.md` | 权威设计（约 950 行，含 §11 V1–V8 实测结论） |
| `docs/PROVIDER-ABSTRACTION.md` | 机器提供商抽象计划（状态：已完成，PR #16–#23） |
| `docs/runbook.md` | 运维手册（App 创建、部署、日常、回滚） |
| `docs/smoke.md` | 沙箱冒烟手册 S1–S8 |
| `docs/HANDBOOK.md` | 本文 |
| `configs/ciorch.example.json` | 示例配置（全部占位符；漏了 `status_token_file`） |
| `deploy/{README.md,install.sh,Caddyfile,ciorch.service}` | 部署产物 |
| `.github/workflows/{ci.yml,healthcheck.yml}` | 自身 CI（托管 runner）与外部健康检查 |
| `cmd/ciorch/main.go` | CLI 入口与装配（blank import 注册 provider） |
| `internal/config` | 配置加载与校验 |
| `internal/githubapp` | App JWT、installation token、REST 客户端、issue API |
| `internal/observe` | 观察口径、JobCache |
| `internal/decide` | 纯决策（`decide.go`、`scenarios.go`、`tuning.go`、`golden_test.go`、`testdata/golden/`） |
| `internal/engine` | 动作执行（`engine.go`、`incident_exec.go`、`renderer.go`） |
| `internal/budget` | 配额与预算 Gate |
| `internal/incident`、`internal/llm` | 事故状态机、诊断材料、校验、DeepSeek 客户端 |
| `internal/machine`、`internal/provider/vpsctl` | Provider 接口、契约套件、vpsctl 实现 |
| `internal/{daemon,server,state,stats}`；`cmd/ciorch/main.go` 中的 `sshx` 类型 | 守护、HTTP、持久化、统计；`sshx` 是 main 包里的 ssh 封装（Diag/Act，调系统 ssh），不是独立包 |

**cirunner**（`~/cirunner`，`hoshinojian/cirunner`）

| 路径 | 用途 |
|---|---|
| `pkg/cloudinit/cloudinit.go`、`testdata/golden.yaml` | cloud-init 渲染与校验（在役，ciorch 依赖） |
| `cmd/cirunner/machine.go`、`internal/{health,diag,act,sys}` | 机器侧 CLI（在役） |
| `.github/workflows/{ci.yml,release.yml}` | 自身 CI 与发布（tag `v*`） |
| `cmd/cirunner/main.go`、`internal/{decide,engine,githubapi,vpsctlapi,config,cloudinit}` | 原型调度器（已停用，回滚路径，待退役） |
| `docs/REDESIGN.md` | 未入库的旧副本（以 ciorch 仓为准） |

**vpsctl**：`~/vpsctl`（公开，MIT；账号文件 `~/.config/vpsctl/accounts.json` 含全部账号，不进库）。

**nms**（`~/NMS2`，`hoshinojian/nms`）：`.github/workflows/ci.yml`（7 个 job，均 `[self-hosted, ci]`）、`.github/coverage-baseline`（89.1）、`docs/runbooks/gitrunner.md`、`docs/engineering/pitfalls.md`、`nmsdocs/00-导读与决策记录.md`（决策台账）、`nmsdocs/12-大模型开发标准.md`（§五含过时口径）、`AGENTS.md`。

**编排器机器**：`/etc/ciorch/`（配置与凭据，0700）、`/var/lib/ciorch/`（状态）、`/usr/local/bin/{ciorch,vpsctl}`、`/usr/local/bin/ciorch.bak-<sha12>`（旧二进制备份）、`/etc/systemd/system/ciorch.service`、`/etc/caddy/Caddyfile`。外部地址 `https://168-144-104-79.sslip.io`（`/healthz`、`/status`、`/webhook`）。

**维护者 WSL**：`~/.local/bin/cirunner-proto`、`~/.config/systemd/user/{cirunner,ciorch-shadow}.service`、`~/.config/cirunner/cirunner.json`、`~/.local/state/cirunner/states.json`、`~/.config/ciorch/{app.pem,shadow.token}`（副本，见 §12.1 R9）、`~/.local/state/ciorch-observe/{observe.sh,observe.log}`、`~/ciorch-s5`（遗留 worktree）。

**素材与会话库**：`/tmp/cutover/notes/01-origins.md … 05-architecture.md`（临时目录，可能被清理）；ZCode 会话库 `~/.zcode/cli/db/db.sqlite`（只读打开可还原各 session 全文，内含明文凭据，勿外传）。

### 13.2 PR 编号表

**ciorch #1–#54**（#45 是 S8 的 issue，不是 PR）

| PR | 内容 |
|---|---|
| 首提交 `1d72f0a` | 骨架 + `ciorch stats`（直推空仓 main，审查认可为例外） |
| #1 | 冷启代理按 runner_id；`-config` 两段形式 |
| #2 | 阶段 1 审查 4 处（重跑排除、run_started_at、无子命令 panic、repos 对象 schema） |
| #3 | config 完整 schema + observe |
| #4 | decide（场景 A/B/C/D/E/G/H/I、熔断、扩缩容、建机退避） |
| #5 | budget + vpsctlapi + state + engine |
| #6 | daemon + githubapp 重试/ETag/Retry-After |
| #7 | dry-run 真透传；观察不完整整轮冻结 |
| #8 | D 按 job 判、Bad 幂等、半开只试探一台、Drain 失败后重置、替换机看 Q、补回 RemoveReg |
| #9 | Engine.mu + StatesSnapshot；budget.Gate 占坑 |
| #10 | 顺手修四处 + 口径回写 |
| #11 | Decide 入口深拷贝；R7；预算持久化；Gate TTL |
| #12 | runners labels 是对象数组 |
| #13 | 小时用量统计 |
| #14 | V3：304 不计额度 |
| #15 | MIT |
| #16 | PROVIDER-ABSTRACTION 计划文档（⓪） |
| #17 | `internal/machine` + 契约套件 + Fake（①） |
| #18 | `internal/provider/vpsctl`（②） |
| #19 | decide golden 夹具（②b） |
| #20 | 全部切到 `machine.Provider`，CI 加 import 边界检查（③，关键路径，`5d6a190`） |
| #21 | 池化配置 + 按池/按小时成本（④） |
| #22 | 文档同步（⑤） |
| #23 | cmd 补 blank import 注册 vpsctl provider |
| #24 | V1/V6 实测回写 |
| #25 | 接入 cirunner `pkg/cloudinit` 渲染器（钉 v0.1.3） |
| #26 | go.mod 去 indirect + CI tidy 门禁；§5.4 补 pgrep 自匹配 |
| #27 | GitHub App 认证 |
| #28 | 抽象计划收尾（`hourly_cost_hint`） |
| #29 | `internal/server`：/webhook /healthz /status |
| #30 | `internal/llm`：DeepSeek 客户端与提示词 |
| #31 | `deploy/` |
| #32 | healthcheck.yml |
| #33 | githubapp issues.go / diagnostics.go |
| #34 | docs/runbook.md |
| #35 | incident 状态机 |
| #36 | 诊断轮（证据采集、白名单校验、限额） |
| #37 | 事故管线接线（含 stash 事故） |
| #38 | docs/smoke.md |
| #39 | 审查修复（webhook secret 换行、issues.closed 接线、自关 issue 过滤、IssueRepo 匹配） |
| #40 | runbook 部署实测回写（V5/V8、user-data 坑） |
| #41 | V4 DeepSeek JSON 模式回填 |
| #42 | 公钥按行 trim |
| #43 | 同轮多个 create 重名 |
| #44 | `global.tuning` 接通 |
| #46 | smoke.md S1/S2 回填（其中「45m 符合设计」后被 #49 纠正） |
| #47 | LastActivity 按 RunID 跨轮去重 |
| #48 | 熔断仓级单发、LLM 动作目标校验、诊断材料补齐 |
| #49 | healthcheck 红叉 + 自建 label；runbook/smoke 纠正 |
| #50 | JobCache 按仓过滤 + 加锁（部署 `e40767506aee`） |
| #51 | ETag 缓存加 `etagMu`（部署 `5761d103e977`） |
| #52 | plan/status/dry-run 事故模块只读（部署 `d5ace1280b26`） |
| #53 | diag exit 1 且 JSON 合法时保留输出（同批） |
| #54 | stats 优先 App 认证（部署 `f652ebce1b48`） |

**cirunner #1–#6，tag v0.1.0–v0.1.3**

| PR | 合并 | 内容 |
|---|---|---|
| #1 | `bf0c615` | 原型热补丁（commit 53be365：in_progress run 内 queued job、30m 冷却试探、`filter=latest`） |
| #2 | `cb24742` | MIT LICENSE |
| #3 | `980e572` | `pkg/cloudinit` + health/diag/act + release workflow → v0.1.0 |
| #4 | `ff6d590` | runner 版本改 `config.sh --version` → v0.1.1 |
| #5 | `3858e90` | act flag 后置 → v0.1.2 |
| #6 | `b0225ce` | 复审修正、cirunner CI、action 钉 SHA → v0.1.3 |

原型时代提交：b1ba8bb（初始）、47221cc、508a0b0（root 密码过期修复，原型钉死版本）。

**相关 nms PR / 决策 / pitfalls**

| 类别 | 编号 | 内容 |
|---|---|---|
| PR | #590、#591 | 迁自建 runner；gitrunner runbook |
| PR | #598、#599 | P139 记录；基线 89.2→89.1（#148） |
| PR | #606 | pitfalls 补「GitHub API/rollup 显示怪象族」（无编号条目） |
| PR | #612、#613、#615 | P142 恢复竞态；`-p 1`→`-p 4`；P143 SNMP 管道超时 |
| PR | #622 | runbook 改写 + 决策 #151 |
| PR | #623 | ci.yml 7 个 job 加 `timeout-minutes` |
| PR | #624 | `core.quotepath=false` + P144 |
| PR | #625 | 决策 #150 撞号处置，测试并行化改号 #152 |
| PR | #626 | docs-only 验证 PR（`docs_only=true`，test 48s） |
| 决策 | #143 / #144 | docs-only 快速放行 / 本地门禁降级 |
| 决策 | #145 | 执行面迁自建 runner |
| 决策 | #148 | 覆盖率基线 89.1，换环境须三连最小值复测 |
| 决策 | #150 / #152 | 台账 #150 为 NetFlow 国家归属（非 CI）；测试并行化为 #152 |
| 决策 | #151 | sgp2 退役、按需执行面、规格钉死 |
| pitfalls | P3 / P70 / P75 | WSL 出站 22 阻拦（已被 V8 修正）/ vpsctl user-data 三连坑 / user-data 纯 ASCII |
| pitfalls | P23 / P63 | 迁移锁 `-p 1`，已由 #152 解除 / 根因改判 |
| pitfalls | P97 / P102 / P46 | 覆盖率基线口径相关 |
| pitfalls | P121 / P136 | fork PR / 分支 push 后 workflow 不触发 |
| pitfalls | P139 | 自建 runner 静默停摆 |
| pitfalls | P142 / P143 / P144 | 恢复竞态 / SNMP 超时 / quotepath |

### 13.3 session 一览

| 简称 | 完整 id | 标题 / 职责 | 时间（UTC） |
|---|---|---|---|
| S-e241 | `sess_e2419262-f984-444b-809d-86fd72e1de4e` | 「启动 vpsctl 查看状态」 | 11-13 |
| S-14a3 | `sess_14a3cb6f-7f3b-4a5e-bf24-949666265436` | 「询问CI服务器运行状态」，cirunner 原型诞生 | 11-15 |
| S-702a | `sess_702a60de-5b1d-4049-9936-6a29c73c721e` | 已归档，只有用户一句提问（串行慢） | 11-16 |
| S-9997 | `sess_9997d6e5-b1fd-453c-8ece-bcd13b4473bb` | 「检查CI服务器运行任务与性能优化」，双 runner 与 `-p 4` | 11-16 |
| S-a171 | `sess_a171d050-e4f4-45c2-8b96-e8bb10d33c7b` | 「部署vpsctl到服务器」 | 11-17 |
| S-a68a | `sess_a68a5090-03e8-4e38-b3ca-690aa83aabb5` | 原协调 session：设计、审查、派工；11-22 07:14 起被 content-filter 拦截，已归档 | 11-19 16:13 → 11-22 07:54 |
| 协调分叉 | `sess_55ccca3a-8211-446f-859b-8afc724d7580` | S-a68a 的前缀副本，无独立内容 | 11-19 → 11-21 |
| 写码 A（S1） | `sess_2e223496-ee77-4ca5-a1c9-81e731576c17` | 「按REDESIGN.md需求执行重构」：阶段 0a–4 代码与冒烟准备 | 11-20 04:36 → 11-21 14:05 |
| 写码 B（S2） | `sess_60855587-7e55-4b21-aba7-bda9339ef529` | 「ciorch 机器提供商抽象」+ 阶段 5 事故管线 | 11-21 10:46 → 14:09 |
| 部署（D） | `sess_2c920fad-58de-4573-9120-8e66d212778c` | 「ciorch 部署（编排器机器 + 沙箱冒烟）」 | 11-21 15:39 → 16:26 |
| 冒烟（S） | `sess_1f08eb65-0456-498a-8405-98fd607b7829` | 「ciorch 冒烟 S1–S8（沙箱）」，冒烟、修复、回归 | 11-21 约 16:25 → 11-22 01:25 |
| 新协调 | `sess_0afeb411-2b87-4d01-a73e-bbbd8c55ed6c` | 11-22 接手：亲自执行 nms 切换、修 #50–#54、起草本文、收尾 PR（nms#632、cirunner#7） | 11-22 07:14 起 |
| 场景测试 | `sess_4a544768-7985-435c-b212-31735c34ae0d` | 「nms CI 切换 ciorch 后真实 PR 场景测试」 | 11-22 13:45 → 16:03 |

注意：§8.4 的 S1–S8 是**沙箱冒烟场景**，§11 里的 S1/S2 是**写码 session**，二者无关。

### 13.4 术语表

| 术语 | 含义 |
|---|---|
| Q | 需求：Matchable 且 queued 的 job 数 |
| QAge | Q 中最老 job 的排队年龄 |
| I | 所辖、online、空闲、非 Bad 的 runner 数 |
| P | 冷启在途供给：未 online、年龄未超时、非 Bad 的机器数 × runners_per_droplet |
| N | 本仓现有机器数（含 Bad 未删） |
| Matchable | job labels（小写）⊆ `{self-hosted,linux,x64}` ∪ profile.labels |
| UnmatchedQueued | 不可匹配的 queued job（场景 E 的证据，不计入 Q） |
| 场景 A | 假活（P139：online、空闲、不接活） |
| 场景 B | 冷启失败（机器年龄 ≥ 8m 仍未 online），累计触发熔断 |
| 场景 C | 曾 online 后掉线（连续 2 轮 offline） |
| 场景 D | job 卡死（60m 告警、90m 强删） |
| 场景 E | label 无人能满足 |
| 场景 F | 配额/预算用尽 |
| 场景 G | 外部 runner 假活（只告知，不操作，不调 LLM） |
| 场景 H | 观察失败（连续多轮 Complete=false） |
| 场景 I | 建机报错（退避，streak=3 发事故） |
| 场景 J | 孤儿机器（未接线） |
| Drain | 先注销 runner 注册，成功后才删机（防空闲删机竞态，依赖 V1） |
| ForceDelete | 先删机再删注册（判死的机器） |
| Bad / Remedy | 机器已判死 / 正在进行的补救（restarting、replacing、draining） |
| Breaker | 熔断器：closed / open / half-open |
| 冻结 | 观察不完整时整轮不建、不删、不 ssh、不推进计数（只累计 H） |
| RunnerName / 命名空间 | 名字匹配池 Namer Pattern 才算本仓所辖，其余视为外部 runner，绝不触碰 |
| 池 / profile / repo | 池 = 同来源同规格机器；profile = runner 形态 + 用哪个池；repo = 托管仓及上限 |
| V1–V8 | REDESIGN §11 待实测项（见 §8.3） |
| S1–S8 | 沙箱冒烟场景（见 §8.4） |
| R1–R11 | 读代码发现的风险（§12.1；R1–R3 已由 #52、#51、#53 修；R7 为 decide 内部的 Drain 注销后误记掉线，已由 #11 修，与 §12.1 的 R7「PendingJobs 偏大」同号不同事，注意区分） |
| R1–R6（阶段 2 复现） | 阶段 2 审查时协调者写的临时复现场景（与上一行的 R* 编号体系不同，仅在 §7/§11 出现） |
| D1–D18 | 本文 §3 内部决策编号，不是 nms 台账编号 |
| 阶段 0a–7 | REDESIGN §9 的实施阶段（0a 原型热补丁 … 7 快照） |
| shadow | `ciorch daemon -dry-run` 与在役原型并跑，只观察不执行 |
| 沙箱 | 私有仓 `hoshinojian/ciorch-sandbox`，故障注入与冒烟专用 |
| ZCode | 审查/调度方在 session 中的自称（协调者的消息经它转达） |
| P139 | nms pitfalls：自建 runner 静默停摆 |
| **P141 编号说明** | REDESIGN 与多处派工里的「P141（run 级判据）」实指 nms pitfalls 2026-11-15「GitHub API/rollup 显示怪象族」那条**无编号**条目（pitfalls.md 约 913 行）；pitfalls.md 里真正的 P141 是 SNMP 条目，P140 也是 SNMP；S-9997 又称它「P140 显示怪象」。建议引用时写全名，或日后给该条补正式编号（§12.3 第 3 项） |
| #150 撞号 | 台账 #150 = NetFlow 国家归属；测试并行化原编 #150，顺延为 #152 |
| `.bak-<sha12>` | 编排器上旧二进制备份命名约定 |
| 观察期 | 2026-11-22 08:16 → 11-23 08:16 UTC，见 §12.2 |

--- 手册结束。 ---
