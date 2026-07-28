# loopcoder 0.6.0 后的产品迭代判断

更新时间：2026-07-08

代码边界：

- 本地已读到 `jasonhnd/loopcoder` main：
  `6795c90a2cfa715b20cf2c6444b4dec7a0ed3646`
  (`loopcoder promote pre-prod to main`, 2026-07-07 23:29:32 +0900)。
- 这比此前读到的 `d0a5c25` 多 6 个提交，包含 0.6.0 Unit B/1 到 Unit B/5 的 reporter 实现。
- GitHub Releases/Tags 当时仍停在 `v0.5.4`，没有公开 `v0.6.0` tag。
- 本机 `go` 不在 PATH，未能运行 `go test ./...`；以下结论来自代码和文档静态阅读。

复核补充（2026-07-08）：

- 已重新 `git fetch --all --tags --prune`，本地 `origin/main` 仍是
  `6795c90a2cfa715b20cf2c6444b4dec7a0ed3646`。
- 本地 tag 列表没有 `v0.6*`。
- 公开 GitHub Releases 页面 (`https://github.com/jasonhnd/loopcoder/releases`)
  显示 latest 仍是 `v0.5.4`；公开 Tags 页面
  (`https://github.com/jasonhnd/loopcoder/tags`) 也从 `v0.5.4` 开始，没有
  `v0.6.0` 或 `v0.6.1`。
- 当前环境仍没有 `go` 命令，因此所有测试建议都写成 0.6.1 发布前必须由
  loopcoder 开发环境/CI 验收，而不是声称本地已经通过。

## 摘要

0.6.0 已经把 loopcoder 的证据层从 `attestation` 过渡到 `reporter`，并补上了本地 `loopcoder report` 查询能力。因此下一阶段不应继续围绕 reporter 改名打转，而应补上开发者本地使用和 host 调用时缺失的产品层。

核心判断：

- 0.6.0 是协议可信度和证据层的进步。
- 0.6.0 不是产品顺滑度的跃迁。
- loopcoder 现在更可靠，但用户仍然离内部协议太近。
- 下一步应把 `DeliveryRun` 变成主要用户对象。

一句话：

> Reporter 已落地，下一步应补开发者本地状态、DeliveryRun 产品层、host 可调用协议。

## 代码事实

0.6.0 main 已经完成的 reporter 变化：

- `internal/attestation` 已迁移为 `internal/reporter`。
- 共享调用记录类型现在是 `reporter.Report`。
- 新的 one-line header 使用 `[reporter]`。
- 新的 result JSON 和 run-state JSON 字段使用 `report`。
- 读路径仍兼容旧的 `[attestation]` header 和 `attestation` 字段。
- `loopcoder report` 已加入命令表。
- `internal/reportquery` 会扫描本地 run JSON/JSONL、attempt records、relay ledger 和 pending relay records。
- relay pending record 可以保存结构化 `report`，也保存 human pretty block。
- `worker`、`dispatch-wave`、`loopreview`、`runstatus`、`audit`、`relay`、conductor hooks 都已向 reporter 命名迁移。

关键代码依据：

- `loopcoder report` 已在命令表：`internal/cli/cli.go:85-114`。
- `report` 已支持 `--repo`、`--work-id`、`--issue`、`--role`、`--limit`、
  `--format text|json`：`internal/cli/cli.go:4106-4210`。
- `reportquery.Record` 内部已有 `source`、`run_id`、`path`，但
  `MarshalJSON` 目前只输出 `reports`：`internal/reportquery/reportquery.go:40-45`
  与 `internal/reportquery/reportquery.go:111-119`。
- `Report.Pretty()` 当前只有 `provider` 字段；provider display 为
  `OpenAI Codex / codex` 等组合字符串，没有单独 `tool` line：
  `internal/reporter/pretty.go`。
- `doctor` 当前只有 text 输出，没有 `--format json`：
  `internal/cli/cli.go:862-907`。
- `doctor` 已检查 git、gh、config、models、providers、origin/default branch、
  binary、compatibility、audit、skill、hooks，但没有检查 `.loopcoder/`
  是否被本地 exclude 或是否已经被 Git tracking：
  `internal/doctor/doctor.go`。
- `loopcoder init` 当前没有 `--repo` flag，不写 `.git/info/exclude`：
  `internal/cli/cli.go:910-972`。
- `scaffold.DeliveryTemplate` 当前生成 `gate: auto`：
  `internal/scaffold/scaffold.go:188-205`。
