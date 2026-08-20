# Conductor — 多 Agent 协作服务 设计方案 (v0.3)

> 状态: 草案(v0.3)。v0.1 → v0.2 → v0.3 主要变化见末尾"版本变更"。本仓库刚刚创建,本文件是第一份设计记录。所有架构决策以"对照 Paseo / Multica 真实代码 + 用户原始分层"为依据,后续讨论请直接编辑本文件或在 Issue 中附 diff。

## 0. 一句话定位

Conductor 是一个 **本地常驻的 Player Daemon**,把 Claude Code / Codex / Pi 等"已经能跑的 agent CLI"包成统一接口,在此之上提供:
- **可编排的工作流**(单 agent / 串行 / 并行 / 条件 / 循环,以及 PDCA 周期)
- **可观测的运行时**(统一日志、timeline、附件、子 agent 关系)
- **可扩展的 Provider 层**(内置 SDK + ACP 基类 + HTTP/RPC 自定义)

Conductor **不**自研 LLM agent 框架,**不**训练模型,**不**做模型路由优化。它只做"宿主 + 编排 + 观测"。

## 1. 竞品对照(已落到代码的事实)

| 维度 | Paseo | Multica | Conductor (拟) |
|---|---|---|---|
| 形态 | 本地 daemon + 跨端 client | Go 服务 + Web/Electron/Expo + 自托管 | 本地 daemon + CLI/Web(可选) |
| 语言栈 | TypeScript (Node) | Go (server) + TS (前端) | TypeScript (Node) — 与生态对齐 |
| Provider 抽象 | `AgentClient` 接口 + ACP 基类 | `Runtimes` 模块 + 健康度派生 | 同 Paseo 模式,见 §5 |
| 编排层 | prompt 模板 + provider-native subagent | `Autopilots`(定时/触发器)+ workflow DSL | Worker 图 + PDCA 引擎,见 §6 |
| 持久化 | 文件 JSON + Zod | Postgres + sqlc | 文件 JSON(Paseo 模式),Postgres 推迟 |
| 子 agent | `ProviderSubagentStore` 仅做跟踪,实执行交给 provider | 多 assignee + runtime binding | 同 Paseo:Store 仅跟踪,执行交给 provider |
| 远程 | 可选 E2E relay | 服务端 + Hub | 先单 host,Hub 推迟 |
| MCP | 是 MCP host + 允许 provider 暴露 MCP | `MCP support` 模块 | 同 Paseo |

核心借鉴决策:
- **Paseo 的 `AgentClient` + `AgentSession` 双层接口** 是 provider 抽象的最优解,直接采用。
- **ACP 作为通用扩展点**,让任意"愿意走 JSON-RPC/stdio"的 agent 即插即用。
- **Subagent store 单独抽出**,只做跟踪与展现,不碰实执行。这是和"自己实现一个跨 provider 子 agent 引擎"的最大区别 —— 后者几乎一定重复造 Claude Task 的轮子。

## 2. 分层架构(对照用户原始分层)

```
┌─────────────────────────────────────────────────────────┐
│  Agent Gateway  (HTTP/WS + CLI + Web UI 可选)            │  ← 用户/外部输入
├─────────────────────────────────────────────────────────┤
│  Agent Workflow Engine  (PDCA 周期 + 动态子任务生成)     │  ← 用户说"每阶段动态生成 worker task, PDCA 循环"
├─────────────────────────────────────────────────────────┤
│  Agent Worker  (编排图节点:单 / 串 / 并 / 条件 / loop)    │  ← 用户原始分层中的"编排层"
├─────────────────────────────────────────────────────────┤
│  Agent Runner  (生命周期 / 流式事件 / 持久化 / 上下文)    │  ← 一个 Agent 实例的运行宿主
├─────────────────────────────────────────────────────────┤
│  Agent Provider  (Claude / Codex / Pi / ACP / Custom)    │  ← 用户原始分层中的"Provider 层"
├─────────────────────────────────────────────────────────┤
│  Player Daemon + Registry (单 host 长期进程 + 注册中心)  │  ← 单机 Player-daemon 架构
└─────────────────────────────────────────────────────────┘
```

每层都通过协议(`@conductor/protocol`)定义的 wire 类型交互,允许替换实现。

## 3. 仓库布局(monorepo)

```
conductor/
├─ packages/
│  ├─ protocol/         # wire 类型 + Zod schema + 消息编解码
│  ├─ provider/         # AgentClient/AgentSession 接口 + 内置 provider 实现
│  │  ├─ base/          #   ACPAgentClient, NativeSdkAgentClient 基类
│  │  ├─ claude/        #   Claude Code SDK 适配
│  │  ├─ codex/         #   Codex app-server 适配
│  │  ├─ pi/            #   Pi RPC 适配
│  │  ├─ omp/           #   OMP 适配(Paseo 同名)
│  │  └─ acp-generic/   #   通用 ACP(供 copilot/cursor/kimi/kiro 等)
│  ├─ runner/           # Agent Runner:生命周期、事件流、resume、清理
│  ├─ worker/           # 编排图节点:DAG、sequence、parallel、switch、loop
│  ├─ workflow/         # 工作流引擎:PDCA 周期、阶段、子任务动态生成
│  ├─ registry/         # Player Registry:本机 agent/provider 注册表
│  ├─ storage/          # 文件 JSON + Zod(Paseo 模式),后续可换 SQLite
│  ├─ gateway/          # HTTP/WS 服务 + Web UI(可选)
│  └─ cli/              # conductor run / send / ls / logs / wait / workflow
├─ apps/
│  ├─ daemon/           # Player Daemon 入口
│  └─ web/              # 可选 Web UI(Phase 3+)
├─ examples/            # 工作流样例
├─ docs/                # 设计文档(本目录)
├─ package.json
└─ pnpm-workspace.yaml
```

> 借鉴 Paseo 的 npm workspace 模式,因为 provider SDK 全部是 npm 生态。

## 4. Agent Spec — 一个 agent 的全部静态定义

参考 Paseo `AgentSessionConfig`,固定以下字段(便于 wire/持久化/Profile 复用):

