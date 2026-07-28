# loopcoder 0.6.1 customer-ready roadmap

更新时间：2026-07-08

## 依据和复核边界

本 roadmap 不是凭感觉写的。它依据以下当前代码、文档和公开发布状态：

- 本地 `jasonhnd/loopcoder` 已同步到 `origin/main`：
  `6795c90a2cfa715b20cf2c6444b4dec7a0ed3646`
  (`loopcoder promote pre-prod to main`, 2026-07-07 23:29:32 +0900)。
- 本地 tags 没有 `v0.6*`；当前公开 GitHub Releases 页面
  (`https://github.com/jasonhnd/loopcoder/releases`) 显示 latest 是 `v0.5.4`，
  Tags 页面 (`https://github.com/jasonhnd/loopcoder/tags`) 也从 `v0.5.4`
  开始，没有 `v0.6.0` 或 `v0.6.1`。
- 本机当前环境没有 `go` 命令，因此这份 roadmap 没有声称已执行 `go test ./...`；测试项是 0.6.1 开发和发布前必须执行的验收要求。

关键代码事实：

- `loopcoder report` 已在命令表中存在：`internal/cli/cli.go:85-114`。
- `report` flags 已支持 `--repo`、`--work-id`、`--issue`、`--role`、`--limit`、`--format text|json`：`internal/cli/cli.go:4106-4210`。
- `reportquery.Record` 内部已经有 `source`、`run_id`、`path`，但 `MarshalJSON` 目前只输出 `reports`，丢掉这些 metadata：`internal/reportquery/reportquery.go:40-45`、`internal/reportquery/reportquery.go:111-119`。
- `reportquery` 已读取 run records、relay pending、relay ledger，并接受 legacy `[attestation]` header / `attestation` JSON 字段：`internal/reportquery/reportquery.go`。
- `doctor` 当前只有 text 输出，没有 `--format json` flag：`internal/cli/cli.go:862-907`。
- `doctor` 已检查 git、gh、`.delivery.yml`、model selection、providers、origin/default branch、binary、version compatibility、audit、skill、conductor hooks，但没有检查 `.loopcoder/` 是否被 exclude 或是否已被 Git tracking：`internal/doctor/doctor.go:120-157`、`internal/doctor/doctor.go:656-690`、`internal/doctor/doctor.go:1087-1165`。
- `loopcoder init` 当前只解析 `--force`、worker/verifier model/effort，并且总是解析当前目录作为 repo；它没有 `--repo` flag，也没有写 `.git/info/exclude`：`internal/cli/cli.go:910-972`。
- `scaffold.DeliveryTemplate` 当前生成 `gate: auto`：`internal/scaffold/scaffold.go:188-205`。
- runtime promotion gate 对空 gate 仍归一化为 `auto`：`internal/orchestration/promote.go:885-890`。
- local run state 当前明确在 repo 内：`state.RunsRoot(repoPath)` 返回 `<repo>/.loopcoder/runs`，见 `internal/state/state.go:145-147`。
- `skill install` 会写 `.loopcoder/conductor-workspace` marker，但没有写 `.git/info/exclude`：`internal/cli/skill.go:23-29`、`internal/cli/skill.go:81-121`、`internal/cli/skill.go:253-290`。
- reporter pretty 代码把 vendor/provider key 合在一个 `provider` 字段里，比如 `OpenAI Codex / codex`；没有单独 `tool` 行：`internal/reporter/pretty.go`。

关键文档事实：

- README badge 仍是 `v0.5.4`，但 README Status 段仍写 `v0.4.2 is the current...`：`README.md:7`、`README.md:193`。
- stability policy 仍写 `Current stable release: 0.3.7`：`docs/reference/stability-policy.md:6`。
- usage docs 的 pinned install 示例仍是 `0.3.7`：`docs/reference/usage.md:108-124`。
- README 的 mechanical command list 没有列 `loopcoder report`：`README.md:96-109`。
- usage docs 的 Binary Commands 列表没有列 `loopcoder report`：`docs/reference/usage.md:560-646`。
- usage docs 写 pretty block 有单独 `tool` line，但当前代码没有：`docs/reference/usage.md:749-751` 对照 `internal/reporter/pretty.go`。
- README 和 usage docs 多处把 `.loopcoder/` 描述为 gitignored/local state，例如 `README.md:111-116`、`docs/reference/usage.md:209-211`、`docs/reference/usage.md:768-770`；但当前 `init` / `skill install` 代码没有把 `.loopcoder/` 写入 `.git/info/exclude`。

