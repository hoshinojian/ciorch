# 机器提供商抽象：实施计划

> 版本：2026-11-21 v1（ZCode 起草，维护者定方向）。
> 读者：接手实现的人/大模型。实现完交 ZCode 审查，审查口径见 §8。
> 上位文档：[REDESIGN.md](REDESIGN.md)。本计划改动其 §2.2、§3.3、§3.5、§3.7、§3.8、§3.10.5、§4，
> 实现时同步修改那几节（§7 列出），两份文档不得互相矛盾。

---

## 0. 一句话

把 ciorch 里"机器从哪来"这件事收进一个 `Provider` 接口，vpsctl（DigitalOcean）作为第一个、
目前唯一的实现。**本次是纯重构：行为零变化，不新增任何提供商。**

## 1. 动机与边界

**动机**：维护者以后可能有自己的高性能机器（自建虚拟化，如 Incus/Proxmox），希望
ciorch/cirunner 能迁过去，或者"本地优先、DO 溢出"混用。短期内仍只用 vpsctl + DO。
趁阶段 3/4 还没把建机路径写得更深，现在理干净成本最低。

**做**：
- 定义提供商无关的机器模型和接口；vpsctl 实现迁到接口后面。
- 配置拆成"池"（pool：一组同来源同规格的机器）与"profile"（runner 长什么样）。
- 预算/配额改为"按池容量 + 按小时成本"，成本由提供商给出（自有机器可以是 0）。
- 命名与归属规则由提供商提供（vpsctl 的命名格式是它自己的约束，不是 ciorch 的）。
- import 边界：除提供商实现包外，任何包不得 import `vpsctlapi`。

**不做**（写进 PR 描述的"非目标"，审查时会核对没有越界）：
- 不实现 Incus/Proxmox 等任何新提供商。
- 不实现多池调度（"本地满了溢出到 DO"）——只把 schema 留好，校验限制每个 profile 只能挂 1 个池。
- 不改 vpsctl 仓。
- 不改 decide 的任何决策语义、阈值、场景矩阵。
- 不改 cirunner 仓。

## 2. 现状：耦合点清单（2026-11-21，ciorch main 7e33ef9）

| 位置 | 耦合 |
|---|---|
| `internal/vpsctlapi` | vpsctl 子进程封装（本身没问题，它就是实现） |
| `internal/observe/observe.go` `VPSSource.ListDroplets(tag, account)` | 接口签名带 DO 的 account 概念；类型名 `Droplet` |
| `internal/observe/adapter.go` | vpsctlapi → observe 的适配器 |
| `internal/engine/engine.go` | `VPSFace` 直接用 `vpsctlapi.CreateOpts/Droplet`；建机拼 Account/Region/Size/Image；`Delete(account, id)`；`nameIndex` 依赖 vpsctl 命名格式；`ScanOrphans` 按 account 列机 |
| `internal/budget/budget.go` | `Lister` 返回 `vpsctlapi.Droplet`；成本按 `PriceMonthly/672`（DO 月封顶口径）；全局计数按 account 去重 |
| `internal/config/config.go` | `Profile` 里 account/region/size/image/snapshot/fallback_regions 是 DO 概念；顶层 `vpsctl_bin`、`vpsctl_accounts_file`；`global.max_droplets`；`NamespacePattern(p)` 拼 `{prefix}-{account}-{region}-NN` |
| `internal/decide` | `NextName(prefix, account, region, …)`；类型用 `observe.Droplet`（语义上无关，但名字和命名规则带 DO 味） |
| `cmd/ciorch/main.go` | 直接构造 `vpsctlapi.Client`、按 account 收集列表 |

decide 的决策逻辑本身已经与提供商无关（只看名字、创建时间、注册状态），这是本次能做成纯重构的前提。

## 3. 目标设计

### 3.1 机器模型与接口（新包 `internal/machine`）

