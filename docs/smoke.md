# ciorch 沙箱冒烟手册（REDESIGN §8.2）

部署完成后按本手册逐条执行并取证。每条记录：命令、时间戳、观察到的
现象、日志/API 输出片段（贴进阶段 4 交付报告）。

**前置（所有条目共用）**：
- 编排器已部署且 `curl -s https://<host>/healthz` 返回 ok。
- 沙箱仓 workflow 已就位（`smoke.yml`、`smoke-badlabel.yml`，仅
  workflow_dispatch 触发——不触发时零成本，job 不会自动排队）。
- 观察日志：`journalctl -u ciorch -f`（编排器机器）；GitHub 侧用
  `gh api repos/hoshinojian/ciorch-sandbox/actions/runs/<id>/jobs` 或网页。
- 每条开始前确认上一条的机器已回收（`/status` 的 N=0、沙箱仓 Runners
  页 0 注册）——避免配额与判定串扰。
- **V1（busy runner 删除 422）已于 2026-11-21 实测（§11），不再重测。**

---

## S1 正常全流程（dispatch → 建机 → 回收）

**操作**：
```sh
gh workflow run smoke.yml --repo hoshinojian/ciorch-sandbox
# 随后取 run id：
gh run list --repo hoshinojian/ciorch-sandbox --workflow smoke --limit 1
```

**预期**：
- 编排器日志秒级出现 `Q=7`（7 个 job）与 `actions=[create,create]`
  （Q=7 > 一台容量，建满 `max_runners`）。
- ~3 分钟内（冷启实测 ~2.5m）首个 job 开跑（GitHub 侧 job 转 in_progress）。
- 全部 job 跑完（最长 600s 的 long-sleep）后进入空闲回收窗口（此后无新
  dispatch），编排器日志出现空闲回收 `drain` 动作，逐台删除；注册清干净。
  空闲阈值是自适应的（§3.6.3）：近 2h **不同 run** 的活跃点 ≥2 →
  `idle_timeout_busy`(45m)，否则 20m。注意：单跑一轮冒烟（1 个 run）按设计
  应走 **20m** 档；2026-11-21 首测出现的 45m 是 LastActivity 跨轮不去重 bug
  所致（同 run 8 job 记 8 点顶满判据），修复见 PR #47——修复后单 run 只记
  1 点、正常回落 20m 档。

**取证**：
- 建机时刻：`journalctl -u ciorch | grep 已建机`（与 dispatch 时间差）。
- 回收：`grep drain`（两条，间隔一轮——一轮只回收一台）。
- 收尾：`gh api repos/hoshinojian/ciorch-sandbox/actions/runners` →
  `[]`；`/status` N=0。
- 费用：两台 × 约 30 分钟 ≈ $0.15。

## S2 掉线自愈（场景 C）

**前置**：S1 完成、机器已回收。重新 dispatch smoke.yml；等首个 job 开跑。

**操作**（编排器机器外，ssh 到 **runner 机器**）：
```sh
vpsctl list -tag ciorch -only team2-ci   # 取 runner 机器 IP
ssh root@<runner-ip> 'systemctl stop actions.runner.*'
```

**预期**：2 轮采样（~30s）后编排器判定场景 C：先建替换机（有配额时），
强删旧机；剩余**排队** job 被新机接走。注意（2026-11-21 实测）：强删时
runner 上**进行中**的 job 不会被 GitHub 重排——该 job 以失败告终
（step 显示 cancelled / runner lost communication），run 结论为 failure；
只有"剩余排队 job 换 runner_name 跑完"这一半是 success。

**取证**：
- 编排器日志：`场景 C 掉线` 的 force-delete 动作行与其 Reason。
- GitHub 侧：剩余 job 的 `runner_name` 换成新机名。

## S3 假活（场景 A 或 C）

**操作**：dispatch smoke.yml；job 开跑后对 runner 机器：
```sh
ssh root@<runner-ip> 'kill -STOP $(pgrep -x Runner.Listener)'
# pgrep 必须 -x 或 [R]unner.Listener 写法，防自匹配（§5.4 坑）
```

**预期（按 §11 V6 实测）**：Listener STOP → GitHub 侧数分钟内转
`status=offline, busy=false` → **落入场景 C**（2 轮采样 → 替换+强删）。
若现场表现为 online+busy=false（P139 原始形态）则应走场景 A（3×15s 采样
→ restart-runner → 2 分钟观察窗）——如实记录实际落入哪个场景。