- runtime promotion gate 对空值仍归一化为 `auto`：
  `internal/orchestration/promote.go:885-890`。
- local run state 当前明确在 repo 内：`state.RunsRoot(repoPath)` 返回
  `<repo>/.loopcoder/runs`，见 `internal/state/state.go:145-147`。
- `skill install` 会写 `.loopcoder/conductor-workspace`，但不写
  `.git/info/exclude`：`internal/cli/skill.go:23-29` 与
  `internal/cli/skill.go:253-290`。

关键文档依据：

- README badge 是 `v0.5.4`，但 Status 段仍写 `v0.4.2 is the current...`。
- stability policy 仍写 `Current stable release: 0.3.7`。
- usage docs 的 pinned install 示例仍是 `0.3.7`。
- README 的 mechanical command list 没有列 `loopcoder report`。
- usage docs 的 Binary Commands 列表没有列 `loopcoder report`。
- usage docs 写 pretty block 有单独 `tool` line，但代码没有。
- README 和 usage docs 多处说 `.loopcoder/` 是 gitignored/local-only，例如
  `README.md:111-116`、`docs/reference/usage.md:209-211`、
  `docs/reference/usage.md:768-770`；但 `init` 和 `skill install` 当前没有
  实际写入 `.git/info/exclude`。

0.6.0 main 没有完成的产品层变化：

- 没有 `loopcoder setup`。
- 没有 `loopcoder project list` 或 `loopcoder project inspect`。
- 没有 `loopcoder run inspect`。
- 没有 `loopcoder plan`。
- 没有 `loopcoder continue`。
- 没有 `loopcoder decide`。
- 没有 `loopcoder capabilities --format json`。
- 没有把运行状态从 `<repo>/.loopcoder` 迁到 loopcoder home。
- 没有为 host 暴露命令副作用等级。
- 没有把 `ready-set / dispatch-wave / loopreview / relay / tick / promote` 包装成用户可理解的下一步动作。

当前本地状态仍在 repo 内：

- `state.RunsRoot(repoPath)` 返回 `<repo>/.loopcoder/runs`。
- relay state 仍在 `<repo>/.loopcoder/relay`。
- `loopcoder init` 仍只负责 scaffold `.delivery.yml`、`ROADMAP.md` 和 GitHub labels。
- `internal/home` 目前主要管理 `~/.loopcoder/bin`、`versions`、`skills`，还不是多项目工作状态 home。

## 重新判断

之前我们说 loopcoder 的问题不是能力弱，而是内部协议泄露到主界面。读完 0.6.0 代码后，这个判断更清楚了。

0.6.0 修好了其中一块：证据层的命名、结构、兼容和查询。

但它没有修主矛盾：用户仍然需要操作 delivery protocol。

Paseo 让用户面对的是：

- agent
- workspace
- session
- terminal
- browser
- message
- status

loopcoder 让用户面对的是：

- ready-set
- dispatch-wave
- loopreview
- reporter/report
- relay
- gate
- state branch
- lease
- promote
- needs-human

这些内部对象都合理，但它们不应该是普通开发者和 host 的主 mental model。

新的判断是：

> loopcoder 的引擎越来越完整，但产品入口仍然像工程控制台，不像开发者可自然委托的本地助手。

## 0.6.0 带来的进步

Reporter 是正向进步，原因有四个。

第一，命名更贴近人。

`report` 比 `attestation` 更容易理解。用户更容易接受“本地报告”“证据报告”“worker report”，不需要先理解安全协议术语。

第二，证据更可查。

`loopcoder report` 可以按 repo、work id、issue、role 查询本地报告。这解决了一部分“跑过什么、谁跑的、模型是什么、token 是多少、是否 verified”的可见性问题。

第三，升级兼容做得比较稳。

新输出用 `reporter/report`，读路径仍接受旧 `attestation`。这对 host 和旧 run state 很重要，不会让用户一升级就丢历史状态。

第四，local-only 原则没有被破坏。

reporter 仍定位为本地可见证据，不进入 PR body、comment、commit、merge commit 或 merge comment。这点应该继续保留。

## 0.6.0 没解决的问题

第一，`report` 是证据查看器，不是下一步控制器。

它能告诉用户“发生过什么”，但不能稳定回答：

- 现在这个 run 目标是什么？
- 当前卡在哪里？
- 风险是什么？
- 需要我做哪个决定？
- 下一步应该执行什么？
- 这个动作会写本地、GitHub、pre-prod 还是 production？

第二，`status` 仍然偏状态表，不是 `DeliveryRun` 视图。