本文中的每个 P0/P1 建议都应能回到这些事实之一。没有代码或文档依据的 0.7.0 产品想法，不放进 0.6.1。

## 定位

0.6.1 是 loopcoder 的 customer-ready bridge release。

它的目标不是提前实现 0.7.0 的新产品形态，而是把 main 上已经出现的 reporter / models / doctor / upgrade 方向整理成一个外部客户可以安装、理解、诊断、升级和安全使用的版本。

一句话目标：

> 0.6.1 必须让客户能放心安装 loopcoder、在自己的 repo 里试跑、知道它做了什么、知道哪些状态不会上传 GitHub，并且在出问题时能用 doctor/report 自己定位。

0.7.0 仍然承接新的产品层能力：

- `loopcoder setup`
- `loopcoder project list/status`
- 全局本地项目数据库
- `DeliveryRun` 一等对象
- `run inspect`
- `plan / continue / decide`
- host callable capability protocol
- sub-agent execution model

0.6.1 不做这些大产品入口，但要为它们铺好发布、诊断、状态安全和文档边界。

## 0.6.1 发布成功标准

0.6.1 可以发布给外部客户，必须同时满足这些条件：

- GitHub release/tag、README、stability policy、usage docs、CHANGELOG 对当前版本的描述一致。
- 0.6.1 发布后，新客户按 README 安装，`loopcoder version` 显示 0.6.1，而不是旧 release。
- `loopcoder doctor --repo .` 能告诉客户当前 repo 是否适合运行 loopcoder。
- `loopcoder skill install --repo .` 和 `loopcoder init` 不会让 `.loopcoder/` 状态被意外提交。
- `.loopcoder/` 如果已经被 Git 追踪，doctor 明确报警，并给出修复命令。
- `loopcoder report` 是文档中的一等命令，text/json 输出契约稳定。
- reporter 的新输出和 legacy attestation 历史状态都能读。
- reporter pretty 文档和实际代码输出一致。
- 旧 `attest` / `conductor-attest` 兼容入口仍可用，并明确标记为 legacy。
- 0.6.1 的首次项目初始化默认安全，客户不会在不理解的情况下自动 promote production。
- loopcoder 自己用 loopcoder 完成 0.6.1 开发、review 和 release 验收。

## 开发方式：loopcoder 开发 loopcoder

loopcoder 本身应该继续 dogfood 自己，但 0.6.1 要用更严格的自举流程。

推荐流程：

1. 创建一个总 issue：`0.6.1 customer-ready bridge release`。
2. 先写并合并一份 spec：`docs/specs/<issue>-customer-ready-bridge.md`。
3. spec 合并后，按本 roadmap 拆 code/docs issue。
4. 每个 code issue 由 loopcoder worker 实现。
5. 每个 PR 必须跑 `loopcoder loopreview`。
6. loopcoder 自己的 repo 必须继续使用 `gate: human-merge`。
7. 不允许 0.6.1 自开发过程中自动 promote production。
8. release 前用 0.6.1 release candidate binary 跑一遍自身 repo 的 `doctor/report/status/audit`。

自举原则：

- loopcoder 可以 dispatch worker。
- loopcoder 可以让 verifier review。
- loopcoder 可以整理 report 和 release checklist。
- 最终 merge、tag、publish release 必须由人批准。

## Epic 0：0.6.1 spec 和范围冻结

目标：

把这份 roadmap 先变成 loopcoder repo 内部正式 spec。因为 loopcoder 的流程是 doc-first，不能直接开代码 PR。

建议文件：

- `docs/specs/<issue>-customer-ready-bridge.md`
- `ROADMAP.md`
- `docs/PROCESS.md` 如需补 release gate 规则