```ts
type AgentSpec = {
  // 身份
  id: string;                 // spec 自身的 id,可被多次实例化
  version: number;            // spec 版本号,变更后已存在的实例保留旧 spec

  // Provider 选择
  provider: "claude" | "codex" | "pi" | "omp" | "acp:<vendor>" | "custom:<id>";
  profile?: string;           // 多 profile 支持(Paseo 模式,例如 Z.AI 继承 claude)
  model?: string;             // provider 模型 id,缺省 = provider 默认

  // 行为
  systemPrompt?: string;      // 直接给定;否则由 skill 注入
  skills: string[];           // 引用 orchestration skill 名称(SKILL.md)
  mcp: {                      // 该 agent 要挂载的 MCP server 列表
    servers: Array<{ name: string; transport: "stdio" | "http"; ... }>;
  };
  toolPolicy?: ToolPolicy;    // 工具白/黑名单 + 预批准
  customArgs?: string[];      // 透传给 provider CLI 的额外参数
  env?: Record<string, string>;

  // 工作环境
  cwd: string;                // 工作目录
  worktree?: {                // 可选:在 cwd 上新建 git worktree 隔离
    branch: string;
    baseBranch?: string;
  };
  attachments?: Attachment[]; // 首轮附件(图片/pdf/文件片段)
};
```

> 这是 `provider + skill + mcp + custom args + prompt` 的统一封装。Paseo 的 `AgentSessionConfig` + `providerOptions` + `toolPolicy` 三块合并后得到上面的形状。

## 5. Agent Provider 层

### 5.1 抽象接口(对照 Paseo `agent-sdk-types.ts`)

```ts
// === Provider 抽象 ===
interface AgentClient {
  readonly provider: AgentProvider;
  readonly capabilities: AgentCapabilityFlags;

  createSession(
    config: AgentSessionConfig,
    launchContext?: AgentLaunchContext,
    options?: AgentCreateSessionOptions,
  ): Promise<AgentSession>;

  resumeSession(
    handle: AgentPersistenceHandle,
    overrides?: Partial<AgentSessionConfig>,
    launchContext?: AgentLaunchContext,
  ): Promise<AgentSession>;

  fetchCatalog(options: FetchCatalogOptions): Promise<ProviderCatalog>;
  isAvailable(): Promise<boolean>;

  // 可选:发现可导入的历史会话
  listImportableSessions?: (...) => Promise<ImportableProviderSession[]>;
  importSession?: (...) => Promise<ImportedProviderSession>;
}

interface AgentSession {
  readonly id: string;
  readonly provider: AgentProvider;
  send(prompt: AgentPrompt, options?: SendOptions): Promise<AgentTurnResult>;
  stream(): AsyncIterable<AgentStreamEvent>;
  cancel(): Promise<void>;
  rewind(options?: RewindOptions): Promise<void>;
  persist(): Promise<AgentPersistenceHandle>;
  close(): Promise<void>;
}
```

> 这是 Paseo 已经验证过的契约,我们直接采用。

### 5.2 Provider 实现分类(三种接入策略)

| 类型 | 适用 | 实现 | 示例 |
|---|---|---|---|
| **Native SDK** | 提供官方 SDK 且能力最强 | 直接调 SDK,自己管进程 | Claude Code SDK, Codex app-server |
| **ACP** | 任何愿意走 stdio JSON-RPC 的 agent | 继承 `ACPAgentClient`,只填 command+argv+env | Pi, OMP, copilot, cursor, kimi, kiro, trae, 自定义 |
| **HTTP / RPC** | 远程 agent 服务 | 实现一个轻量 wrapper,转成本地契约 | 内部 HTTP agent、第三方云 agent |

> Paseo 实测 10+ provider 都跑得稳。我们采用相同的"两路 SDK + 一路 HTTP"格局。

### 5.3 Provider Registry 与 Profile

```ts
// 参考 Paseo provider-registry.ts
const ProviderDefinition = {
  id: "claude",
  enabled: true,
  optionsSchema: ClaudeProviderOptionsSchema,  // Zod
  derivedFromProviderId: null,                 // 继承链(自定义 profile 用)
  createClient: (logger) => new ClaudeAgentClient({...}),
  // ...
};

const registry = buildProviderRegistry({
  runtimeSettings: loadRuntimeSettings(),
  providerOverrides: loadCustomProviders(),   // 用户配置: extends: "claude"
});
```

**Profile / 扩展机制**(直接借鉴 Paseo 的 `extends: "claude"` 语义):允许声明"我的 zai-profile 继承 claude,只换 command/argv/env 和 API base"。这覆盖了用户原始需求中的"对外暴露统一接口"。

## 6. Agent Runner 层

职责单一:**管好一个 AgentSession 的生命周期**。

- 进程所有权:谁 `spawn` 谁负责清理。**绝对不要**把 spawned process 留在 readiness promise 里。Paseo `providers.md` 明确警告过这条。
- 事件流:把 SDK 的事件归一为 `AgentStreamEvent`(text/tool_call/permission/subagent/finish/error)。
- 持久化:每次 `persist()` 返回 `AgentPersistenceHandle`,崩溃后可 `resumeSession(handle)`。
- 上下文边界:`AgentRunner` 只持有自己的 timeline;跨 runner 的上下文通过 Worker 编排层显式传参,不共享内存。

### 6.1 Provider-Native Subagent 跟踪(只读)

**v0.2 硬约束**:Conductor **不**干预 provider 内部是否使用 subagent——Claude Task、OMP task、Codex sub-agent 等都是 provider 的私事。Conductor 的子 agent 跟踪是**纯观测层**,不调用、不控制、不阻止。

复用 Paseo `ProviderSubagentStore` 模式(纯跟踪、不实执行):

```ts
class ProviderSubagentStore {
  upsert(parentAgentId, provider, descriptor): void;  // 粘性更新,省略字段保留旧值
  appendTimeline(parentAgentId, subagentId, item): TimelineRow;
  remove(parentAgentId, subagentId): void;
}
```

关键约束:
- **实执行永远在 provider 内**(Claude Task 工具、OMP task 工具)。
- Conductor 只在 UI/时间线里"看到"这个子 agent,**不主动 spawn、不 cancel、不干预**。
- agent 之间是**对等关系**(peer),不是父子树——两个 stage 在 Conductor 视角下完全平等;parent/child 只是 provider SDK 内部概念。
- 不做"跨 provider 子 agent"(Codex 调 Claude)。需要跨 provider 协作时,走 Conductor 的 Worker 编排层,不是 provider 子 agent 层。

> 这是 Conductor **不重造子 agent 引擎**的核心保证。

## 7. Agent Worker 层 —— 编排图节点

这是用户原始分层里的"编排层",提供 5 种基本节点:

```
              ┌─────────┐
              │  entry  │   (工作流入口)
              └────┬────┘
                   │
        ┌──────────┼──────────┐
        ▼          ▼          ▼
    ┌──────┐  ┌──────┐  ┌──────────────┐
    │single│  │ seq  │  │  parallel    │   (并行 fan-out)
    └──┬───┘  └──┬───┘  └──────┬───────┘
       │         │             │
       │         ▼             │
       │     ┌──────┐          │
       │     │switch│ (条件)    │
       │     └──┬───┘          │
       │        │              ▼
       │        │        ┌────────────┐
       │        │        │   loop     │   (loop-until 条件)
       │        │        └─────┬──────┘
       ▼        ▼              ▼
    ┌────────────────────────────┐
    │           exit              │   (工作流出口)
    └────────────────────────────┘
```

### 7.1 节点统一契约

```ts
type NodeRef =
  | { kind: "single";   agent: AgentSpec; name: string }
  | { kind: "sequence"; steps: NodeRef[] }
  | { kind: "parallel"; branches: NodeRef[]; join: "all" | "any" | "race" }
  | { kind: "switch";   on: string; cases: Record<string, NodeRef>; default?: NodeRef }
  | { kind: "loop";     body: NodeRef; until: Expr; maxIter: number };

type NodeResult = {
  node: NodeRef;
  status: "ok" | "error" | "skipped" | "cancelled";
  output?: unknown;
  durationMs: number;
  children: NodeResult[];   // 子节点的递归结果,便于审计
};
```

> 这层用纯函数 + 图描述,**不**碰 LLM。可独立测试。

### 7.2 与 Workflow 引擎的关系

Worker 是"图节点类型系统",Workflow 引擎是"动态生成图 + 跑 PDCA 循环"——见 §8。

## 8. Agent Workflow 层 —— 软工作流与阶段标签

> **v0.3 重要修正**:PDCA 在这里是**任务阶段的语义标签**(Plan → Do → Check → **Apply**),**不是** Deming 质量循环的硬状态机。它更接近 gsd 这类"软工作流":每个阶段有目标与交付物,但阶段之间的顺序与重复由 agent 自主决定。**PDCA 只是默认 phase 集,不是唯一形态——用户可自定义更多阶段**(如 design / verify / ship / release)。

Conductor 与 Paseo 最大差异仍然成立:
- Paseo 把编排完全交给父 LLM,父 agent 用 Task tool 触发子 agent(纯 prompt 层)。
- Conductor 需要一个**显式的阶段调度引擎**,原因是用户期望"每阶段动态生成 worker task"——**子任务结构是程序化生成的,phase name 是结构化标签,不是 LLM 自由发挥**。

### 8.1 Stage / Phase 模型

```ts
type PhaseName = string;   // 任意语义标签,默认:"plan" | "do" | "check" | "apply",可扩展

interface WorkflowStage {
  name: PhaseName;                                  // 阶段语义标签
  input:  ZodSchema<unknown>;                        // 接受 inputs / 之前 stages 输出
  output: ZodSchema<unknown>;                        // 落盘前校验
  run(ctx: StageContext): Promise<unknown>;          // 同 StageSpec,但强调 phase 语义
  // 可选:软门禁
  skipIf?: Expr;                                     // 表达式为真则跳过该 phase
  retries?: number;
  gate?: { expr: Expr; timeoutMs?: number };         // 进入下一 phase 前的软门禁
}
```

关键点:
- **阶段顺序不强制**:`Workflow.phases` 是有序数组,但 `phase[i]` 可以根据 `ctx.prev` 跳回 `phase[j]`(软循环)。
- **阶段可重复**:同 phase 可在一次 run 中出现多次(如 plan → do → check → apply → plan → do,这是合规的 PDCA 多轮)。
- **阶段可扩展**:用户可在 spec 里加 `phases: ["design", "plan", "do", "verify", "ship", "release"]`,无需改引擎。

### 8.2 PDCA 默认预设(Plan → Do → Check → Apply)

```ts
const PDCA_PRESET: WorkflowStage[] = [
  { name: "plan",  output: PlanOutputSchema,  run: planStage  },
  { name: "do",    output: DoOutputSchema,    run: doStage    },
  { name: "check", output: CheckOutputSchema, run: checkStage },
  { name: "apply", output: ApplyOutputSchema, run: applyStage },
];
```

> 注意是 **Apply**(应用变更),不是古典 Deming 里的 Act(根据结果行动)。在软件场景下 Apply 更精确——"apply the change/improvement"。

阶段间关系(示意,**非强制**):

```
  plan ──→ do ──→ check ──→ apply
   ▲                            │
   └────────────────────────────┘
       (可选:check 不通过则回到 plan 或 do)
```

门禁示例(默认全开,可覆盖):
- `do`:可跳过(某些 task 不需要执行)
- `check`:必跑,失败则回到 `do`(默认)或 `plan`(可选)
- `apply`:必跑;失败则终止 run

### 8.3 GSD 风格预设(示例:Research → Spec → Build → Verify → Ship)

```ts
const GSD_PRESET: WorkflowStage[] = [
  { name: "research", output: ResearchOutputSchema, run: researchStage },  // 读 codebase
  { name: "spec",     output: SpecOutputSchema,     run: specStage     },  // 写 spec
  { name: "build",    output: BuildOutputSchema,    run: buildStage    },  // 实现
  { name: "verify",   output: VerifyOutputSchema,   run: verifyStage   },  // 跑测试 / 静态检查
  { name: "ship",     output: ShipOutputSchema,     run: shipStage     },  // commit / PR
];
```

GSD 与 PDCA 的差别:
- **research 先于 plan**:不熟悉 codebase 时先调研
- **spec 是显式阶段**:交付物是结构化 spec(可被存档、引用)
- **verify 与 check 不同**:verify 跑真实测试/lint,check 是逻辑/可行性 review
- **ship 是终点**:一次性,失败不回退(用户介入)

### 8.4 自定义阶段

用户可声明任意 phase name,无需改引擎。例如:
```ts
conductor run --workflow ./my-workflow.json
// my-workflow.json:
// {
//   "phases": ["design", "implement", "review", "release"],
//   "stages": {
//     "design":    { "input": ..., "output": ..., "run": "designStage" },
//     ...
//   }
// }
```

引擎只负责:`phases[i] → phases[i+1]` 调度、阶段间 ctx 传递、persistence、cancellation。**它不规定"phases 必须是 PDCA"**。

### 8.5 动态阶段生成(planner)

```ts
interface WorkflowSpec {
  phases: WorkflowStage[];                          // 静态 phase 集
  planner?: (ctx: WorkflowContext) => Promise<NodeRef>;  // 可选:动态生成下一个 NodeRef
}
```