`loopcoder status` 已经能看 worker/verifier 信息，但它仍从 `.loopcoder/runs` 组织状态，输出的是 run record 和表格，不是一个面向用户和 host 的产品对象。

第三，本地状态仍放在 repo 目录下。

这与“本地开发工具状态不能上传 GitHub”的目标冲突。即使 `.gitignore` 可以降低风险，它也不是隐私边界。用户可能 force-add，repo 可能被打包，工具可能复制整个 working tree。

第四，第一次运行仍不够自然。

`loopcoder init` 是 scaffold，不是 setup。它不会完成项目注册、状态根目录选择、provider auth 检查、GitHub remote 解释、风险说明、下一步建议。

第五，host 调用仍缺少 contract。

Paseo、Codex、Claude Code 这类 host 如果要调用 loopcoder，需要知道：

- 这个 binary 支持哪些能力？
- 哪些命令只读？
- 哪些命令会花 token？
- 哪些命令会写 GitHub？
- 哪些命令会 merge pre-prod？
- 哪些命令可能 promote production？
- 当前 run 的下一步是否需要用户批准？

现在这些信息主要藏在文档、命令名和代码路径里。

## 产品方向

loopcoder 下一阶段要做的不是更多自动化，而是 host-legible delivery control。

也就是让任何本地 host 能稳定问 loopcoder 四个问题：

1. 这个项目是谁？
2. 当前 delivery run 想完成什么？
3. 现在卡在哪里，风险是什么，需要谁决定？
4. 如果用户批准，下一步会执行什么，副作用是什么？

推荐把 `DeliveryRun` 作为主产品对象。

`DeliveryRun` 应该包含：

- project identity
- workspace identity
- goal
- current phase
- active worker/verifier invocations
- changed branches and PRs
- reports
- blockers
- decisions
- risk summary
- next action
- side-effect class
- approval requirement

内部仍然可以使用 `compile`、`ready-set`、`dispatch-wave`、`loopreview`、`risk gate`、`promote`、`relay`、`reporter`、`state branch`。但用户和 host 不应该首先面对这些内部步骤。

## 推荐迭代顺序

### P0：本地项目注册和状态搬家

先解决多项目本地状态问题。

推荐新增：

```text
~/.loopcoder/
  bin/
  versions/
  skills/
  projects.json
  projects/
    github.com/owner/repo/
      project.json
      workspaces/
        <workspace-id>/
          workspace.json
          runs/
          relay/
          hooks/
          guardrails/
          events/
          index.db
```

repo 内只保留共享配置和源码：

```text
<repo>/
  .delivery.yml
  ROADMAP.md
  source files...
```

设计原则：

- 项目显示名可以是 `owner/repo`。
- 内部 project id 应包含 host，例如 `github.com/owner/repo`。
- workspace id 应区分同一个 repo 的多个 clone 或 worktree。
- 新运行状态默认写到 `~/.loopcoder/projects/...`。
- 旧 `<repo>/.loopcoder` 继续读取，作为兼容输入。
- 迁移用显式命令，例如 `loopcoder state migrate --repo <path>`。
- 对旧路径做防御性保护，把 `.loopcoder/` 写入 `.git/info/exclude`。

SQLite 的位置：

- 不要先做一个全局 canonical SQLite。
- 可以在每个 workspace 下放 rebuildable `index.db`。
- JSON/JSONL 先作为事实来源。
- SQLite 只做查询索引和 host UI 加速。

### P1：guided setup

新增：

```bash
loopcoder setup --repo <path>
```

它应该做：

1. 解析 repo。
2. 识别 GitHub remote，显示 `owner/repo`。
3. 注册 project 和 workspace。
4. 选择 out-of-repo state root。
5. 检查 `git`、`gh`、provider CLI、provider auth、model registry。
6. 推荐 worker/verifier 默认值。
7. 创建或更新 `.delivery.yml`。
8. 只在用户明确同意时创建或更新 `ROADMAP.md`。
9. 把 `.loopcoder/` 写入 `.git/info/exclude` 作为 legacy 防线。
10. 说明哪些东西是 repo-tracked，哪些东西是 local-only。
11. 跑只读 readiness check。
12. 打印第一个安全下一步。

第一次运行不应该直接 `tick`，应该先给计划。

### P2：只读 run inspect 和 plan

新增：

```bash
loopcoder run inspect --repo <path> [--run-id <id>] --format json
loopcoder plan --repo <path> --goal <text> --format json
```

`run inspect` 回答当前状态。