任务：

- 写明 0.6.1 是 customer-ready bridge release。
- 写明 0.7.0 才做产品层重构。
- 写明 0.6.1 必须修复的版本、文档、状态安全、report contract、doctor、release gate。
- 写明本 release 的 legacy compatibility 期限。
- 写明 loopcoder 自开发 loopcoder 时必须 human-merge。

验收标准：

- spec 合并前不能开代码 issue。
- spec 明确区分 0.6.1 和 0.7.0。
- 每个后续 issue 都能引用这份 spec。

推荐优先级：P0。

## Epic 1：版本、release 和文档一致性

目标：

客户看到的版本信息必须一致。现在 main、README、usage docs、stability policy 和 GitHub release 状态不一致，这会直接影响客户信任。

当前依据：

- README badge 是 `v0.5.4`。
- README Status 段说当前版本是 `v0.4.2`。
- stability policy 说当前 stable 是 `0.3.7`。
- usage docs 的 pin 示例是 `0.3.7`。
- 公开 release/tag 目前没有 `v0.6.0` 或 `v0.6.1`。

建议文件：

- `README.md`
- `CHANGELOG.md`
- `docs/reference/stability-policy.md`
- `docs/reference/usage.md`
- `scripts/install.sh`
- `scripts/install.ps1`
- GitHub release notes

任务：

- 把 README badge 更新为 `v0.6.1`。
- 删除或改写 README 中旧的 `v0.4.2 is the current...` 状态段。
- 把 stability policy 的 current stable release 改为 `0.6.1`。
- usage docs 里的 pinned install 示例从 `0.3.7` 改为 `0.6.1`。
- CHANGELOG 增加 `0.6.1`，写清它是 customer-ready bridge release。
- release notes 写清 `0.6.0` 与 `0.6.1` 的关系：
  - 如果 `0.6.0` 没有公开 release，则说明 `0.6.1` 是 reporter transition 的第一个公开客户版本。
  - 如果先补 tag `0.6.0`，则 `0.6.1` 是其 customer-ready patch。
- installer 文档明确 `@latest` / release script 当前会安装哪个版本。

验收标准：

- repo 中当前 release 文案不再同时出现 `0.3.7`、`0.4.2`、`0.5.4`、`0.6.0` 作为“当前稳定版”。
- `README.md`、`stability-policy.md`、`usage.md` 对当前稳定版说法一致。
- GitHub release note 能独立解释客户为什么要升级到 0.6.1。
- 新客户不会误以为 README 描述的能力已经在旧 `@latest` 里可用。

推荐优先级：P0。

## Epic 2：客户首次使用安全默认值

目标：

外部客户第一次运行时，不应该在没理解 loopcoder 行为的情况下进入自动 production promotion。

当前依据：

- `scaffold.DeliveryTemplate` 生成 `gate: auto`。
- runtime `normalizePromotionGate("")` 仍把空 gate 当作 `auto`。
- README 和 usage docs 都明确说 production promotion 默认 automatic。

兼容约束：

- 0.6.1 是 patch/customer-ready release，不应静默改变已有项目或 absent config 的 runtime gate 语义。
- 因此 0.6.1 推荐改变的是新 scaffold/quickstart 的显式默认，而不是 runtime 空值归一化规则。

建议文件：

- `internal/scaffold/scaffold.go`
- `internal/config`
- `README.md`
- `docs/reference/usage.md`
- `.delivery.yml` 示例

任务：

- 新 `loopcoder init` 生成的 `.delivery.yml` 显式写 `gate: human-merge`，让新客户默认安全。
- 不改变已有项目，也不改变 runtime 对缺失 `adapters.gate` 的空值归一化；缺失 gate 仍按现有代码走 `auto`，避免 patch release 破坏旧项目。
- 新增显式 flag，例如 `--gate human-merge|auto` 或 `--auto-promote`，让客户主动选择自动 production promotion。
- README quickstart 使用 `human-merge` 作为推荐路径。
- 文档为 `auto` promotion 增加醒目的 side-effect 说明。
- 自身 repo 保持 `human-merge`，作为 self-hosting safety example。

