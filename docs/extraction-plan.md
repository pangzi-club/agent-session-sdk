# agent-session-sdk 抽取计划

将 agent-view 与 achat 中公共的「agent 对话列表 / 对话详情 / 用量统计」逻辑抽取为独立 Go 库 + CLI，两个应用改为消费本库，删除各自重复实现。

## 1. 现状盘点

同一套扫描逻辑目前存在三份实现，能力各有缺项：

| 实现 | 语言 | 对话列表 | 对话详情 | 用量（token/credit） | 计价（USD） |
|---|---|---|---|---|---|
| `agent-view/src/main/agents/` + `usage/` | TS | ✅ 14 agents | ❌ | ✅ | ✅ 价格表 + 报告聚合 |
| `agent-view/scanner/` | Go | ✅ | ❌ | ✅ records + summary | ❌（由 TS 侧挂载） |
| `achat/agentlog/` | Go | ✅（移植自 scanner） | ✅ transcript + LRU 缓存 | ❌（TokenUsage 无人填充） | ❌ |

关键事实（已核实）：

- 数据源 14 个：claude、codex、gemini、cursor、opencode、zcode、workbuddy、pi、dsh、grok、qoder、qoder-cn、kimi、hermes（qwen 仅检测）。注册表见 `agent-view/scanner/internal/scansession/registry.go:22` 与 `achat/agentlog/registry.go:16`。
- `achat/agentlog/` 注释明确是 scanner 的移植（`contract.go:1-3`），零 UI 依赖，可直接 import；但**没有用量逻辑**。
- `agent-view/scanner/` 已有独立 CLI（`cmd/agent-view-scanner/main.go`：sessions / usage / summary 子命令，stdout JSON），agent-view 经 `src/main/scan-bridge.ts` 子进程调用，失败回落 TS；`scripts/parity-scan.mjs` 保证双引擎逐字段一致。
- 输出契约以 `agent-view/src/ipc-contract.ts` 为准，Go 侧 `scanner/internal/contract/contract.go` 逐字段对齐（float64、omitempty 与显式 null 的区分都有明确约定）。
- 计价只在 TS：`usage/model-prices.ts`（models.dev 快照，USD/1M tokens）、`usage/pricing.ts:108` `estimateTokenCost`、Qoder credit 单价。报告聚合 `usage/report.ts:41` `buildUsageReport` 是 UI 侧逻辑。
- 对话详情只在 achat：`agentlog/transcript.go:54` `LoadTranscript` 按 agent 分发，模型 `Entry{Role,Text,Tools,Timestamp}`、`ToolCall{Name,Title,Meta,Kind}`，带 size+mtime 签名失效的 LRU `Cache`。
- 本仓库远端 `github.com/pangzi-club/agent-session-sdk`（私有）已建。

## 2. 目标与非目标

**目标**

- 一个可 import 的 Go 库：对话列表、对话详情（transcript）、用量统计（token / credit / USD cost）。
- 一个无状态 CLI：一次性扫描、stdout 输出 JSON，单文件分发、交叉编译。
- achat 直接 import 库；agent-view 以子进程消费 CLI（沿用现有 bridge 模式）；两边删除重复代码。

**非目标**

- 会话删除（留在 agent-view，契约中 `canDeleteSessions` 字段保留兼容）。
- UI 报告聚合（`buildUsageReport` 的周时热力/工具/技能报表留在 agent-view TS）。
- 文件监听 / daemon 模式；缓存持久化（缓存仍由各应用自己管）。

## 3. 关键决策

**D1 — 用 Go。** achat 是 Go 可直接 import；agent-view 已有 Go 子进程桥接模式；依赖纯 Go 无 cgo（modernc.org/sqlite、klauspost/compress、x/sync），单二进制易分发。TS 库无法服务 achat，不选。

**D2 — 扁平根包 `agentsession`。** module 为 `github.com/pangzi-club/agent-session-sdk`。沿用 agentlog 已验证的扁平布局（contract / shared / registry / 各 agent 文件同包），避免 contract↔shared↔scanners 之间的循环 import；scanner 的 `internal/` 结构无法被外部 import，不沿用。

**D3 — JSON 契约与 ipc-contract.ts 逐字段对齐。** 保留 float64、omitempty / 显式 null 约定（`contract.go:1-11` 的注释规范），使 agent-view 换二进制后 TS 零改动。transcript 输出是新增契约，给 achat 的 `Entry`/`ToolCall` 补 JSON tag。