`plan` 回答准备怎么做，但不产生远程写入，不启动 worker，不花 provider token，除非未来显式允许。

输出应包含：

- goal
- phase
- current status
- blockers
- reports summary
- risk summary
- proposed next action
- side-effect class
- required approval
- expert command preview

关键点：用户看到的是“下一步决策”，不是“请运行 ready-set 或 dispatch-wave”。

### P3：continue 和 decide

新增：

```bash
loopcoder continue --repo <path> --run-id <id> --approve <decision>
loopcoder decide --repo <path> --run-id <id> --decision <type>
```

它们应该把执行变成明确批准后的动作。

典型 decision：

- start worker wave
- retry failed worker
- run verifier
- flush local report evidence
- approve pre-prod merge
- pause run
- mark needs-human
- approve production promotion

原则：

- provider spend 需要明确批准。
- GitHub write 需要明确批准。
- pre-prod merge 需要明确批准或配置中明确允许。
- production promotion 必须显式显示为 production action。
- vague chat text 不应自动推断为生产批准。

### P4：host capabilities contract

新增：

```bash
loopcoder capabilities --format json
```

它应该告诉 host：

- binary version
- commit
- command list
- supported providers
- supported models and effort/depth values
- reporter support
- legacy attestation compatibility
- state layout version
- JSON schema versions
- side-effect classes per command
- whether command is local read, local write, provider spend, GitHub write, pre-prod write, production write

建议 side-effect classes：

- `read_local`
- `read_remote`
- `local_write`
- `provider_spend`
- `git_remote_write`
- `github_write`
- `preprod_write`
- `production_write`

host 不应该靠命令名猜副作用。

### P5：文档重排

文档应从开发者旅程开始，而不是从协议细节开始。

推荐结构：

- Quickstart：一个开发者，一个 repo，一个安全 first run。
- Concepts：Project、Workspace、DeliveryRun、Worker、Verifier、Report、Blocker、Decision、Promotion。
- Guided workflows：setup、first delivery、inspect、blocked work、recover、review、promote。
- Host integration：Paseo/Codex/Claude Code 如何调用，如何处理 stdout/stderr，如何保留 local-only reports。
- Reference：expert commands、config、state layout、exit codes、provider registry、reporter compatibility。
- Internals/specs：reporter、relay gate、state branch、risk gates、process supervision。

## 命令分层

保留现有 expert commands，但不要让它们成为主路径。

面向用户的主命令：

- `loopcoder setup`
- `loopcoder project list`
- `loopcoder project inspect`
- `loopcoder run inspect`
- `loopcoder plan`
- `loopcoder continue`
- `loopcoder decide`
- `loopcoder report`

expert commands 继续存在：

- `compile`
- `ready-set`
- `dispatch`
- `dispatch-wave`
- `loopreview`
- `tick`
- `relay`
- `state`
- `lease`
- `recover`
- `promote`
- `verify-local`
- `audit`
- `doctor`

映射关系：

- `ready-set` 成为 `plan` 和 `run inspect` 背后的实现细节。
- `dispatch` 和 `dispatch-wave` 成为 `continue` 背后的执行动作。
- `loopreview` 可以保留名称，但主界面展示为 review/verifier。
- `relay flush/list` 展示为“本地 report evidence 需要显示或确认”。
- `promote` 永远显示为 production approval。
- `attest` 保留一版兼容，但新文档不要把它作为用户主概念。

## 模式设计

推荐三种模式：

1. `guided`
   - 默认本地开发模式。
   - 解释下一步。
   - 对 provider spend、GitHub write、pre-prod、production 要求明确批准。
   - 优先服务 Paseo/Codex/Claude Code 这类 host。

2. `headless`
   - CI 或机器模式。
   - JSON-only 可用。
   - 不交互。
   - 缺少 approval 时 fail closed。
   - 高风险动作必须有显式 flag。

3. `expert`
   - 当前低层命令面。
   - 用于维护者、调试、CI wiring。
   - 不隐藏 `ready-set`、`dispatch-wave`、`relay`、`state`、`lease`、`promote`。

默认建议：

- CLI 默认：`guided`。
- host 默认：`guided` 加 JSON inspection。
- CI 默认：`headless`。
- 现有命令：继续兼容为 expert surface。

## 对 Paseo/Codex/Claude Code 调用的注意事项

loopcoder 被 host 调用时，应被视为 nested delivery engine，不是普通 shell 命令。

需要注意：