验收标准：

- 新客户按 quickstart 初始化，不会自动 merge/promotion 到 production。
- 旧项目已有 `.delivery.yml` 的行为不被静默改写。
- 旧项目缺失 `adapters.gate` 的 runtime 行为不在 0.6.1 中改变；若要改变 runtime default，应放到 0.7.0 或更明确的 minor release。
- 文档明确区分 `human-merge` 和 `auto` 的副作用。

推荐优先级：P0。

## Epic 3：本地状态防泄露

目标：

0.6.1 仍然把运行状态放在 repo 下的 `.loopcoder/`，那么必须保证客户不会误把它提交到 GitHub。

当前依据：

- `state.RunsRoot(repoPath)` 使用 `<repo>/.loopcoder/runs`，见 `internal/state/state.go:145-147`。
- `skill install` 会写 `<repo>/.loopcoder/conductor-workspace` marker。
- docs 多处说 `.loopcoder/` 是 gitignored/local-only，但 `init` 和 `skill install` 当前没有写 `.git/info/exclude`。

原则：

- 不把 `.loopcoder/` 写入 tracked `.gitignore` 作为默认方案。
- 默认写本地 `.git/info/exclude`。
- 如果项目已经有 `.gitignore` 忽略 `.loopcoder/`，doctor 可以接受，但不要自动改 tracked 文件。

建议文件：

- `internal/cli/skill.go`
- `internal/cli/cli.go`
- `internal/doctor/doctor.go`
- `internal/doctor/doctor_test.go`
- 可新增 `internal/gitlocal` 或 `internal/localstate`
- `docs/reference/usage.md`
- `README.md`

任务：

- `loopcoder skill install --repo .` 写入 `.git/info/exclude`：
  - 如果不存在 `.loopcoder/` 条目，则追加。
  - 如果已存在，则不重复写。
  - 如果 repo 不是 git repo，则给出清晰 warning。
- `loopcoder init` 也执行同样的本地 exclude 保护。
- `doctor` 增加 `local state git exclusion` 检查：
  - `.loopcoder/` 在 `.git/info/exclude` 中：OK。
  - `.loopcoder/` 在 tracked `.gitignore` 中：OK + 提示本地 exclude 更适合私有状态。
  - 没有任何 ignore/exclude：WARN 或 FAIL。
  - `.loopcoder/` 已有 tracked 文件：FAIL。
- doctor 输出修复命令：
  - `printf '\n.loopcoder/\n' >> .git/info/exclude`
  - `git rm --cached -r .loopcoder`，仅当已经 tracked 时提示，不自动执行。
- docs 把“gitignored `.loopcoder/`”改成更准确的说法：
  - “loopcoder protects local state by adding `.loopcoder/` to `.git/info/exclude` during init/skill install.”

验收标准：

- fresh repo 运行 `loopcoder init` 后，`.git/info/exclude` 包含 `.loopcoder/`。
- fresh repo 运行 `loopcoder skill install` 后，`.git/info/exclude` 包含 `.loopcoder/`。
- 重复运行不会重复追加。
- `.loopcoder/` 被 Git 追踪时，doctor hard fail，并只提示修复命令，不自动 `git rm`。
- 任何 PR、commit、merge commit、GitHub comment 都不会包含 `.loopcoder` 私有状态。

推荐优先级：P0。

## Epic 4：`loopcoder report` 成为稳定客户入口

目标：

0.6.1 要把 `report` 从“代码里有的工具”变成客户能用、host 能解析、文档能解释的一等入口。

当前依据：

- `report` 已在 root command table 中。
- `report` 的 CLI flags 已经基本完整。
- `reportquery.Record` 已经收集 source metadata，但 JSON 输出目前只暴露 `reports`。
- README usage list 和 usage docs Binary Commands 列表没有把 `report` 列进去。

建议文件：

- `internal/reportquery/reportquery.go`
- `internal/cli/cli.go`
- `internal/reporter`
- `docs/reference/usage.md`
- `docs/reference/worker.md`
- `README.md`

任务：

