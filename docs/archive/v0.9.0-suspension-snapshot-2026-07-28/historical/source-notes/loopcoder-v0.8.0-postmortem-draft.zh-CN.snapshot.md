# LoopCoder v0.8.0 开发与发布复盘

日期：2026-07-16  
性质：内部工程复盘与后续版本强制执行规则  
适用范围：LoopCoder 自举开发、Paseo/Codex/Claude Code/Grok 等宿主调用、GitHub CI 和公开发布

## 一句话结论

v0.8.0 的主要功能已经开发完成，但开发过程不合格：版本范围过大、自举链路不稳定、测试重复、失败恢复粒度错误、状态不可见，并且没有本机资源上限。最终结果是大量时间花在重复编排和重复验证上，持续占用本机资源，甚至导致电脑硬重启。

这不是“复杂软件自然需要三天三夜”，而是工程流程没有设置成本上限、停止条件和责任边界。

## 当前发布状态快照

截至 2026-07-16：

| 环节 | 状态 |
|---|---|
| v0.8.0 功能开发 | 基本完成 |
| `#967` 本地确定性等待状态机 | 完成 |
| `#968` 编排 token 成本预算 | 完成 |
| 最后一个 release blocker `#997` | 修复已进入 `pre-prod` |
| `pre-prod -> main` | PR `#999` 正在跑远端保护门 |
| macOS arm64 构建与签名 | 上一个候选已通过 |
| Release smoke | 上一个候选因迁移健康 JSON 字段错误失败；`#997` 已针对该问题修复 |
| GitHub Release | 仍为 draft，尚未公开 |

因此，“开发完成”和“发布完成”必须区分：功能开发已经收口，公开发布尚未完成。

## 发生了什么

### 1. 一个版本承担了过多目标

v0.8.0 同时承载了多 provider、动态模型路由、额度感知、sub-agent、状态持久化、恢复、等待状态机、报告、成本治理、macOS arm64 单平台发布等多条主线。

这些不是一个普通小版本，而是多个基础设施项目叠在一起。版本号仍叫 `0.8.0`，实际工作量和风险已经接近一次编排核心重写。

### 2. LoopCoder 用自己开发自己，放大了每个缺陷

普通项目中，开发工具出错只影响一次任务。自举开发中，LoopCoder 的 worker、恢复、报告、进程管理或交付缺陷，会直接破坏 LoopCoder 自己的开发过程。

于是出现了放大回路：

1. LoopCoder 派 worker 修 LoopCoder。
2. worker 完成实现，但 push、报告或恢复出错。
3. conductor 把交付故障误判成实现失败。
4. 同一个实现重新跑 worker、测试和 verifier。
5. 新运行再次消耗模型额度、CI 时间和本机资源。

### 3. 验证层级严重重复

同一套或高度重叠的检查曾出现在：

- worker 自测；
- 本机 pre-push hook；
- conductor 再次本地测试；
- verifier 定向测试；
- PR CI；
- promotion PR CI；
- `main` 集成 CI；
- release workflow；
- release smoke。

全仓 `race` 本身可能运行三十分钟以上。一次微小修复重复经过上述层级后，等待成本远高于编码成本。

### 4. 失败恢复的粒度错误

不同失败没有被严格分类：

| 实际失败 | 正确动作 | 曾经发生的错误动作 |
|---|---|---|
| 编码失败 | 恢复或重派 worker | 合理 |
| 定向测试失败 | 修正对应代码/测试 | 有时扩大为全仓返工 |
| push 超时 | 只重试 push | 重新跑实现和测试 |
| PR 创建失败 | 只恢复 PR 创建 | 重派 worker |
| CI 等待 | 本地无模型等待 | 启动 watcher 或反复查询 |
| verifier 报告未展示 | 恢复报告 | 重新调用昂贵 verifier |
| GitHub runner 慢 | 等待远端 | 本地又跑一遍同类测试 |

最典型的问题是：实现已经存在于 commit 中，但 delivery-only 故障触发完整重跑。

### 5. 没有真正的版本冻结线

审计不断发现真实问题，这是有价值的；但没有预先定义“什么等级必须阻止发布、什么等级进入下一个补丁版本”。