```go
package machine

// Machine 是提供商无关的机器视图（替代 observe.Droplet / vpsctlapi.Droplet 在上层的使用）。
type Machine struct {
    Pool       string        // 所属池名（配置里的 key）
    ID         string        // 提供商内唯一 ID
    Name       string        // = runner 名
    CreatedAt  time.Time     // 冷启/寿命/成本计时起点
    Addr       string        // 编排器 ssh 可达的地址（提供商决定用私网还是公网）；可空
    HourlyCost float64       // USD/小时；自有机器为 0
    Tags       []string      // 归属标记（公共标记 + 仓标记）
}

// CreateRequest 是建机请求。名字由 ciorch 先定好（= runner 名，cloud-init 里已写死），
// 提供商必须原样使用这个名字，做不到就返回错误。
type CreateRequest struct {
    Name     string
    Tags     []string
    UserData []byte   // 渲染好的 cloud-init
}

// Provider 是一个池背后的机器来源。实现必须并发安全。
type Provider interface {
    // Kind 返回实现类型（"vpsctl"），用于日志与 status。
    Kind() string
    // Create 建一台机器并立即返回（不等就绪；就绪以 GitHub 侧 runner online 为准）。
    // 返回的 Machine 至少要有 ID、Name、CreatedAt。
    Create(ctx context.Context, req CreateRequest) (Machine, error)
    // Delete 删机。机器已不存在时返回 nil（幂等）。
    Delete(ctx context.Context, id string) error
    // List 列出带指定标记的全部机器。任何部分失败都必须返回 error，
    // 不得返回不完整的列表（observe 的"观察冻结"依赖这一点）。
    List(ctx context.Context, tag string) ([]Machine, error)
    // Namer 返回本池的命名规则（见 3.2）。
    Namer() Namer
}
```

### 3.2 命名与归属

现在的命名 `ci-<repo>-<account>-<region>-NN` 是 **vpsctl 的约束**（它只接受前缀 + 起始序号，
自己拼出 `{prefix}-{account}-{region}-NN`），不应该写死在 config/decide 里。改为由提供商提供：

```go
type Namer interface {
    // Next 在现有机器名里取下一个名字（现有 NextName 的跳号语义：最大序号 +1）。
    Next(prefix string, existing []string) string
    // Pattern 返回本池里"属于这个前缀"的名字正则（替代 RepoEntry.NamespacePattern）。
    // rpd=2 时 runner 名会带 -a/-b 后缀，Pattern 只描述机器名，后缀匹配仍在 decide 里做。
    Pattern(prefix string) string
}
```

- vpsctl 实现：完全沿用现在的格式与正则，结果必须逐字节一致（§6 有对应测试）。
- 仓标记（`ciorch-r-…`）、公共标记（`ciorch`）、编排器自身标记（`ciorch-host`）的规则不变，
  仍由 config 生成，所有提供商都必须支持"按标记列机"。没有原生 tag 的平台（以后的本地虚拟化）
  由实现自己映射（如写进实例元数据），这不是本次的事。
- decide 不再调用 `NextName(prefix, account, region, …)`；`Params` 里新增 `Namer`（或由 engine
  算好名字后传入），decide 保持纯函数、不做 IO。

### 3.3 配置 schema（破坏性变更，一次到位）

```json
{
  "pools": {
    "do-sgp1-c2": {
      "provider": "vpsctl",
      "max_machines": 2,
      "vpsctl": {
        "bin": "/usr/local/bin/vpsctl",
        "accounts_file": "/etc/ciorch/vpsctl-accounts.json",
        "account": "team2-ci",
        "region": "sgp1",
        "size": "c2-4vcpu-8gb",
        "image": "ubuntu-24-04-x64",
        "snapshot": "",
        "prefer_private_ip": true
      }
    }
  },
  "profiles": {
    "go-heavy": {
      "pools": ["do-sgp1-c2"],
      "labels": ["ci"],
      "runners_per_droplet": 1
    }
  },
  "global": {
    "max_machines": 2,
    "daily_budget_usd": 5.0
  }
}
```

要点：
- **池（pool）**：`provider` 选实现，同名子对象放该实现的私有配置（只有对应实现解析，其他字段一律拒绝）。
  `max_machines` 是池容量上限（自有机器的物理上限；DO 池可以等于全局上限）。
- **profile**：只描述 runner（labels、每机 runner 数）和用哪些池。`pools` 是**有序列表**，为将来
  "本地优先、DO 溢出"预留；**本次校验强制长度为 1**，错误信息写明"多池调度尚未实现"。
- 顶层 `vpsctl_bin`、`vpsctl_accounts_file` 移入池配置；`global.max_droplets` 改名 `global.max_machines`。
- `fallback_regions` 删除：它是 REDESIGN §3.10.5 大模型 `change_region` 动作的白名单。该动作改为
  `switch_pool`，白名单 = profile 的 `pools` 列表（本次只改文档 §3.10.5；事故管线阶段 5 按新口径实现）。
- 校验：池名、provider 已注册、私有配置齐全、每个 profile 至少 1 个池且池存在、
  `global.max_machines ≥ 任一仓 max_runners`、`Σ 池容量` 与全局上限的关系不强制。
- **不做向后兼容的旧格式解析**：现存配置只有 shadow 用的一份。`check-config` 遇到旧字段
  （`vpsctl_bin`、`max_droplets`、profile 里的 `account` 等）时给出明确的迁移提示，指向本文 §3.3。
  仓内 `configs/` 示例同步改，全部用占位符。

### 3.4 预算与配额