- 在 README command overview 中加入 `loopcoder report`。
- 在 `docs/reference/usage.md` 的 Binary Commands 中加入：
  - `loopcoder report --repo .`
  - `loopcoder report --repo . --work-id <id>`
  - `loopcoder report --repo . --issue <n>`
  - `loopcoder report --repo . --role worker`
  - `loopcoder report --repo . --format json`
- `report --format json` 输出必须包含 source metadata。
- 推荐 JSON 形状：

```json
{
  "records": [
    {
      "source": "attempt",
      "run_id": "run-...",
      "path": ".loopcoder/runs/.../workers/job-1.attempt.json",
      "report": {}
    }
  ],
  "reports": []
}
```

其中 `reports` 是兼容字段，可以继续只包含 report 数组；新 host 应该读 `records`。

- text 输出可以继续保持当前简洁格式，但建议加上 `source` 和 `run_id`。
- 保持读取 legacy `[attestation]` 和 `attestation` 字段。
- 文档写清 report 是 local-only，不上传 PR body/comment/merge commit。

验收标准：

- `report --format json` 对 host 可用，不需要从 path 反推 run/source。
- 兼容字段 `reports` 保留，避免破坏已经按 0.6.0 main 输出解析的脚本。
- 旧 `.loopcoder` 历史记录仍能被查询。
- 空 repo 输出清晰，不报奇怪错误。
- report query 有单元测试覆盖 run records、relay pending、legacy attestation、dedupe、limit/filter。

推荐优先级：P0。

## Epic 5：reporter pretty 文档和代码一致

目标：

当前文档说 pretty output 有单独 `provider` 和 `tool` 行，但代码实际把 provider vendor 和 provider key 合成一行。0.6.1 必须统一。

当前依据：

- docs/reference/usage.md 写 “separate `tool` line”。
- `Report.Pretty()` 只输出 `provider` 字段，`prettyProviderDisplay()` 返回 `OpenAI Codex / codex` 等字符串。

推荐选择：

改文档，不改代码。

理由：

- 0.6.1 是发布收敛版，不应为了展示格式引入新的行为差异。
- 代码当前的 `OpenAI Codex / codex` 形式足够表达 provider + tool。
- 文档错比代码错更容易修。

建议文件：

- `docs/reference/usage.md`
- `README.md`
- reporter pretty tests 如需更新 golden

任务：

- 删除“separate `tool` line”的表述。
- 改成“provider line includes vendor and provider key when available”。
- 文档示例和代码 golden 保持一致。

验收标准：

- 文档示例复制出来和真实 pretty 输出一致。
- 没有客户按文档解析一个实际不存在的 `tool` 行。

推荐优先级：P1。

## Epic 6：doctor 变成客户支持入口

目标：

客户遇到问题时，第一条命令应该是 `loopcoder doctor --repo .`。0.6.1 的 doctor 必须能覆盖安装、配置、provider、reporter、local state 和 legacy transition。

当前依据：

- `doctor` 当前已有 text check 框架和多项环境检查。
- `doctor` 当前没有 JSON 输出。
- `doctor` 当前没有 local state git exclusion / tracked `.loopcoder` 检查。
- `doctor` 当前检查 hooks 是否存在，但没有明确把 reporter transition / legacy alias 作为单独诊断。

建议文件：

- `internal/doctor/doctor.go`
- `internal/doctor/doctor_test.go`
- `internal/cli/cli.go`
- `docs/reference/usage.md`
- `README.md`

任务：

- 增加 local state git exclusion 检查。
- 增加 `.loopcoder/` tracked-file 检查。
- 增加 reporter transition 检查：
  - binary emits `[reporter]`。
  - readers still accept `[attestation]`。
  - installed hooks use `conductor-reporter`。
  - old `conductor-attest` exists时提示 legacy。
- 增加 release/version consistency check：
  - selected binary path。
  - selected version。
  - `.delivery.yml min_loopcoder_version` 是否满足。
- 增加 provider readiness 的客户化提示：
  - codex CLI missing/auth issue。
  - claude CLI missing/auth issue。
  - antigravity login missing。