> 这是 Conductor 与"硬编码图引擎"的差别:**计划本身是 LLM/procedure 调用**,但执行图是结构化数据。LLM 只负责"算下一阶段要做什么",执行仍然走 Worker 层。

### 8.6 实现选项(技术选型)

| 选项 | 描述 | 优点 | 缺点 |
|---|---|---|---|
| A. 内置轻量引擎 | Conductor 自己实现 phase 调度器 | 无外部依赖、可控 | 需自己写调度/持久化/恢复 |
| B. 外部 Temporal / Restate | 借用成熟工作流引擎 | 持久化/恢复/可视化免费 | 引入重依赖,部署复杂 |
| C. Prompt-only | 退化到 Paseo 模式 | 极简 | 不满足"动态生成 worker task"语义 |

**默认推荐 A**(轻量 + 文件持久化),为未来切换到 B 留接口。

## 9. Agent Gateway 层

入口层,接受外部输入并唤起 workflow。

### 9.1 三种入口

- **HTTP/WS API**:`POST /v1/workflows` 启动、`GET /v1/workflows/:id` 查状态、`WS /v1/workflows/:id/stream` 流式订阅。
- **CLI**:`conductor run|send|ls|logs|wait|workflow`。完全镜像 Paseo 的最小 CLI 表面,便于脚本化。
- **Web UI**(可选,Phase 3+):Paseo / Multica 都做了,我们延后。

### 9.2 Gateway 与 Daemon 关系

Gateway 可以有两种部署形态:
- **嵌入 Daemon**:单 host 默认。Daemon 直接暴露 HTTP/WS。
- **独立 Gateway → 多 Daemon**:Hub 模式,Gateway 负责路由到指定 Daemon。**Phase 4 再做。**

## 10. Player Daemon + Registry

用户原始需求:"**单机 Player-daemon 架构**" + "**player registry**"。
v0.2 决策:**registry 是多 host Hub 形态**——单 host 上的 daemon 仍按"单机"运行,registry 是跨 host 的注册中心,见 §10.3。

### 10.1 单 host = 单 Daemon 进程

借鉴 Paseo `pid-lock.ts` 模式:同一 host 同一时间只允许一个 Daemon 实例,违反则报错退出。

### 10.2 Registry — 进程内 + Hub

**每 host 内部**:`PlayerRegistry` 是进程内 map,与 §10.1 同。

**跨 host**:Hub 是一个独立服务(也是 Conductor 的一个 daemon),维护所有 Player 的注册:

```ts
class PlayerRegistry {                     // 进程内
  agents: Map<AgentId, AgentRunner>;
  providers: Map<ProviderId, AgentClient>;
  workflows: Map<WorkflowId, WorkflowInstance>;
  worktrees: Map<WorktreeId, WorktreeHandle>;

  heartbeat(): Promise<HealthSnapshot>;   // Hub 来取
  prune(): Promise<void>;
}

class PlayerHub {                          // 跨 host 注册中心
  players: Map<PlayerId, PlayerEndpoint>; // PlayerId = host + daemon-instance-id
  workflows: Map<WorkflowId, WorkflowHandle>;
  
  register(player: PlayerEndpoint): void;
  heartbeat(player: PlayerId, snap: HealthSnapshot): void;
  routeWorkflow(wf: WorkflowHandle, target: PlayerSelector): PlayerEndpoint;
  migrateWorkflow(wf: WorkflowId, from: PlayerId, to: PlayerId): void;
}

type PlayerSelector =
  | { kind: "any" }                              // 任意健康 host
  | { kind: "by-tag"; tags: string[] }           // host tag(架构、GPU、地域)
  | { kind: "by-provider"; provider: string }     // 必须有某 provider
  | { kind: "by-pinned"; playerId: PlayerId };   // 钉死某 host
```

> Hub 与 Player 间是 WS 长连接 + 心跳;Player 离线后 Hub 把 run 迁到其他健康 host(§11.6 恢复)。

### 10.3 Worktree 隔离

并行 agent 在同一 repo 上工作时必须隔离(否则 git 工作树互相覆盖)。直接复用 Paseo `server/worktree/` 的模式:每条并行 branch 自动 `git worktree add` 到 `.conductor/worktrees/<branch>`。
Hub 调度时**优先把同 workflow 的 stage 路由到同一 host**(避免 worktree 反复 sync),只有该 host 不可用时才迁移。

## 11. 持久化与可观测

### 11.1 持久化

v0.2 决策:**默认 JSON + SQLite 一等切换**(不是"以后再说")。

**Storage Interface**(两个后端实现同一接口):

```ts
interface Storage {
  // 原子写入
  putWorkflow(state: WorkflowState): Promise<void>;
  getWorkflow(runId: string): Promise<WorkflowState | null>;
  listWorkflows(filter?: WorkflowFilter): AsyncIterable<WorkflowState>;
  
  putAgent(spec: AgentSpec, instance: AgentInstance): Promise<void>;
  getAgent(id: string): Promise<{spec, instance} | null>;
  
  appendTimeline(agentId: string, item: TimelineItem): Promise<void>;  // 永远 append-only
  readTimeline(agentId: string, opts: TimelineQuery): AsyncIterable<TimelineItem>;
  
  // Blobs(大对象 offload)
  putBlob(sha256: string, stream: ReadableStream): Promise<{bytes: number}>;
  getBlob(sha256: string): Promise<ReadableStream | null>;
  
  // 索引
  queryWorkflows(filter: WorkflowFilter): Promise<WorkflowSummary[]>;
}
```

**两个实现**:
- `JsonFileStorage` (Phase 1 默认):`$CONDUCTOR_HOME` 下分目录,workflow 存 `runs/<id>/state.json`,timeline 用 append-only NDJSON,blob 走文件系统。
- `SqliteStorage` (Phase 1 同步实现,运行时切换):better-sqlite3 单文件,schema 与 JSON 等价,通过 STORAGE_BACKEND 环境变量切换,运行期不混用。

切换时机:数据量到 10k+ runs 或并发写多 stage 时切 SQLite;小规模调试用 JSON(更易人肉读)。

### 11.1.1 wire schema 不变

两个 backend 共享同一个 Zod schema;JSON 是直接序列化,SQLite 是 schema-as-table。读路径必须返回同一类型(由 Zod 在边界校验),业务代码无感。

### 11.2 可观测