- Host agent 和 loopcoder worker/verifier 不应同时编辑同一个工作区。
- Host 必须能取消 loopcoder 管理的子进程。
- Host 必须保留 stdout/stderr，不能吞掉 reporter pretty block 或 relay block。
- Host 必须区分只读、花 token、写 GitHub、merge、promote。
- Host 不应把 local-only report 自动复制到 PR、comment、commit 或 issue。
- Host 应通过 `capabilities` 探测能力，而不是靠 release tag 猜。
- Codex/Paseo 不应照搬 Claude Code hook；需要通用 host contract。
- 当前 Claude hooks 是兼容路径，不是跨 host 的根本方案。

## Sub-Agent 并行模型

背景：

Paseo、Codex、Claude Code 这类 host 或 provider 都可能支持 sub-agent。
也就是说，A 可以把工作派给 B，B 又可以拆出 B1、B2、B3 并行执行。
这能提高速度，但也会带来状态、权限、成本、错误归因和取消控制问题。

基本原则：

> loopcoder 不应该无脑利用每个模型自己的 sub-agent 能力，而应该把
> sub-agent 当成可控的并行执行能力，纳入 DeliveryRun 管理。

否则会出现多层嵌套：

```text
Human
  Host
    loopcoder
      Worker
        provider sub-agent
          provider sub-agent
```

这种结构如果不受控，速度可能更快，但用户和 host 会失去对状态、
成本、权限和责任边界的理解。

推荐分工：

- loopcoder 拥有顶层任务拆分权。
- loopcoder 决定哪些 issue、PR、worker、verifier 可以并行。
- provider sub-agent 只负责单个工作单元内部的辅助分析、搜索、验证或
  patch proposal。
- 最终代码写入、commit、push、PR 创建仍由一个 loopcoder worker 汇总完成。

也就是说：

> sub-agent 可以帮忙想、查、验证、提出 patch；但第一版不要让多个
> sub-agent 同时拥有真实写入同一个 worktree 的权力。

推荐的 sub-agent 类型：

1. `research sub-agent`
   - 只读。
   - 可以读代码、读文档、查资料。
   - 输出发现和建议。
   - 不改文件。

2. `implementation sub-agent`
   - 默认不直接写 repo。
   - 可以生成 patch proposal。
   - 主 worker 决定是否应用。
   - 如果未来允许写，也必须在隔离 worktree 内写。

3. `review sub-agent`
   - 只读。
   - 检查实现、测试、风险和 spec conformance。
   - 输出 findings。
   - 不直接修代码，除非进入明确的 retry/fix worker。

第一版推荐只开放：

- 只读 sub-agent；
- patch-proposal sub-agent；
- 单 worker 汇总写入。

暂时不要开放：

- 多个 sub-agent 同时写同一个 worktree；
- 无限递归 sub-agent；
- sub-agent 自行创建 GitHub issue、PR、merge 或 promote；
- sub-agent 绕过 reporter；
- provider 自行决定成本上限；
- host 完全看不到 sub-agent 过程，只看到最终一句完成。

DeliveryRun 应记录 sub-agent graph：

```text
DeliveryRun
  Worker #123
    SubAgent B1: codebase research
    SubAgent B2: test planning
    SubAgent B3: risk review
  Verifier #123
```

每个 sub-agent 至少应有 report summary：

- parent run id
- parent worker/verifier id
- sub-agent id
- role
- provider
- model
- permission
- task
- started / ended
- token usage if available
- output summary
- artifact or patch proposal path if any
- exit status

host UI 应展示 agent tree，而不是散乱终端输出。

推荐的第一阶段能力：

1. 顶层并行仍由 loopcoder 控制。
2. worker 可以声明使用了 sub-agent。
3. sub-agent 默认只读。
4. sub-agent 输出必须被 worker 汇总。
5. 只有 worker 最终产出代码变更。
6. reporter 记录 sub-agent summary。
7. host 可以取消整个 worker，并由 loopcoder 传播取消到 sub-agent。
8. host 可以看到 sub-agent tree、状态、成本和失败点。

预算和限制：

- 每个 DeliveryRun 有总预算。
- 每个 worker 有预算。
- 每个 sub-agent 有子预算。
- sub-agent 数量有上限。
- sub-agent 深度第一版限制为 1。
- sub-agent 默认不能再创建 sub-agent。
- 超预算时 fail closed 或转为 needs-human。

权限建议：

- `research`: `read_local` / `read_remote`。
- `implementation-proposal`: `read_local` plus patch artifact write in temp state root。
- `implementation-write`: 暂缓；未来必须隔离 worktree。
- `review`: `read_local` / `read_remote`。
- `merge/promote`: sub-agent 永远不允许。