- 新增 `doctor --format json`，供 host 和客服诊断脚本读取。
- doctor 默认只读，不做修改。

验收标准：

- `doctor` 每个 fail/warn 都有可执行 fix suggestion。
- `doctor --format json` 输出稳定字段：`name/status/message/hard/fix_command`。
- text 输出保持兼容；JSON 是 additive flag，不改变现有默认。
- 无 provider auth 时错误能被客户理解。
- `.loopcoder/` 泄露风险能被 doctor 捕获。

推荐优先级：P0。

## Epic 7：legacy compatibility 延长到真正公开发布之后

目标：

如果 0.6.1 是第一个真正面向客户的 reporter 版本，那么不能在 0.6.1 结束 legacy 窗口。

当前依据：

- 公开 release/tag 当前仍停在 v0.5.4。
- main 代码和 docs 已进入 reporter 迁移，但客户未必已有公开 0.6.0 reporter release。
- 代码当前已经接受 legacy `[attestation]` 和旧 `conductor-attest` alias。

建议：

- `attest` 命令 alias 保留到至少 0.7.x。
- `conductor-attest` hook alias 保留到至少 0.7.x。
- `[attestation]` 读取兼容保留到至少 0.7.x。
- docs 里把 “one-version compatibility” 改成“transition compatibility; removal no earlier than 0.8.0”。

建议文件：

- `internal/cli`
- `internal/conductorhooks`
- `docs/reference/usage.md`
- `docs/specs/0567-reporter.md` 只能改 lifecycle/补 superseding spec；不要直接重写已接受历史 spec
- 新 0.6.1 spec

任务：

- 更新 current docs 的 legacy wording。
- 确保 alias 测试存在。
- doctor 对 legacy alias 给 WARN，不给 FAIL。

验收标准：

- 0.5.x 或早期 0.6.0 本地状态升级后还能读。
- 已安装旧 hook 的客户不会升级后直接 lock out。
- 新安装写入新 hook 名。

推荐优先级：P0。

## Epic 8：安装、升级和 release artifact 验收

目标：

客户不应该需要从源码编译 0.6.1。release artifact、installer、upgrade 路径必须可靠。

当前依据：

- README 和 usage docs 宣称 GitHub Releases 是 no-Go installer 的消费路径。
- 公开 v0.5.4 release notes 显示 release 使用 GitHub Actions artifacts、SHA256SUMS、cosign keyless identity。
- 当前公开 latest 仍是 v0.5.4，因此 0.6.1 文档不能提前暗示 `@latest` 已经能拿到 0.6.1，除非 release 已发布。

建议文件：

- `scripts/install.sh`
- `scripts/install.ps1`
- `internal/upgrade`
- `internal/cli/cli.go`
- `.github/workflows`
- `README.md`
- `docs/reference/usage.md`

任务：

- install scripts 默认解析 GitHub latest release。
- `--version 0.6.1` pin 安装可用。
- Windows PowerShell 安装路径文档更新。
- `loopcoder upgrade --version 0.6.1` 文档可用。
- release artifact 带 checksum。
- 如既有 cosign/SHA256SUMS 机制存在，则 0.6.1 release 必须走同样机制。
- release note 附 smoke-test 命令：
  - `loopcoder version`
  - `loopcoder doctor --repo .`
  - `loopcoder models`
  - `loopcoder report --repo .`

验收标准：

- macOS/Linux/Windows 至少各有一条安装路径。
- 安装后 PATH 指引明确。
- 从 0.5.4 升级到 0.6.1 的路径明确。
- 失败时 installer 不留下半安装状态。

推荐优先级：P0。

## Epic 9：客户文档重写

目标：

0.6.1 文档必须从“内部控制台说明”转向“客户能完成第一次成功运行”。

建议文件：

- `README.md`
- `docs/reference/usage.md`
- `docs/reference/worker.md`
- `docs/reference/architecture.md`
- `docs/reference/stability-policy.md`
- 可新增 `docs/reference/troubleshooting.md`

任务：

- README 首屏说明：
  - loopcoder 是什么。
  - 安装。
  - 第一次运行。
  - 安全默认值。
  - 怎么看结果。
  - 怎么诊断。
