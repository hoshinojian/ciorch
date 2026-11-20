# ciorch — CI 执行面编排器

给我的所有 GitHub 项目按需起 CI 机器：常驻一台小机（不跑任何 CI），观察排队 →
建/删 DigitalOcean runner 机器（经 vpsctl）→ 自愈 → 出事开 issue 并调用大模型诊断。
机器侧配套见 [cirunner](https://github.com/hoshinojian/cirunner)（cloud-init 配方 + 机器上的
`cirunner health/diag/act`）。

完整设计（唯一依据）：[docs/REDESIGN.md](docs/REDESIGN.md)。

## 当前状态（阶段 1）

仅骨架 + 配置加载 + `ciorch stats`（基线统计）。调度/事故管线按 REDESIGN §9 分阶段落地。

```
ciorch [-config path] stats [-repo owner/name] [-since 7d] [-token <tok>]
```

token 解析顺序：`-token` > `CIORCH_TOKEN` > 配置 `token_file` > `gh auth token`。
（阶段 1 临时 token 源；GitHub App 认证阶段 3 落地。）

## 开发

```
make lint    # gofmt + go vet（+ golangci-lint，未装则跳过）
make test    # go test ./...
make build   # 构建到 bin/ciorch
```

纪律：文档中文，代码/标识符/JSON 字段英文；conventional commits（中文描述）；
单一目的 PR，draft 打开，描述五段（变更/验证/风险/测试/关联）。