推荐实现路线：

1. 在 `DeliveryRun` 模型中加入 `children` 或 `sub_agents`。
2. 在 `reporter.Report` 外层增加 host/run 层的 sub-agent summary，不急着改变
   0.6.0 reporter 核心 schema。
3. 给 provider adapter 增加可选的 sub-agent manifest 输入/输出。
4. 第一版只接受 provider 返回的 sub-agent summary，不让 loopcoder 直接调度
   provider 内部 sub-agent。
5. 第二版再考虑 loopcoder 显式调度 read-only sub-agent。
6. 第三版才考虑隔离 worktree 下的 implementation sub-agent。

最终推荐：

> 混合方案。loopcoder 显式控制顶层 worker/verifier；provider 内部可以用
> sub-agent 加速，但必须受限、可报告、可取消、可预算。

## 暂缓事项

暂时不要做：

- 不要把 reporter 再大改一轮。
- 不要弱化 gates。
- 不要删除 expert commands。
- 不要把 local-only report 放到 GitHub 可见面。
- 不要先做一个全局 canonical SQLite。
- 不要让 SQLite 成为唯一事实来源。
- 不要先追求无人值守全自动，应该先让本地开发者看得懂、能接管、能批准。

## 最终建议

下一步产品路线应是：

1. `setup` 和 out-of-repo project state。
2. `run inspect` 和 `plan`。
3. `continue` 和 `decide`。
4. `capabilities --format json`。
5. 文档改成开发者旅程优先。

这条路线的目标不是让 loopcoder 少做事，而是让它把强协议包装成顺滑的本地开发体验。

真正要追的体验是：

> 用户说“帮我推进这个项目”，loopcoder 能清楚回答“我理解的目标是这个；现在状态是这个；我建议下一步做这个；会产生这些副作用；需要你批准这个决定。”

## 0.6.1 发布路线调整

新的版本判断：

- 0.7.0 继续承接真正的新产品层：`setup`、project registry、全局本地状态、`DeliveryRun`、`run inspect`、`plan/continue/decide`、capability protocol、sub-agent execution。
- 0.6.1 不再拆成多个 0.6.x 小版本，而是一次性做成 customer-ready bridge release。
- 0.6.1 的目标不是变成 0.7.0，而是让外部客户能安全安装、理解、诊断和试用当前 loopcoder。
- 0.6.1 必须保持 patch-release 谨慎性：不应静默改变已有项目的 runtime 语义，尤其不要把缺失 `adapters.gate` 的运行时默认从 `auto` 改掉；更稳的做法是只改变新 scaffold/quickstart 的显式默认。

0.6.1 必须塞入的过渡内容：

- 版本、release、README、usage docs、stability policy、CHANGELOG 全部对齐到 0.6.1。
- 首次使用默认安全：新 `loopcoder init` 生成的 `.delivery.yml` 推荐显式写
  `gate: human-merge`，但旧项目和缺失 gate 的 runtime 归一化不在 0.6.1
  中静默改变。
- `.loopcoder/` 本地状态必须用 `.git/info/exclude` 保护，doctor 必须检查泄露风险。
- `loopcoder report` 成为文档中的一等客户入口，JSON 输出包含 host 可解析的 source metadata。
- reporter pretty 文档和代码输出保持一致。
- `loopcoder doctor` 成为客户支持入口，覆盖 provider、version、local state、reporter transition、legacy hooks。
- legacy `attest`、`conductor-attest`、`[attestation]` 兼容窗口要延长到真正公开发布之后，不应在 0.6.1 结束。
- install/upgrade/release artifacts 要能被客户直接使用，不要求源码编译。
- 文档增加 command side-effect table，明确哪些命令只读、写本地、花 token、写 GitHub、merge/promote。
- loopcoder 自己继续用 loopcoder 开发自己，但 self-hosting repo 必须保持 human merge gate。

已单独写成完整 roadmap：

- `work/loopcoder-0.6.1-customer-ready-roadmap.md`

新的 release slogan：

> Customer-ready reporter release: safer local state, stable reports, stronger doctor, clearer docs.

## v0.8.0 开发与发布复盘

v0.8.0 的自举开发暴露了版本范围、重复验证、失败恢复、状态可见性和本机资源治理方面的系统性问题。完整复盘与后续强制规则见：

- `work/loopcoder-v0.8.0-postmortem.zh-CN.md`
