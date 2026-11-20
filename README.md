# ciorch — CI 执行面编排器

给我的所有 GitHub 项目按需起 CI 机器：常驻一台小机（不跑任何 CI），观察排队 →
建/删 DigitalOcean runner 机器（经 vpsctl）→ 自愈 → 出事开 issue 并调用大模型诊断。
机器侧配套见 [cirunner](https://github.com/hoshinojian/cirunner)（cloud-init 配方 + 机器上的
`cirunner health/diag/act`）。

完整设计（唯一依据）：[docs/REDESIGN.md](docs/REDESIGN.md)。

## 当前状态（阶段 2 完成）

决策核心（observe/decide/budget/engine/state/daemon）已落地并经单测覆盖；
`plan`/`status`/`daemon -dry-run` 可对真实仓只读运行（建机执行面待阶段 3
cirunner `pkg/cloudinit` 接入后启用）。事故管线与 webhook 在阶段 4/5。

```
ciorch [-config path] stats  [-repo owner/name] [-since 7d]   基线统计
ciorch [-config path] plan   [-repo owner/name]               观察+决策，打印本轮动作（不执行）
ciorch [-config path] status [-repo owner/name]               观察各仓 Q/runner/droplet
ciorch [-config path] daemon [-dry-run]                       守护循环（-dry-run = shadow 模式）
```

token 解析顺序：`-token` > `CIORCH_TOKEN` > 配置 `token_file` > `gh auth token`
（临时 token 源；GitHub App 认证阶段 3 落地）。

## 开发

```
make lint    # gofmt + go vet（+ golangci-lint，未装则跳过）
make test    # go test ./...
make build   # 构建到 bin/ciorch
```

纪律：文档中文，代码/标识符/JSON 字段英文；conventional commits（中文描述）；
单一目的 PR，draft 打开，描述五段（变更/验证/风险/测试/关联）。