结果是：

- issue 被多次重开；
- 每轮审计都可能把新发现继续塞回 0.8.0；
- release candidate 不断失效；
- 修完一个边界后继续无限寻找下一个边界；
- 版本没有稳定的结束条件。

质量不能靠无限审计定义。发布前必须先定义可接受风险和停止条件。

### 6. 报告能力没有形成闭环

虽然 LoopCoder 已有 reporter、progress receipt、relay 和 verifier 报告，但用户看到的仍可能是长时间无输出、只有 `Thinking`、或者任务结束后才出现完整报告。

缺失的是统一的用户可见运行状态：

- 当前处于哪个阶段；
- 最近一次真实进展是什么；
- 当前是否正在调用模型；
- 当前是否只是等待 CI；
- 已消耗多少时间和额度；
- 下一个超时点是什么；
- 是否需要用户介入。

“后台仍在运行”不是报告。报告必须提供可验证的新证据。

### 7. 没有本机资源治理

`docs/specs/0968-orchestration-cost-budget.md` 已经约束模型调用次数、token 和编排开销比例，但它没有覆盖：

- CPU 核心占用；
- 内存和 swap；
- 子进程数量；
- 同时存在的 provider CLI 数量；
- 本地测试并发；
- 单个进程组最长寿命；
- 孤儿进程；
- Paseo、Codex、Claude、Grok 等宿主叠加后的总负载。

因此，系统可能满足 token 预算，却仍把整台电脑拖死。这是当前成本模型的重大缺口。

## 走过的弯路

### 版本规划

- 把多个独立平台能力全部塞进 0.8.0。
- issue 粒度过大，一个 worker 同时负责设计、迁移、并发、恢复、测试和文档。
- 已进入发布阶段后仍加入新的能力要求。
- 没有在编码前锁定 release blocker 的严重度标准。

### Worker 与 sub-agent

- 大任务交给单个长时间 worker，缺少中间检查点。
- worker 失败后经常从头重跑，而不是从 durable checkpoint 恢复。
- 编排层没有严格限制 provider 和 sub-agent 的总并发。
- 高级模型用于机械等待、轮询和交付恢复，浪费额度。

### 测试与 CI

- 本地和远端重复执行全仓测试。
- 小改动也运行完整 `race` 矩阵。
- pre-push hook 承担了本应由远端 CI 承担的重检查。
- 用 wall-clock 等待代替确定性同步，产生慢 runner 偶发失败。
- 一个测试超时被误认为产品并发错误，造成额外返工。
- 没有把 focused、package、integration、full-race、release-smoke 分成明确层级。

### Git、PR 与 worktree

- push 超时后重复执行上游步骤。
- 临时 worktree 和分支缺少统一生命周期管理。
- 空 commit、错误 tip 或错误 worktree 曾污染远端分支。
- conductor 本地 checkout 与远端 `main` / `pre-prod` 不一致，增加误判风险。
- 交付和实现没有清晰的幂等边界。

### 等待与状态

- 使用常驻 shell watcher 或高频轮询等待 GitHub 状态。
- 等待过程没有稳定的五分钟用户回执。
- verifier 已执行，但报告没有可靠地进入用户可见通道。
- “进程还在”被当作进度，而没有判断它是否真的前进。

### 发布

- 过度依赖绿 CI，直到真实 release artifact smoke 才发现 JSON 合约问题。
- 修复一个 release smoke 缺陷后，需要重新经历完整候选链路。
- draft、tag、候选 SHA 和公开 release 的状态区分不够直观。
- 发布完成标准没有在一开始形成一张单一证据表。

## 做对了什么

复盘不能只记录错误。以下做法应保留：

- 最终将平台冻结为 macOS arm64，避免继续承担三平台矩阵。
- `main` 启用了保护分支和远端必需检查。
- Release 使用单一构建产物、SHA256 和 Sigstore keyless 签名。
- 发布前运行真实 archive 安装、升级、迁移和回滚 smoke。
- 运行时状态移出 repo，避免跟随 Git 上传。
- 对嵌套执行、claim、lease、fencing 和恢复语义进行了真实并发测试。
- 最后把 `#997` 限制为两个文件的最小 release-blocker，而没有再扩展功能。