**取证**：STOP 后每 15s 采一次 `gh api .../actions/runners`（5 分钟），
贴 status/busy 序列；编排器命中的场景行；测完 `kill -CONT` 无必要
（机器会被回收）。

## S4 冷启失败 → 熔断 → 事故 → 人工恢复（场景 B/I + LLM）

**操作**：改编排器 `/etc/ciorch/ciorch.json` 的 **`machine_agent.sha256`**
为 64 个 `0`（agent 下载校验必失败 → cloud-init exit 1 → 冷启超时）：
```sh
cd /etc/ciorch && cp ciorch.json ciorch.json.bak
jq '.machine_agent.sha256 = "0000000000000000000000000000000000000000000000000000000000000000"' \
  ciorch.json > ciorch.json.tmp && mv ciorch.json.tmp ciorch.json
systemctl restart ciorch
```

**预期**：每次 dispatch smoke.yml → 建机 → 8 分钟冷启超时 → 场景 B 强删
+ `Fails++`。**第 3 次**后熔断 open（停止建机）+ 开 issue（阶段 5 后含
LLM 诊断 → needs-human）。**人工关闭该 issue** → 熔断复位；恢复配置：
```sh
cd /etc/ciorch && mv ciorch.json.bak ciorch.json && systemctl restart ciorch
```
再 dispatch 一次确认建机恢复。

**取证**：三次 B 动作行（Fails 1→2→3）、熔断 open 行、issue 链接与
内容、关闭后 `ciorch status`（或 /status）的 breaker=closed。

## S5 job 卡死（场景 D）

**操作**：dispatch 时传长睡参数：
```sh
gh workflow run smoke.yml --repo hoshinojian/ciorch-sandbox -f long_sleep_sec=7200
```
（阈值不调时 60m 告警 / 90m 击杀；要快速验证可在 config 的
`global.tuning` 覆盖 `job_stuck_alert`/`job_stuck_kill` 为 3m/5m——
测完删掉覆盖重启。）

**预期**：长睡 job 进行中：60m（或调低后 3m）编排器开 D 事故 issue；
90m（或 5m）强删机器（force-delete），job 在 GitHub 侧失败
（runner lost communication）。其余 6 个短 job 早已正常完成。

**取证**：issue 内容（含 job 时长）、force-delete 行、GitHub job
conclusion=failure 的原因片段。

## S6 label 无人能满足（场景 E）

**操作**：
```sh
gh workflow run smoke-badlabel.yml --repo hoshinojian/ciorch-sandbox
```

**预期**：编排器**不建机**（`UnmatchedQueued` 非空）；5 分钟后开 E 事故
issue（label 组合为主体）。job 一直 queued——**测完手动取消 run**。

**取证**：日志的 `未匹配排队`/E 事故行、issue 链接、`/status` N=0。

## S7 编排器重启（状态恢复）

**操作**：S1 的 run 进行到中段（如 test job 在跑）时：
```sh
systemctl restart ciorch
```

**预期**：状态从 state.json 恢复；**不误删**（无因记忆丢失触发的删除）、
**不重复建**（droplet CreatedAt 仍为判龄依据，在线机器不判冷启超时）；
run 正常完成、后续回收照常。

**取证**：重启前后各一条 `/status`（N、机器列表一致）；run 最终 success；
之后仍出现空闲回收行。

## S8 编排器宕机外检

**操作**：`systemctl stop ciorch`，等下一个整点过 7 分钟（healthcheck
cron `7 * * * *`）或手动 `gh workflow run healthcheck.yml --repo hoshinojian/ciorch`。

**预期**：healthcheck workflow 开 `ciorch-down` issue（或对已有 open
issue 追加评论）@维护者。**测完 `systemctl start ciorch` 并关闭 issue**。

**取证**：issue 链接与时间戳；编排器恢复后下一次 dispatch 正常。

---

## 清理清单（每条测完）

- runner 机器：`vpsctl list -tag ciorch -only team2-ci` 应只剩编排器自身
  （tag `ciorch-host`）；多余的按 `vpsctl delete -ids team2-ci/<id> -confirm 1`。
- 注册：沙箱仓 Runners 应为 0（编排器自愈路径已覆盖；残留手动删）。
- S4 的配置恢复（`ciorch.json.bak`）与 tuning 覆盖删除。
- S6/S8 的 run 取消与 issue 关闭。
- state：`/var/lib/ciorch/state.json` 可保留（熔断记忆是测试产物，若
  S4 后 breaker 仍 open 用 `ciorch reset hoshinojian/ciorch-sandbox`）。