- **Timeline**:每个 agent 一条 timeline(text/tool_call/permission/subagent/finish/error),统一格式。
- **Logs**:结构化日志(参考 Paseo `daemon.log`),由 pino 或类似 logger 输出。
- **Replay**:因为 timeline + persistence handle 都在,可以重放任意时刻。

## 12. Workflow Context — 跨步骤传递(详细设计)

v0.2 重点新增。Context 是 PDCA 工作流的脊柱,设计错会让长跑 workflow 不可恢复、跨 host 不可迁移。

### 12.1 三类数据流动

| 类型 | 例子 | 特性 |
|---|---|---|
| **Inputs** | 用户初始 prompt、附件、env | run 内不可变,每个步骤可见 |
| **Stage outputs** | plan 阶段输出、check 阶段 verdict | 按 stage 名累积,有类型 schema |
| **Refs** | worktree 句柄、diff 文件、persistence handle | 懒加载,大对象都走 ref |

> 把所有上下文塞进 system prompt 是反模式——provider 的 context window 有上限,workflow 跑 100 步后上下文会爆炸。**必须**有 offload 边界。

### 12.2 Stage 输出契约(强类型)

```ts
interface StageSpec {
  name: string;                              // 全局唯一
  input:  ZodSchema<unknown>;                 // 接受 inputs / 之前 stages 的并集
  output: ZodSchema<unknown>;                 // 落盘前校验
  run(ctx: StageContext): Promise<unknown>;
}

interface StageOutput {
  status: "ok" | "error" | "skipped" | "cancelled";
  data:  unknown;                            // Zod-validated
  refs:  RefMap;                             // 关联的句柄(见 12.5)
  ts:    string;                             // ISO 时间戳
  host:  string;                             // 哪个 host 跑的
  durationMs: number;
}
```

### 12.3 StageContext(步骤运行时收到的视图)

```ts
interface StageContext {
  run:    { id: string; workflow: string; inputs: RunInputs };
  self:   StageSpec;                         // 当前阶段自己的定义
  prev:   Record<string, StageOutput>;       // 历史阶段输出(只读)
  refs:   RefMap;                            // 全局可访问的 ref
  signal: AbortSignal;                       // 取消信号,Hub/父任务可中断
  emit(e: StreamEvent): void;                // 流式事件(给 Gateway 推)
  store(key: string, value: unknown): Promise<void>;  // 步骤内 KV(不强制 schema)
}
```

关键点:
- **`prev` 是只读 map,按 stage 名查**——不靠隐式流式累积
- **`signal` 是单一取消入口**——Hub 取消 / 用户取消 / 超时 三路合一
- **`emit` 给 Gateway 推实时进度,不进持久化**

### 12.4 PDCA 实例的 context 演化

例:"重构 auth 模块" workflow,Plan → Check → Do → Check → Act → loop:

```ts
// 初始
ctx = { run: { inputs: { repo: "...", target: "auth" } }, prev: {}, ... }

// Plan 阶段跑完(codex)
ctx.prev["plan"] = {
  status: "ok",
  data:   { plan: "拆 3 个 PR: 抽接口/补测试/换实现" },
  refs:   {}, ts: "...", host: "local", durationMs: 12000,
}

// Check 阶段(claude,读 prev.plan)
ctx.prev["check_feasibility"] = {
  status: "ok",
  data:   { verdict: "approved", concerns: ["接口命名 X"] },
  ...
}

// Do 阶段(codex,读 prev.plan + prev.check,产出 worktree + diff ref)
ctx.prev["do_pr1"] = {
  status: "ok",
  data:   { summary: "已抽 IAuthService" },
  refs:   {
    worktree: { kind: "worktree", id: "wt_xyz", branch: "refactor/auth-iface" },
    diff:     { kind: "file",     path: "/.../pr1.diff", bytes: 24180 },
  },
  ...
}
```

每个 stage 落盘前 **Zod 校验 output**,出错直接 fail 不入库。

### 12.5 Ref 系统(防 context 爆炸)

```ts
type Ref =
  | { kind: "file";      path: string; mime?: string; bytes?: number }
  | { kind: "worktree";  id: string; branch: string }
  | { kind: "session";   provider: string; handle: AgentPersistenceHandle }
  | { kind: "blob";      id: string; sha256: string; bytes: number };  // $CONDUCTOR_HOME/runs/<id>/blobs/<sha>

interface RefMap { [name: string]: Ref; }
```

规则:
- **Inline 阈值默认 256KB**——超过自动转 `blob` ref,落到 `runs/<runId>/blobs/<sha>`
- **worktree 和 session 永远是 ref**(不能 inline)
- **provider 看到的 prompt** 自动把 ref 替换成 `<ref name="diff" file="/path" />` 形式的标记,实际数据按需 fetch(provider 自己读取文件)

### 12.6 跨 host 序列化(Hub 路由时)

- **inline stage output** → JSON over WS(标准 message)
- **ref** → 同 host 直连;跨 host 时 Hub 把 ref 解析成可访问 URL(`GET /v1/refs/:id`,目标 host 自签短期 token)
- **blobs** → 走 BlobTransfer(可选压缩,大文件流式分片)

### 12.7 Provider 视角的 context

Provider 的 LLM 看到的不是 raw `StageContext`,而是 workflow 引擎**组装的 system prompt**:

```
[System prompt]
You are running stage "check_feasibility" of workflow "refactor-auth".
Available context from previous stages:
  plan.output: "<plan text>"
  plan.refs.worktree: worktree id=wt_xyz branch=refactor/auth-iface

Run inputs:
  repo: /Users/.../app
  target: auth module

Your job: <stage spec prompt>

Return a JSON matching this schema: <Zod schema as JSON Schema>
```

**Provider 不感知 contextBus 存在**——它只看到一个普通任务。这正好和 §6.1"provider 内部 subagent 不干预"一致。

### 12.8 持久化与恢复

```ts
interface WorkflowState {
  runId: string;
  workflow: string;
  spec: WorkflowSpec;             // 描述(可热重载新版,旧实例走旧 spec)
  inputs: RunInputs;
  stages: Record<string, StageOutput>;  // 只存 inline 部分
  refs: RefIndex;                 // 所有 ref 的位置表
  cursor: { stage: string; attempt: number };
  meta: { startedAt, hosts: [...], totalCost };
  schemaVersion: number;          // 迁移用
}
```

恢复:
- Daemon 重启 → 从 storage 读 WorkflowState → 在 `cursor` 续跑
- Hub 把 run 从 host A 迁到 host B → 序列化为 wire → host B 反序列化,从 cursor 续跑
- **provider session ref** 用 `AgentPersistenceHandle` resume(若 provider 支持);不支持 resume 的 provider → 新建 session,prompt 重新组装(可能丢失部分多轮上下文,标 "degraded")