- usage docs 增加 command side-effect table：
  - read-only。
  - local write。
  - provider/token spend。
  - GitHub write。
  - PR/merge/promotion。
- 明确 `report/status/doctor/audit/models` 是客户排障入口。
- 明确 `.loopcoder/` 是 local state，不应提交。
- 增加“0.6.1 不包含什么”：
  - 不包含全局项目数据库。
  - 不包含 0.7.0 的 `setup/project/run inspect`。
  - 不包含 sub-agent execution。
- 增加 troubleshooting：
  - provider command missing。
  - auth missing。
  - `.loopcoder` 被 tracked。
  - stale hooks。
  - missing reports。
  - relay pending blocks。

验收标准：

- 新客户只读 README 就能完成安装和 doctor。
- 使用文档能解释每个命令的副作用。
- 文档不再把内部术语当作唯一入口。

推荐优先级：P0。

## Epic 10：测试和 CI release gate

目标：

0.6.1 是客户发布版，不能只靠静态阅读。必须有 release gate。

当前依据：

- 本次笔记修订环境没有 `go` 命令，因此这里列的是 0.6.1 发布前必须由开发环境/CI 执行的 gate。
- CHANGELOG 和过往 release notes 已经把 full CI、staticcheck、govulncheck、audit 作为 release discipline 的一部分。

建议文件：

- `.github/workflows`
- `internal/*_test.go`
- `docs/*`
- `scripts/*`

任务：

- 必跑：
  - `go test ./...`
  - `go test -race ./...`，如耗时太长至少对核心 packages 跑
  - `go vet ./...`
  - `staticcheck ./...`
  - `govulncheck ./...`
- 增加或确认测试：
  - report JSON includes `records` source metadata。
  - report reads legacy attestation。
  - doctor catches missing `.loopcoder/` exclude。
  - doctor catches tracked `.loopcoder` files。
  - skill install writes `.git/info/exclude` idempotently。
  - init writes `.git/info/exclude` idempotently。
  - README/usage command matrix includes all root commands。
  - pretty docs/golden output consistent。
  - legacy hook aliases still work。
- release candidate smoke：
  - fresh temp repo。
  - run `loopcoder init`。
  - run `loopcoder skill install`。
  - run `loopcoder doctor --repo .`。
  - run `loopcoder report --repo .` on empty state。

验收标准：

- CI 全绿。
- 本地 release checklist 有记录。
- 没有 `.loopcoder/` 私有状态进入 tracked files。
- `git diff` review 后再 tag。

推荐优先级：P0。

## Epic 11：host 调用的最小铺路

目标：

0.6.1 不实现完整 host callable protocol，但要让 Paseo / Codex / Claude Code 这类 host 至少能稳定调用只读诊断命令。

当前依据：

- `report --format json` 已有，但缺少 source metadata。
- `doctor` 没有 JSON 输出。
- 文档没有统一 side-effect table，host 只能从命令名和 prose 猜副作用。

范围：

- 做 `doctor --format json`。
- 稳定 `report --format json`。
- 文档写 side-effect table。
- 不做 `capabilities --format json`，留给 0.7.0。

建议文件：

- `internal/cli/cli.go`
- `internal/doctor`
- `internal/reportquery`
- `docs/reference/usage.md`

任务：

- 定义 0.6.1 host-safe commands：
  - `version`
  - `doctor --format json`
  - `models`
  - `status`
  - `report --format json`
  - `audit --layer sast`
- 文档标注这些命令 read-only 或 local-only。
- 对会写 GitHub、会花 token、会 merge/promotion 的命令标注需要用户批准。

验收标准：

- host 可以安全调用只读命令获取状态。
- host 不需要猜测 `report` JSON 结构。
- 0.7.0 capability protocol 有明确前置基础。

推荐优先级：P1。

## Epic 12：0.7.0 bridge 文档

目标：

0.6.1 发布时，客户和开发者要知道 0.7.0 将解决什么，而不是误以为 0.6.1 已经是最终体验。

建议文件：