**D4 — 代码来源：列表 + 用量以 scanner 为准，transcript 以 achat 为准。** scanner 更新且有用量；agentlog 可能有移植后的独立修复，S1 先 diff 两边同名文件确认漂移再合并。S1 实测：两侧除 §9 四类增强外代码逐声明一致，会话层基座取 achat 版（注释英文、扁平包），用量层（S2）仍以 scanner 为准。

**D5 — 计价移植进库。** 价格表（models.dev 快照）转 JSON 用 `go:embed` 内嵌，移植 `estimateTokenCost` 与模型名归一化、Qoder credit 单价。基础契约不变，cost 作为 CLI 可选输出（`--cost`），避免影响现有 parity。

**D6 — 私有 GitHub 仓库，消费方直接引源码。** 代码 push 到 `github.com/pangzi-club/agent-session-sdk`（私有）后：achat 通过 Go module 直接 import（配 `GOPRIVATE=github.com/pangzi-club/*` + git SSH 访问）；agent-view 的 dev/打包脚本用 `go build` / `go install` 从模块源码构建 CLI。不依赖公开 Release 产物，消费方按 tag 固定版本。

## 4. 仓库布局

```
agent-session-sdk/
  go.mod                        # github.com/pangzi-club/agent-session-sdk, go 1.27
  contract.go                   # SessionInfo/SessionScanResult/UsageRecord/UsageScan/AgentUsageSummary
  shared.go                     # ScanLines/ToTitle/DecodePathDirName/OpenSQLite/QueryMaps/SessionTally…
  registry.go                   # agent 注册表 + ScanSessions 并行编排 + IsKnownAgent
  claude.go codex.go …          # 14 个会话扫描器（每 agent 一文件）
  transcript.go                 # LoadTranscript 分发 + Entry/ToolCall + LRU Cache
  transcript_claude.go …        # 各 agent transcript 解析（来自 achat）
  usage.go usage_claude.go …    # ScanAllUsage + 各 agent 用量提取（来自 scanner/scanusage）
  summary.go                    # Summarize 按 agent 聚合
  pricing.go                    # EstimateTokenCost / 模型名归一化 / credit 单价
  model-prices.json             # go:embed 价格表（从 TS model-prices.ts 转写）
  *_test.go                     # 从两个仓库移植 + 新增
  cmd/agent-session-sdk/main.go # CLI 入口
  docs/extraction-plan.md       # 本文档
  README.md
```

## 5. 公共 API（根包 agentsession）

```go
func ScanSessions(home string) []SessionScanResult           // 对话列表
func LoadTranscript(agentID string, info SessionInfo) ([]Entry, error) // 对话详情
func NewCache(max int) *Cache                                // transcript LRU 缓存
func ScanAllUsage(home string) UsageScan                     // 用量 records + sessions
func Summarize(scan UsageScan) []AgentUsageSummary           // 按 agent 聚合
func EstimateTokenCost(model string, tokens TokenCount) (usd, unpricedTokens float64) // 计价
```

签名与现有两侧保持一致，迁移时基本是机械改名。

## 6. CLI 设计

`agent-session-sdk <subcommand> [flags]`，通用 flags：`--home` / `--agent ids` / `--pretty`（沿用 scanner 语义与英文 help）。

| 子命令 | 输出 | 来源 |
|---|---|---|
| `sessions` | SessionScanResult[] JSON | 与 agent-view-scanner 完全同契约（drop-in） |
| `usage` | {records, sessions} JSON | 同上；`--cost` 可选追加 costUsd 字段 |
| `summary` | AgentUsageSummary[] JSON | 同上 |
| `transcript --agent X --session Y` | Entry[] JSON | 新增（会话定位复用列表扫描结果） |

## 7. 分阶段实施

每阶段独立验证 + 一个 commit，汇报后等「继续」再推进。