- 成本：`Machine.HourlyCost` 由提供商给出。vpsctl 实现按现在的口径 `price_monthly / 672` 计算；
  `budget` 包里不再出现 672 和 `PriceMonthly`。
- 建机前的预算投影："新机 1 小时"的成本取该池最近一台机器的 HourlyCost；池里没有机器时，
  由提供商给一个估计值（vpsctl：按 size 查不到就保守地用池里历史最大值，没有历史就拒绝并报配置错误——
  不能当成 0，否则 DO 池会绕过预算）。为此接口增加 `EstimateHourlyCost(ctx) (float64, error)`，
  或者放进池配置 `hourly_cost_hint`——二选一，PR 里说明理由。
- Gate（Reserve/Commit/Abort 占坑模型不变）同时检查三件事：全局 `max_machines`、**目标池**的
  `max_machines`、每日预算。计数改为对每个池 `List(公共标记)` 后按 (池, ID) 去重，不再按 account 去重。
- 持久化的当日花费（state.json）字段名不变或做一次迁移读取，保证升级当天不清零。

### 3.5 各包改造

| 包 | 改造 |
|---|---|
| `internal/machine`（新） | 3.1/3.2 的类型与接口；`Registry`（provider 名 → 构造函数）；`machinetest` 契约测试套件（§6） |
| `internal/provider/vpsctl`（新，由 `internal/vpsctlapi` 迁入） | 子进程封装 + 实现 `machine.Provider`；`Create` 内部把 `Name` 解析回 prefix + start-index 调 vpsctl，解析失败报错；`Delete` 对"已不存在"返回 nil（现在的 vpsctl 返回什么要先实测确认） |
| `internal/observe` | `Droplet` → `machine.Machine`；`VPSSource` 换成"按池列机"；删 `adapter.go` |
| `internal/decide` | 类型改名；`NextName` 移入 vpsctl Namer；`Params.OwnedRunnerRe` 改由池的 Namer.Pattern 生成。**决策逻辑一行不改** |
| `internal/engine` | `VPSFace` 删除，改持有 `map[池名]machine.Provider`；建机/删机/孤儿扫描都按机器的 `Pool` 路由；SSH 用 `Machine.Addr` |
| `internal/budget` | 3.4；`Lister` 删除 |
| `internal/config` | 3.3 |
| `cmd/ciorch` | 按配置从 Registry 构造各池 provider；`status` 输出里显示池名和 provider 类型 |

依赖方向：`machine` 不依赖任何业务包；`provider/vpsctl` 只依赖 `machine`；业务包只依赖 `machine`。

### 3.6 import 边界（CI 强制）

ciorch 没有 golangci 配置，用一个最简单的检查加进 CI 的 lint job：

```sh
# 除提供商实现外，任何包不得 import vpsctl 实现
! go list -f '{{.ImportPath}}: {{join .Imports " "}}' ./... \
  | grep -v '^github.com/hoshinojian/ciorch/internal/provider/' \
  | grep -v '^github.com/hoshinojian/ciorch/cmd/' \
  | grep 'internal/provider/vpsctl'
```

`cmd/ciorch` 允许 import（它负责注册实现）。

## 4. PR 拆分（顺序执行，每个单一目的、draft、五段描述、托管 CI 绿后可自行合并）

| # | 内容 | 验收 |
|---|---|---|
| ① | 新增 `internal/machine`（类型、接口、Namer、Registry）+ `machinetest` 契约测试套件 + fake provider。**不改任何调用方** | 契约套件对 fake 全绿 |
| ② | `internal/vpsctlapi` 迁为 `internal/provider/vpsctl` 并实现接口；契约套件对它跑（用注入的假子进程，不碰真 DO）；Namer 与旧 `NextName`/`NamespacePattern` 的逐字节一致性测试。旧包暂留，调用方不动 | 契约 + 一致性测试全绿 |
| ③ | observe/decide/engine/budget/daemon/cmd 全部切到 `machine.Provider`；删除旧 `vpsctlapi` 包和 observe 适配器；加 §3.6 的 import 检查 | 全量 `-race` 绿；§5 的行为不变验证 |
| ④ | 配置 schema（§3.3）+ 预算按池/按小时成本（§3.4）+ `check-config` 迁移提示 + 示例配置 | 新旧配置各一份的加载测试；预算测试改用 HourlyCost |
| ⑤ | 文档：REDESIGN §7 列出的各节 + ciorch README + runbook 里的配置示例 | 两份文档无矛盾（审查逐节核对） |

③ 可能超过 400 行（主要是改名）。如果超了，允许把"纯改名"单独拆成一个 PR，PR 描述里注明"仅改名、无逻辑变化"。

## 5. "行为零变化"怎么证明