这些措施提高了产品可靠性。问题在于它们缺乏成本分层，被重复执行了太多次。

## 今后的强制规则

以下不是建议，而是后续版本的默认工程契约。

### 1. 版本规模

- 一个 minor 版本最多 8 到 12 个可交付 issue。
- 每个实现 issue 的目标 worker 时间不超过 30 分钟。
- 一个 issue 最多一个主要产品行为、一次迁移和一组验收测试。
- 超过 5 条独立 acceptance criteria 时必须继续拆分。
- 依赖链深度最多 3 层。
- 进入 release candidate 后禁止加入新功能。

更大的主题进入 roadmap，但必须跨多个版本交付，不能全部进入同一个 milestone。

### 2. 发布冻结与 bug 分级

进入 RC 后只允许以下问题阻止发布：

- P0：数据损坏、安全漏洞、无法安装或启动；
- P1：核心验收路径必现失败、重复执行、错误成功、不可恢复；
- release-contract：签名、版本、资产、升级、迁移或 rollback smoke 失败。

以下内容默认进入下一个补丁版本：

- P2 非核心边界；
- 文案、格式和低风险兼容性问题；
- 不影响当前公开承诺的增强；
- 需要大规模重构才能修复的非阻塞问题。

同一 issue 被重开两次后，不得继续无限重开。必须由人决定：降级、拆分、延后或取消发布。

### 3. 本地与远端边界

| 工作 | 默认位置 |
|---|---|
| 格式化、编译、单包 focused tests | 本地，短时 |
| `go test ./...` | GitHub 远端 |
| 全仓 `go test -race` | GitHub 远端 |
| staticcheck、vet、安全扫描 | GitHub 远端 |
| macOS arm64 构建 | GitHub macOS runner |
| 签名、checksum、release assets | GitHub 远端 |
| 安装、升级、迁移、回滚 smoke | GitHub macOS runner |
| provider 安装与本地认证探测 | 本地，只读 |
| Paseo/Codex/Claude/Grok 宿主集成 | 本地，小范围人工 smoke |

除非用户显式允许，LoopCoder 不得在本机运行全仓测试或全仓 race。

### 4. 本机资源上限

默认本地预算：

- provider worker 并发：1；
- verifier 并发：1，且不得与 worker 同时运行；
- sub-agent 并发：0；
- 本地测试并发：1；
- 单个任务软超时：10 分钟；
- 单个任务硬超时：15 分钟；
- LoopCoder 进程树最大进程数：8；
- LoopCoder 进程树最大 RSS：2 GiB；
- LoopCoder 进程树持续 CPU 上限：150%（约 1.5 个核心）；
- 本机 load 或 memory pressure 超过阈值时禁止启动新任务；
- 超时、取消或宿主退出时必须终止整个进程组并等待清理完成。

任何一项超过上限，默认结果是 `needs-human`，不得自动降级到另一个 provider 继续消耗资源。

### 5. 五分钟报告契约

运行超过五分钟的任务必须至少每五分钟发出一次用户可见回执，内容固定为：

```text
stage: <planning|coding|testing|waiting-ci|reviewing|delivering>
elapsed: <duration>
last_progress: <timestamp + evidence>
provider_active: <yes|no>
local_processes: <count>
remote_gate: <name + state>
next_timeout: <timestamp>
next_action: <one concrete action>
```

如果五分钟内没有新证据，必须明确写“无新进展”，并说明是在等待什么。连续两个周期无新进展时自动停止或转为 `needs-human`，不能继续静默运行。

### 6. 失败分类与恢复

每次失败必须先归入以下唯一类别：

1. `implementation-failure`
2. `test-failure`
3. `delivery-failure`
4. `provider-failure`
5. `infrastructure-failure`
6. `waiting-timeout`
7. `human-decision-required`

只有 `implementation-failure` 可以重新进入编码。

- push 失败只重试 push；
- PR 创建失败只重试 PR 创建；
- report 丢失只重放 report；
- CI 慢只等待远端；
- provider 已返回成功时不得重新调用 provider；
- 已存在 commit 时不得重新生成同一实现。