### 12.9 Loop / Parallel 下的 context 传递规则

| 节点 | 传给子节点的 ctx |
|---|---|
| `sequence` | 子步骤依次拿到父 ctx + 累积 prev |
| `parallel` | 每个分支拿父 ctx 的**快照**(只读),各自分别累积,join 时合并 |
| `switch` | 命中分支拿父 ctx,其他分支 skipped |
| `loop` | 每次迭代拿"loop 开始时的 ctx 快照" + 本轮迭代累积;用 `until` 表达式读 `ctx.prev` 决定终止 |

**关键边界:分支不能污染 ctx。** parallel 三分支跑完,join 后主 ctx 的 `prev` 只增"join step"一项,各分支的内部 stage 仍按名字可见——这给 planner 一个完整的"分支→分支内 stage→join"层次视图。

### 12.10 长上下文策略分层(4 层,各司其职)

> **v0.3 修正**:context 不是单一机制,而是**分层策略**——每层解决不同问题,跨层兜底。

**Layer 0 — Ref 懒加载(Conductor 默认,always-on)**

§12.5 的 Ref 系统就是这一层。provider 看到的 prompt 里只有 ref 标记(如 `<ref name="diff" file="/.../pr1.diff" />`),实际数据按需 fetch。**对所有 stage 启用,零成本,无限规模**。

**Layer 1 — Workflow 级选择性传参(声明式)**

Workflow spec 声明每个 stage 显式 read 哪些前驱 stage:
```ts
{
  name: "do_pr1",
  reads: ["plan", "check_feasibility"],   // 只有这两个 stage 的 output 进 prompt
  // ...
}
```
默认:仅读直接前驱。可声明空数组(`reads: []`)完全隔离;可声明 `"*"` 读全部(危险,需 confirm)。

**Layer 2 — Agent 级按需 retrieve(agent 协作逻辑)**

Conductor 给 agent 暴露一组工具:
```ts
// agent 可调用的 context 工具
{
  "conductor.context.get":    (stageName, refName) => Ref,
  "conductor.context.list":   (filter) => StageSummary[],
  "conductor.context.search": (query, scope?) => SearchHit[],   // 全文搜
  "conductor.context.summary": (stageName, maxTokens?) => string, // 摘要
}
```

Agent **自主决定**什么时候拉什么。这是"agent 协作逻辑"层——agent 比 workflow 更清楚"我现在需要看 plan 的哪一段"。

**Layer 3 — Provider 层超阈值摘要(兜底)**

当 agent 多轮对话超出 context window 时,**provider SDK** 自动启动摘要:
- Claude Code / Codex 等已有自己的 summarization 机制
- 这是 provider 私事,Conductor **不实现、不干预**
- Conductor 只暴露 hooks:`onContextPressure(thresholdPct, action)`,provider 触发时回调

**Layer 4 — Tiered memory(可选,长跑 workflow)**

> 50+ stage 的工作流。Phase 4+ 才考虑。
- Hot:最近 N 个 stage 完整保留
- Cold:老 stage 摘要索引,可通过 `conductor.context.search` 检索
- 触发条件:`stage count > tieredMemoryThreshold`(默认 30)

**策略路由总览**:

```
┌──────────────────────────────────────────────────────┐
│ Provider SDK 内部(兜底)                                │ Layer 3
├──────────────────────────────────────────────────────┤
│ Agent 工具调用(主动 retrieve)                          │ Layer 2
├──────────────────────────────────────────────────────┤
│ Workflow spec 声明(显式 reads)                         │ Layer 1
├──────────────────────────────────────────────────────┤
│ Ref 懒加载(标记 + 按需 fetch)                           │ Layer 0
└──────────────────────────────────────────────────────┘
```

**各层关系**:
- Layer 0 是地基,所有 stage 都自动启用
- Layer 1 是默认推荐:workflow 编写者显式声明读取
- Layer 2 是协作层:agent 在多层 context 中导航,需要 `conductor.context.*` 工具族
- Layer 3 是兜底:当 Layer 0+1+2 都不够时,provider 自己的 summarization 顶上去
- Layer 4 是长跑优化:Phase 4+

**Phase 1/2 实现优先级**:
- Phase 1:Layer 0(已有)+ Layer 1(spec 字段预留)
- Phase 2:Layer 2(`conductor.context.*` 工具族实现)
- Phase 2 末:Layer 3 适配(provider hook)
- Phase 4+:Layer 4

---

## 13. 实施路线图

```
Phase 1 (MVP):
  - protocol/ skeleton + Zod schemas
  - provider/ base + Claude/Codex 内置 provider
  - runner/ 生命周期 + 事件流
  - storage/ 文件 JSON
  - daemon/ 入口 + pid-lock
  - cli/ run/ls/logs/send/wait

Phase 2:
  - worker/ 5 种节点类型
  - workflow/ 软工作流引擎(自研,§8.6 方案 A)+ PDCA/GSD 默认预设
  - registry/ 进程内注册表
  - worktree/ 自动隔离
  - cancellation/ §14 协议落地 + 跨 host e2e

Phase 3:
  - provider/ Pi/OMP/ACP-generic(扩到 5+ provider)
  - provider-subagents/ 跟踪 + UI(可选 web)
  - 更多编排原语(join 策略、loop 条件)

Phase 4:
  - Hub: 多 Daemon 注册与路由 + HA
  - 可选 Web UI
  - 可选 Postgres 后端
  - Tiered memory(Layer 4)
```

## 14. Cancellation Protocol

> **v0.3 新增**。多 host + 多 stage 并发 + provider 子进程,取消信号必须三路合一(Hub / 用户 / workflow 自身)且可恢复。

### 14.1 取消来源

| 来源 | 触发条件 | 优先级 |
|---|---|---|
| **用户** | CLI `cancel`、API `DELETE /v1/runs/:id`、WS 断开 | 最高(立即响应) |
| **Hub** | host 失联、run 迁移、配额超限 | 高 |
| **Workflow** | gate 超时、retry 耗尽、`loop.until` 命中 cancel、表达式求值为 cancel | 中 |
| **Stage** | 依赖 stage 失败(在 parallel/sequence 中传播) | 低 |

### 14.2 取消目标层级

取消信号按依赖树向下传播:

```
workflow (cancelled)
  └─ stage A (running)
       └─ provider session (current turn)
            └─ provider subprocess (CLI / app-server 进程)
```