1. **决策回放对比**：在 PR ③ 之前，写一个测试夹具，把一组有代表性的观察序列（空闲、扩容、A/B/C/D 各场景、
   观察不完整、半开熔断，可直接复用 decide_test 里的场景）跑一遍，把每轮输出的动作列表和状态序列化存成
   golden 文件。③ 之后同一组输入的输出必须与 golden **完全一致**（允许的差异仅限类型改名导致的字段名）。
2. **真实环境 dry-run**：③ 和 ④ 合并后，用新配置格式重跑 `ciorch plan` 与 `ciorch status`，输出与改造前一致；
   再启动 shadow（`ciorch-shadow.service`，dry-run）跑 1~2 小时，期间手动触发 nms CI 一次，确认 Q 序列与
   计划建机数和 2026-11-20 的 shadow 记录一致。shadow 用的仍是维护者个人 token，跑完即停。
3. 全量 `go test -race -count=1 ./...`、`go vet`、`gofmt -l`。

## 6. 测试要求

- **契约测试套件**（`machinetest.Run(t, newProvider)`，所有现在和将来的提供商都必须通过）：
  建机后能按标记列出且名字原样；不带该标记的机器不出现；删机后列不出；重复删除返回 nil；
  `List` 遇到部分失败必须返回 error（不得返回半截列表）；并发 Create/List 在 `-race` 下无竞态。
- **vpsctl 实现**：Namer 与旧实现逐字节一致（跳号、外部前缀、不同区域互不占号、rpd=2 后缀）；
  `Create` 传给 vpsctl 的参数与旧实现一致（不带 `-wait`、`-only`、`-start-index`、`-tags`、`-user-data`）；
  名字解析失败报错；HourlyCost = price_monthly/672。
- **config**：新格式全字段校验；旧字段给出迁移提示；profile 挂 2 个池时报"多池调度尚未实现"。
- **budget**：按池容量拒绝、按全局拒绝、按预算拒绝三种分别有用例；HourlyCost=0 的池不消耗预算；
  空池预估成本拿不到时拒绝建机（不当 0）。
- **engine**：两个池时建机/删机路由到正确的 provider（用两个 fake）；孤儿扫描覆盖所有池。

## 7. REDESIGN.md 需要同步修改的章节

- §2.1 架构图：`vpsctl 子进程` 改为 `Provider（当前：vpsctl/DO）`。
- §2.2 原则：新增一条"机器来源经 Provider 接口，业务包不感知具体提供商"。
- §3.2 仓布局：加 `internal/machine`、`internal/provider/vpsctl`，删 `internal/vpsctlapi`。
- §3.3 配置：整节按本文 §3.3 重写。
- §3.5 命名与归属：命名格式改为"由池的 Namer 决定，vpsctl 池为 …"。
- §3.7 预算：按本文 §3.4。
- §3.8 执行：建机/删机按池路由。
- §3.10.5：`change_region` → `switch_pool`，白名单改为 profile.pools。
- §4 与 vpsctl 的契约：改为"vpsctl 提供商实现的契约"，内容不变。

## 8. 审查清单（交 ZCode 时逐条自查，对照代码，不凭印象）

- decide 包的 diff 只有改名和 Namer 注入，决策逻辑零改动；golden 回放完全一致。
- 除 `internal/provider/*` 和 `cmd/` 外没有任何包 import vpsctl 实现（CI 检查在跑）。
- 契约测试套件存在，vpsctl 实现与 fake 都通过。
- 每个删除路径仍经过归属判定（仓标记 + Namer.Pattern），外部 runner、孤儿机器、`ciorch-host` 不被删的测试仍在且通过。
- 预算：DO 池的成本口径与改造前一致；HourlyCost 缺失时不会被当成 0。
- 配置示例与测试数据无真实凭据。
- 没有越过 §1 的"不做"清单。

## 9. 与其他工作的关系

- **阶段 3（cirunner 机器侧重写）**主要在 cirunner 仓，可以并行；但 ciorch 里"接入 cloud-init 渲染器"
  那一小步（engine 的建机路径）要等本计划 PR ③ 合并后再做，避免两边同时改 engine.go。
- **阶段 4（上云 + GitHub App + webhook）**在本计划完成之后开始，部署配置直接用新 schema。
- 将来真加本地虚拟化提供商时，需要另外考虑（不在本计划内）：CI 虚拟机绑独占 CPU（`-race` 时敏用例，
  nms runbook 共享核教训）；nms 换测量环境后按决策 #148 的口径复测覆盖率基线；编排器仍建议留在 DO 小机上
  （持有凭据、需要公网 webhook 入口，不与跑 PR 代码的机器同宿主）；多池调度（profile.pools 长度 > 1）的语义与测试。