自动重试最多一次。第二次失败必须停止并报告。

### 7. 测试分层

| 阶段 | 允许的检查 |
|---|---|
| worker | 格式化、编译、直接受影响包的 focused tests |
| PR | 远端 test、race、verify、security |
| promotion PR | 远端保护门，不重复本地测试 |
| integrated main | 一次远端集成矩阵 |
| release tag | build once、签名、真实 artifact smoke |

pre-push hook 只能做快速检查，目标不超过 60 秒。禁止在 pre-push 中运行全仓 race 或与 PR CI 完全重复的全套检查。

### 8. 等待必须零模型、零忙循环

CI、审批、额度重置、outbox 和 worker terminalization 等待必须：

- 不调用模型；
- 不保持 provider CLI；
- 不使用高频 shell loop；
- 使用远端事件或低频确定性查询；
- 不占用 worker claim；
- 不延长无意义的上下文会话；
- 状态变化后只唤醒一次。

`#967` 的本地等待状态机是正确方向，但必须和用户可见报告及资源治理结合，不能只做到“模型调用为零”。

### 9. Worktree 和进程清理

每个任务结束时必须生成清理清单：

- provider 进程已退出；
- 测试进程已退出；
- watcher 已退出；
- 临时 worktree 已移除或明确保留原因；
- stale worktree registration 已 prune；
- 分支已推送或标记为本地草稿；
- durable report 已写入；
- 不存在孤儿进程。

清理失败必须可见，不能在后台继续积累。

### 10. 发布完成定义

一个版本只有在以下证据全部存在时才算完成：

- 保护分支 `main` 上的最终 SHA；
- 最终 SHA 的远端 CI 全绿；
- tag 指向该 SHA；
- 只包含目标平台资产；
- checksum 与远端资产一致；
- Sigstore 验证成功；
- 下载后的真实二进制版本信息正确；
- 安装、升级、迁移和 rollback smoke 成功；
- GitHub Release 已公开而非 draft；
- release blocker 为 0；
- GO/NO-GO 文档记录了上述证据。

“代码写完”“PR 合并”“CI 绿”都不等于公开发布完成。

## 下次版本的推荐流程

### Phase 0：范围冻结

- 选择最多 8 到 12 个 issue。
- 每个 issue 估算代码、测试和迁移风险。
- 明确哪些内容不属于本版本。
- 写好停止条件和 release blocker 等级。

### Phase 1：小批实现

- 每批最多 2 到 3 个互不依赖 issue。
- 本地只做 focused tests。
- 每个 issue 一个 durable checkpoint。
- delivery 失败不得重新编码。

### Phase 2：远端集成

- PR CI 在 GitHub 运行。
- 失败按类别修复一次。
- 不在本机重复远端检查。

### Phase 3：RC 冻结

- 停止新功能。
- 只修 P0、P1 和 release-contract 问题。
- 最多两个 release candidate；第二个仍失败时召开人工 go/no-go，不自动无限修复。

### Phase 4：公开发布

- 构建一次；
- 签名一次；
- smoke 一次；
- 人工批准发布；
- 记录证据并关闭 milestone。

## 0.8.1 应优先补的工程能力

以下内容是本次复盘直接产生的后续项，不应重新塞回 0.8.0：

1. 本机 CPU、内存、进程数和执行时间 resource governor。
2. 默认 remote-heavy execution policy。
3. 五分钟用户可见 progress receipt 硬门。
4. delivery-only resume，不重新调用 provider。
5. focused / PR / promotion / release 测试分层。
6. worktree 与孤儿进程 janitor。
7. verifier/report 的单一可靠展示通道。
8. 版本 RC 冻结和最多两次候选规则。

这些必须拆成小 issue，每个 issue 只解决一个明确问题。

## 最终决策

v0.8.0 继续按当前冻结范围完成发布，不再加入任何新功能。

后续版本默认采用“远端承担重活，本机只做短时宿主验证”的模式。任何自动化如果不能证明自己受 CPU、内存、进程、时间、重试和报告约束，就不允许长时间运行。

本次最重要的教训不是“测试太慢”，而是：

> 自动化没有预算、停止条件和可见状态时，能力越强，失控成本越高。