**关键边界**:
- **兄弟 stage 默认 fail-fast**:`parallel` 中一个分支被取消,其他分支也取消(默认 `cancelPolicy: "fail-fast"`)
- **sequence 阶段一个失败,后续 skip**(而非 cancel,因为语义上是"前置失败")
- **worktree 等副作用保留**——取消不删 worktree,留给用户/Hub 检查

### 14.3 三阶段生命周期

```
active → cancelling → cancelled
            │              │
            └─→ cancel_failed (timeout 等不到 ack)
```

| 状态 | 含义 | 持续时间 |
|---|---|---|
| `active` | 正常运行 | — |
| `cancelling` | 信号已发,等待 ack | ≤ `timeoutMs` |
| `cancelled` | stage 已 ack,状态落盘 | 终态 |
| `cancel_failed` | 超时未 ack,force-kill | 终态 |

### 14.4 信号与超时

```ts
interface CancelOptions {
  reason: "user" | "hub" | "workflow-gate" | "retry-exhausted"
        | "loop-maxiter" | "dependency-failed" | "quota-exceeded";
  timeoutMs: number;       // 默认 30000(grace period)
  force: boolean;          // true → 立即 SIGKILL,跳过 grace
}

interface CancelResult {
  status: "cancelled" | "cancel_failed";
  cancelledAt: string;
  reason: CancelOptions["reason"];
  providerState: "resumable" | "lost";  // 取决于 provider 是否保留 persistence handle
  forcedKill: boolean;
}
```

`StageContext.signal` 是**单一取消入口**——三路信号合流。Stage 函数应监听 `signal` 并尽快返回。

### 14.5 Provider 取消语义

Stage 收到 cancel 后:
1. `signal.abort()` 触发,Stage 函数应在 `timeoutMs` 内返回
2. Conductor 调 `providerSession.cancel()`:
   - Claude Code SDK → 中断当前 turn,保留 session
   - Codex app-server → interrupt,保留 session
   - ACP → 发 `cancel` notification,等 ack
3. 若 provider 支持 `AgentPersistenceHandle`,persist 部分状态 → 下次可 resume
4. Stage 标记 `status: "cancelled"`,落盘
5. Ref 保留(不删)

`timeoutMs` 到达后未 ack:
1. Provider subprocess force-kill(SIGKILL)
2. Stage 标记 `cancel_failed`
3. Provider 状态标记 `lost`(无法 resume)
4. Workflow 决定 retry / fail run

### 14.6 取消传播策略

```ts
type CancelPolicy =
  | { kind: "fail-fast" }                              // 取消兄弟 stage(parallel 默认)
  | { kind: "continue-siblings" }                       // 兄弟继续跑(sequence 默认)
  | { kind: "drain"; timeoutMs: number };              // 给兄弟 timeoutMs 收尾,然后取消
```

### 14.7 幂等性

Stage 需要 `idempotencyKey`:
- `idempotencyKey = "${runId}:${stageName}:${attempt}"`
- 副作用(git commit、worktree、PR 创建)必须先 check key 再执行
- Ref 用 sha256 命名天然幂等

### 14.8 Hub 层取消

Hub 检测到 host 失联(heartbeat 超时):
1. Hub 向该 host 上的所有 stage 发 cancel 信号
2. run 标记 `migrating`
3. cancel ack 后(或 force-kill),Hub 选新 host
4. 新 host 从 storage 读 `WorkflowState`,从 `cursor` 续跑
5. cancelled stage 保持 `cancelled` 状态(默认不重跑)
6. spec 可配置 `restartCancelled: boolean` → 重新跑被取消的 stage

### 14.9 用户取消 UX

```bash
# CLI
conductor cancel <runId>                      # 优雅取消
conductor cancel <runId> --force              # 立即 SIGKILL
conductor cancel <runId> --reason "user"      # 标记来源(用于 analytics)

# HTTP
DELETE /v1/runs/<runId>                       # 优雅
DELETE /v1/runs/<runId>?force=true            # 立即
```

返回 202 Accepted + cancel 任务 ID;真正的 cancel 状态通过 `GET /v1/runs/:id` 或 WS 流查看。

### 14.10 取消与持久化的关系

- Cancel 信号发出后,stage 状态(`cancelling`)立即落盘
- 即使 force-kill,WorkflowState 已写入 `cancelling`
- 重启 Daemon 后可恢复 cancel 流程(避免"半取消"状态)
- `schemaVersion` 必须支持 cancel 字段的演化

## 15. 设计不足 / 自我审视

> 这是与用户原始分层逐条对照的诚实清单,不是宣传。

1. **PDCA 自研引擎的代价** —— 已选定方案 A(自研),代价是持久化恢复、超时取消、回放调试都要重做。需要在 §12.8 把 schemaVersion / cursor / degraded 标记做扎实;Phase 2 必须有"中断 + 续跑"的 e2e 测试覆盖。

2. **跨 provider 子 agent 不做** —— §6.1 已硬约束:agent 是 peer,不做 Conductor-owned subagent。需要"Codex 调 Claude"协作时,只能走 Worker 编排层(让 Claude 作为独立 stage 跑),不能伪装成 provider 子 agent——这会损失一些"由 provider 维护的多轮上下文"语义,但换来清晰的边界。

3. **Provider-native subagent 的可见性差异** —— Claude Task 工具和 OMP task 工具产生的子 agent,我们通过 store 跟踪;但子 agent 的真实事件流受限于 provider SDK。Codex app-server 的子 agent 事件归一化成本可能不低。

4. **Context 长上下文累积** —— §12.10 已给出 4 层策略分层(Ref / 声明式 reads / agent retrieve / provider 摘要),但**实际效果依赖 provider 摘要质量**(Layer 3 兜底层)和 agent 协作智能( Layer 2)。Phase 2 必须有 e2e 测试覆盖"50+ stage workflow 不爆 context"。

5. **多 workspace / 多 repo** —— 单 host 单 cwd 是 Phase 1 默认行为。如果用户要把 Conductor 跑在 monorepo 上同时编排多个 package,需要 workspace 隔离(类似 Paseo `workspace-labels`),**当前未设计**。

6. **Quota / Auth 抽象** —— Paseo 有 `services/quota-fetcher`,我们 Phase 1 不做。意味着用户得自己在 provider 配置里管 API key。

7. **Hub 的可靠性是单点** —— v0.2 已确认 multi-host Hub,但 Hub 本身是中心服务,挂了整个集群没法调度新 run(已运行的不受影响)。Phase 1 假设单 Hub;Phase 4 需考虑 Hub HA(主备/共识)。