- **S0 骨架**（已完成）：go.mod、README、contract.go（含 transcript JSON 契约）、空 registry。验证 `go build ./...`。
- **S1 对话列表**（已完成）：漂移门禁已跑——两侧归一化后除 §9 的 4 类 achat 增强外代码完全一致，故基座取 achat 版（注释英文、扁平包），补回 scanner 独有的 registry 编排（`canDelete` / `CanDeleteSessions`）；`shared/tally.go` 只服务用量，留 S2。移植 achat 的 shared_test + scan_test（两侧 shared 测试本就一致）。验证：单测全绿 + 与旧 scanner `sessions` 输出比对，差异恰好等于 §9 登记项（真实 home 368 会话、夹具树 40 会话覆盖 14 agent，均零意外差异）。
- **S2 用量**（已完成）：移植 scanusage 全部（11 个用量模块）+ summary + tally + interval 方法 + qoder_test，注释英文化，AST 校验与 scanner 原版逐声明一致（仅去重 `strPtr`）。验证：单测全绿；`usage` / `summary` 与旧 scanner 在真实 home（9932 records / 367 sessions）与夹具树（24 records / 36 sessions）逐字段一致，差异仅 §9 登记项（真实 home：qoder-cn userPrompts 74 + activeMs 26 为 #2 连带效应；夹具：grok filePath + hermes workspace）。用量覆盖 11 个 agent（无 cursor/gemini/kimi——TS 侧本就没有）。
- **S3 对话详情**（已完成）：从 achat 移植 transcript.go（LoadTranscript 分发 + Cache）+ 8 个解析器 + transcript_test；Entry/ToolCall 用 contract.go 带 JSON tag 的定义（删 achat 侧无 tag 重复）。AST 校验与 achat 逐声明一致。验证：单测全绿 + 双引擎（本库 vs achat agentlog）对真实 home（16 会话）与夹具树（23 会话，14 agent）LoadTranscript 逐 entry DeepEqual 一致。
- **S4 计价**（已完成）：model-prices.ts → model-prices.json 转写（137 模型，`order` 数组保 TS 声明序）+ go:embed；移植 normalizeModelId / LookupModelPrice（含点号变体与别名）/ EstimateTokenCost / credit 单价 / PricingVersion（FNV-1a 32 位，逐位对齐 TS：1191524259，`init()` 末尾计算——包级 var 初始化先于 init，踩过坑已注释）。注意转写时 `o3` 是表里唯一无引号 key，正则别漏。验证：pricing_test 全绿（含与 TS 版本哈希锁定）+ 300 组随机输入（credits/负值/前缀/别名/未定价）与 TS estimateTokenCost 逐位一致。
- **S5 CLI**（已完成）：cmd/agent-session-sdk/main.go——sessions / usage / summary / transcript 四个子命令 + 英文 help（如实列出会话 14 / 用量 11 的覆盖差异，修正旧 help 漏 cursor/hermes 的历史问题）；通用 flags --home / --agent / --pretty，transcript 需 --agent + --session，usage 支持 --cost 追加 costUsd / costUnpricedTokens（基础输出不变）。验证：与旧 CLI 三命令在真实 home + 夹具树逐字段 diff，差异恰好 = §9 登记项；transcript 对 claude/qoder-cn（JSONL）/ hermes（SQLite）冒烟通过。Makefile 补 build-cli（输出 ./bin）。
- **S6 achat 迁移**（已完成）：go.mod 引入本库（S8 打 tag 前用 replace 指向本地路径），`agentlog.` → `agentsession.` 机械替换（6 文件 51 处），删除 agentlog/（-4052 行）。验证：achat 构建 + vet + 全部测试 + 启动冒烟（368 会话 / 33ms，与迁移前一致）。agent-view 侧 commit：`refactor: consume agent-session-sdk, drop the agentlog port`。
- **S7 agent-view 迁移**（进行中，agent-view 仓库 3 个 commit）：a) TS 回合并 §9 四处增强 + 同步 2026-11-08 价格表（`feat: merge back the agent-session-sdk session enhancements`，TS↔新库 parity 双场景全绿：夹具树 + 真实 home 10190 records）；b) scan-bridge / dev.mjs / pack-scanner.mjs / package.json 指向新 CLI（`feat: wire the scan bridge to the agent-session-sdk CLI`，AGENT_SESSION_SDK_DIR 可覆盖兄弟仓库路径；bridge 冒烟 14 agents / 368 sessions / 10213 records）；c)（已完成，`refactor: delete the TS scan layer and in-repo scanner`，-11727 行）：删 scanner/ + TS 扫描层 + pricing.ts/model-prices.ts + parity/bench 脚本；agent 模块缩为 def（claude/codex 留 deleteSessions）；scan-bridge 删 TS 回落与 env kill-switch（缺失即报错）；cost 全链路改吃 `usage --cost` 内联字段，pricingState 持表版本（缓存语义不变）；缓存版本 sessions 16→17、usage 12→13；AGENTS.md / README×2 / skills / docs 同步指向本仓库。验证：pnpm check 全绿（73 测试）+ bridge 冒烟（14 agents / 368 sessions / 10291 records 全带 cost，pricingVersion 64204264）。
- **S8 发布**：push 到私有 GitHub 并打 tag（v0.x）；消费方固定版本——achat `go get github.com/pangzi-club/agent-session-sdk@vX`，agent-view 构建脚本 `go install …/cmd/agent-session-sdk@vX`；README 写明私有库访问配置（GOPRIVATE、git SSH insteadOf）。