- `ROADMAP.md`
- 可新增 `docs/reference/0.7-preview.md`

任务：

- 写清 0.7.0 的 planned capabilities：
  - global local state store。
  - project registry。
  - setup。
  - DeliveryRun。
  - run inspect。
  - plan/continue/decide。
  - capability protocol。
  - sub-agent orchestration。
- 写清 0.6.1 和 0.7.0 的边界。
- 写清 0.6.1 的数据不会被自动迁移，0.7.0 会提供 migration plan。

验收标准：

- 客户不会把 0.6.1 当作 0.7.0。
- 开发者知道 0.7.0 从哪里接。

推荐优先级：P1。

## 推荐 issue 切分

按照 loopcoder 的 doc-first 流程，推荐这样切：

1. `doc: 0.6.1 customer-ready bridge spec`
2. `docs: align 0.6.1 release/version/customer docs`
3. `code: protect .loopcoder local state with git info exclude`
4. `code: add doctor local-state and reporter-transition checks`
5. `code: stabilize report json records output`
6. `docs: make report a first-class customer command`
7. `docs: fix reporter pretty output mismatch`
8. `code: safer init defaults for customer repos`
9. `docs: customer quickstart and command side-effect table`
10. `code: doctor --format json`
11. `test: 0.6.1 release gate and docs/command inventory tests`
12. `release: 0.6.1 artifacts, release notes, smoke test`

依赖顺序：

```text
1
├─ 2
├─ 3 ─ 4
├─ 5 ─ 6
├─ 7
├─ 8 ─ 9
├─ 10
└─ 11 ─ 12
```

如果要压缩发布节奏，最低限度也不能少于：

- 1 spec
- 2 version/docs consistency
- 3 local state protection
- 4 doctor checks
- 5 report JSON/docs
- 8 safer init defaults
- 11 release gate
- 12 release

## 0.6.1 不应做的事

这些事情很重要，但不应该塞进 0.6.1：

- SQLite 或其他全局数据库。
- repo identity/project registry。
- 迁移 `.loopcoder/runs` 到 `~/.loopcoder/projects`。
- `loopcoder setup`。
- `loopcoder project`。
- `loopcoder run inspect`。
- `loopcoder plan`。
- `loopcoder continue`。
- `loopcoder decide`。
- sub-agent execution。
- 完整 host capability protocol。

原因：

0.6.1 要变成客户可用发布版，不是再做一轮产品层大改。把这些放进 0.6.1 会让 release 风险变高，也会让 0.7.0 的边界不清楚。

## Release checklist

发布 0.6.1 前逐项确认：

- `go test ./...` 通过。
- race/staticcheck/govulncheck 按项目规则通过。
- `loopcoder doctor --repo .` 在 loopcoder 自身 repo 上无 P0 fail。
- `loopcoder report --repo . --format json` 输出稳定 JSON。
- fresh temp repo `init` 后 `.git/info/exclude` 包含 `.loopcoder/`。
- fresh temp repo `skill install` 后 `.git/info/exclude` 包含 `.loopcoder/`。
- `.loopcoder/` 没有 tracked files。
- README 当前版本是 0.6.1。
- stability policy 当前稳定版是 0.6.1。
- usage docs pinned install 示例是 0.6.1。
- Binary Commands 包含 `loopcoder report`。
- reporter pretty 文档和代码一致。
- release notes 写明 legacy attestation compatibility。
- release notes 写明 0.7.0 才会做产品层新入口。
- GitHub release artifacts、checksums、install scripts 全部验证。
- 最终 tag/publish 由人批准。

## 最终建议

0.6.1 应该是一个“外部客户可用”的硬化发布：

- 对客户：安装清楚、默认安全、出错能诊断、状态不会误上传。
- 对 host：只读诊断和 report JSON 足够稳定。
- 对 loopcoder 自己：能继续用自己开发自己，但最终 release gate 由人控制。
- 对 0.7.0：不抢产品层工作，只把地基铺平。

我推荐把 0.6.1 的 release slogan 写成：

> Customer-ready reporter release: safer local state, stable reports, stronger doctor, clearer docs.