8. **测试与并发模型** —— §14 已给出 cancellation 协议,但死锁、cancellation 跨 host 传播一致性、idempotency 在分布式下的语义还需在 Phase 2 e2e 中验证(尤其 "Hub cancel → 多 host 级联"的赛跑场景)。

9. **Provider 版本兼容** —— Claude Code SDK、Codex app-server 都在快速迭代。Provider 实现必须把"哪些字段是稳定的、哪些会变"显式标注,**当前未约定 deprecation 策略**。

10. **不与 LLM 直接耦合** —— 这是 Conductor 的定位选择,但也意味着"用一个 LLM 当 planner 来动态生成图节点"的能力被限定在 Worker 层之上。如果未来需要"LLM 在线重规划图结构",引擎层必须支持热替换 NodeRef,**当前未实现**。

## 16. 已确认的关键决策(v0.3)

| 决策点 | 选定方案 | 章节 |
|---|---|---|
| Workflow 引擎 | **A. 自研轻量软工作流引擎**(否掉 Temporal/Restate 重方案,否掉 prompt-only 退化) | §8.6 |
| Player registry | **多 host Hub**(进程内 + 跨 host 注册中心) | §10.2 |
| 持久化默认 | **文件 JSON + SQLite 一等切换**(运行时可切,wire schema 不变) | §11.1 |
| 跨 provider 子 agent | **不做**,agent 间对等(peer) | §6.1 |
| Provider 内部 subagent | **不干预**,纯观测 | §6.1 |
| Context 跨步骤 | **强类型 StageSpec + Ref offload + Zod 校验** | §12 |
| 长上下文策略 | **4 层分层**:Ref / 声明式 reads / agent retrieve / provider 摘要 | §12.10 |
| Workflow 阶段 | **软工作流 + 语义标签**:PDCA(Plan→Do→Check→**Apply**)是默认预设,非强制;可扩展 GSD、自定义 phases | §8 |
| 取消协议 | **三路合一**:Hub / 用户 / workflow 自身 → 同一 signal,三阶段生命周期 | §14 |
| Web UI | Phase 1 不出,Phase 3+ 可选 | §9.1 |

仍待确认的非阻塞项(实施时再定):
- Hub 的鉴权模型(token / mTLS / SSH-like)
- Provider 之间 auth/credential 隔离(`$CONDUCTOR_HOME/providers/<id>/auth`)
- Web UI 是 Phase 1 还是 Phase 3+
- Provider hook `onContextPressure` 的具体 contract(provider SDK 不统一,需要适配层)

---

## 附录 B:已读的竞品源文件清单(供追溯)

- Paseo:
  - `packages/server/src/server/agent/provider-registry.ts`(934 行,Provider 抽象)
  - `packages/server/src/server/agent/provider-subagents/store.ts`(186 行,子 agent 跟踪)
  - `packages/server/src/server/orchestration-skills/index.ts` + `internal/*`(Skill 安装管理)
  - `packages/server/src/server/agent/providers/{claude,codex,opencode,omp,pi}/`(各 provider 实现)
  - `packages/server/src/server/agent/providers/acp-agent.ts`(ACP 基类)
  - `docs/architecture.md`、`docs/agent-lifecycle.md`、`docs/providers.md`、`docs/custom-providers.md`、`docs/data-model.md`
- Multica:
  - `packages/core/runtimes/`(Runtime 健康度派生,Provider 抽象)
  - `packages/core/agents/`(Agent 在团队中的身份)
  - `packages/core/autopilots/`(触发式工作流,可借鉴为 PDCA 的"Act"输入)
  - `server/`(Go 服务,sqlc + Chi + WebSocket)
  - `VISION.md`、`CLI_AND_DAEMON.md`、`AGENTS.md`

---

## 版本变更

### v0.2 → v0.3(本次更新)

**用户决策**:
- ✅ PDCA = **软工作流阶段标签**,非硬循环。A = **Apply**(不是 Act)。可扩展 phases。
- ✅ Context 策略 = **4 层分层**:声明式 reads / agent retrieve / provider 摘要 / tiered memory
- ✅ 超阈值摘要定位为 **provider 层兜底**,Conductor 不实现
- ✅ 按需 retrieve 定位为 **agent 协作逻辑**,通过 `conductor.context.*` 工具族暴露

**新增 §14 Cancellation Protocol**:
- 4 类取消来源(用户/Hub/workflow/stage)+ 优先级
- 三阶段生命周期(active → cancelling → cancelled/cancel_failed)
- Signal 与 `timeoutMs`(默认 30s grace)
- Provider 取消语义(各 provider SDK 行为差异 + persistence handle 恢复)
- CancelPolicy(fail-fast / continue-siblings / drain)
- 幂等性键设计
- Hub 层取消 + host 迁移
- 用户取消 UX(CLI + HTTP)

**新增 §12.10 Context 策略 4 层分层**:
- Layer 0:Ref 懒加载(已有,always-on)
- Layer 1:Workflow spec `reads: [...]`(声明式)
- Layer 2:`conductor.context.{get,list,search,summary}` 工具族(agent 协作层)
- Layer 3:Provider hook `onContextPressure`(provider 兜底层)
- Layer 4:Tiered memory(Phase 4+)

**§8 重大修订**:
- 标题改为"软工作流与阶段标签"
- PDCA 改为预设而非唯一形态
- 新增 §8.3 GSD 预设示例(research → spec → build → verify → ship)
- 新增 §8.4 自定义阶段说明

**下一轮(待用户输入)**:
- Provider `onContextPressure` hook 的具体 contract(provider SDK 不统一)
- cancel 跨 host 一致性测试场景设计

**用户决策**:
- ❌ 否决 Temporal/Restate(方案 B)——PDCA 引擎定为自研
- ✅ Player registry 升级为多 host Hub
- ✅ 持久化:`SQLite` 从"以后再说"改为"一等切换"
- ✅ Context 跨步骤:补 §12 详细设计(StageSpec / Ref / 持久化 / 跨 host)

**新增硬约束**:
- §6.1 写明"provider 内部 subagent 不干预"+ agent 间对等

**架构演进**:
- §10 拆为单 host daemon + 跨 host PlayerHub
- §11.1 Storage interface 化,JsonFile / Sqlite 双实现并存
- §12 新增(原 §12 顺延为 §13)

**下一轮(待用户输入)**:
- Hub 鉴权模型(影响部署门槛)
- Provider auth 隔离形态(影响多用户共享 host)