## 8. 风险与开放问题

- **agentlog 与 scanner 漂移**：S1 门禁已跑完，结论见 §9；4 类 achat 增强全部采纳，S7 需回合并到 agent-view TS 才能让 parity 全绿。
- **价格表维护**：models.dev 快照后续更新只改本库 JSON。2026-11-08 已按 magpie 缓存（当日刷新）从快照 2026-10-05 → 2026-11-08 更新：+7 新模型（claude-haiku-5-5 / gemini-4-argon / gemini-pro-latest / gpt-6.1-terra / kimi-k3.1-pro ×2 / minimax-m3.1-flash）、-2 下架（gpt-5.2/5.3-chat-latest）、23 处改价（glm 系第一方真实降价 14 处、qwen 系 cacheWrite 补录等）。甄别要点：同名跨 provider 取第一方（deepseek-v4-flash-0731 第一方已下线，现用 alibaba 网关价 0.44/1.32）；lyria-3 输出含 audio 按规则排除；claude 日期版与裸名并存时剔除日期版。PricingVersion 随表变为 64204264（node 同算法独立复算一致）。
- **agent-view 删 TS 的时机**：parity 通过后再删（S7 内分两个 commit：先接线、后删除），不保留长期双引擎。
- **私有库访问**：消费方需配好 GOPRIVATE 与 GitHub SSH 权限（D6），否则 `go get`/`go install` 会走公开 proxy 失败；README 给出一行配置说明。
- **cursor/hermes 覆盖**：旧 scanner CLI help 未列 cursor/hermes 但会话注册表有；scanusage 目录里没有 cursor/gemini/kimi/zcode 之外的缺项需 S2 核实（用量覆盖面小于会话覆盖面），S5 写 help 时按实际覆盖列。
- 命名已定（S0）：module `github.com/pangzi-club/agent-session-sdk`、包名 `agentsession`、CLI `agent-session-sdk`。原 Swift 平行移植已删除，该名字唯一归本库。

## 9. 已登记偏差（S1 采纳的 achat 增强）

agent-view 的 TS 实现与旧 scanner 都没有这些行为；采纳后 `sessions` 输出与旧 scanner 存在且仅存在这几类差异。S7 需把它们回合并到 agent-view TS，parity 才能恢复逐字段一致。

| # | 改动 | 涉及 agent | 输出差异 |
|---|---|---|---|
| 1 | 会话补 `FilePath`（S3 transcript 定位必需） | gemini / grok / kimi | `sessions` 多 `filePath` 键 |
| 2 | `QoderUserPromptText` 先剥 `<system-reminder>` 再判注入，返回修剪后文本 | qoder / qoder-cn | 标题更准（真实 home 上 16 处变化，含 null → 真实标题）；**连带效应（S2 实测）**：被误判注入的提示不再漏计——同一批会话的 `usage` 记录 `userPrompts` 恢复计数（74 条）、`activeMs` 增加对应活动时间（26 条），summary 的 activeMs 同步变化 |
| 3 | hermes 无 cwd 时 workspace 伪路径分隔符 | hermes | `Hermes · x` → `Hermes \| x`（真实 home 231 处；· 在 achat 文案规则里禁用） |
| 4 | 新增 `stripLeadingSystemReminder` 辅助函数 | qoder.go | 无直接输出差异（服务 #2 与 S3 transcript） |

验证证据：真实 home 368 会话、夹具树 40 会话（14 agent 全覆盖），逐字段比对差异全部归入上表，零意外。