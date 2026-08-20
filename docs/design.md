# Conductor — 多 Agent 协作服务 设计方案 (v0.1)

> 状态: 草案。本仓库刚刚创建,本文件是第一份设计记录。所有架构决策以"对照 Paseo / Multica 真实代码 + 用户原始分层"为依据,后续讨论请直接编辑本文件或在 Issue 中附 diff。

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

### 6.1 Provider-Native Subagent 跟踪

复用 Paseo `ProviderSubagentStore` 模式(纯跟踪、不实执行):

```ts
class ProviderSubagentStore {
  upsert(parentAgentId, provider, descriptor): void;  // 粘性更新,省略字段保留旧值
  appendTimeline(parentAgentId, subagentId, item): TimelineRow;
  remove(parentAgentId, subagentId): void;
}
```

关键约束:**实执行永远在 provider 内**(Claude Task 工具、OMP task 工具)。Conductor 只做"父面板里看到这个子 agent"。

> 这是 Conductor **不重造子 agent 引擎**的核心保证。如果未来需要"跨 provider 子 agent"(Codex 调 Claude),再做一层独立的"Conductor-owned subagent",但默认走 provider-native。

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

## 8. Agent Workflow 层 —— PDCA 周期与动态子任务

用户原始需求里写"**每阶段动态生成 worker task,PDCA 循环**"。这是 Conductor 与 Paseo 最大差异点:

- Paseo 把编排完全交给父 LLM,父 agent 用 Task tool 触发子 agent(纯 prompt 层)。
- Conductor 需要一个**显式的 PDCA 引擎**,原因是用户期望"每阶段动态生成 worker task"——也就是说,**子任务结构是程序生成的,不是 LLM 自由发挥的**。

### 8.1 PDCA 阶段模型

```
Plan  →  Check  →  Do  →  Act
 ▲                            │
 └────────────────────────────┘
       (until criteria met)
```

每个阶段是一个 NodeRef,阶段之间通过显式 `gate`(布尔表达式 + 超时)切换。

### 8.2 动态子任务生成

```ts
interface WorkflowStage {
  name: "plan" | "do" | "check" | "act" | string;
  planner: (prev: StageOutput[]) => Promise<NodeRef>;  // 关键:动态生成下一阶段图
  gate?: { expr: Expr; timeoutMs?: number };
  retries?: number;
}
```

> 这是 Conductor 与"硬编码图引擎"的差别:**计划本身是 LLM/procedure 调用**,但执行图是结构化数据。LLM 只负责"算下一阶段要做什么",执行仍然走 Worker 层。

### 8.3 PDCA 实现选项(技术选型,见 §12 问题清单)

| 选项 | 描述 | 优点 | 缺点 |
|---|---|---|---|
| A. 内置轻量引擎 | Conductor 自己实现 PDCA 调度器 | 无外部依赖、可控 | 需自己写调度/持久化/恢复 |
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

### 10.1 单 host = 单 Daemon 进程

借鉴 Paseo `pid-lock.ts` 模式:同一 host 同一时间只允许一个 Daemon 实例,违反则报错退出。

### 10.2 Registry = 进程内注册表

```ts
class PlayerRegistry {
  agents: Map<AgentId, AgentRunner>;
  providers: Map<ProviderId, AgentClient>;
  workflows: Map<WorkflowId, WorkflowInstance>;
  worktrees: Map<WorktreeId, WorktreeHandle>;

  heartbeat(): Promise<HealthSnapshot>;  // 给 Hub(未来)用
  prune(): Promise<void>;                // 清理已完成/超时 runner
}
```

> 单 host 内一切都在内存;状态通过 `storage/` 持久化为文件 JSON。重启后从 `storage/` 重建 registry。

### 10.3 Worktree 隔离

并行 agent 在同一 repo 上工作时必须隔离(否则 git 工作树互相覆盖)。直接复用 Paseo `server/worktree/` 的模式:每条并行 branch 自动 `git worktree add` 到 `.conductor/worktrees/<branch>`。

## 11. 持久化与可观测

### 11.1 持久化

- **Phase 1-2**:文件 JSON + Zod(Paseo 模式)。`$CONDUCTOR_HOME/state.json`、每个 agent 单独 `agents/<id>.json`、timeline 用 append-only NDJSON。
- **Phase 3+**:可换 SQLite/Postgres,但 wire schema 不变。

### 11.2 可观测

- **Timeline**:每个 agent 一条 timeline(text/tool_call/permission/subagent/finish/error),统一格式。
- **Logs**:结构化日志(参考 Paseo `daemon.log`),由 pino 或类似 logger 输出。
- **Replay**:因为 timeline + persistence handle 都在,可以重放任意时刻。

## 12. 实施路线图

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
  - workflow/ PDCA 引擎(方案 A)
  - registry/ 进程内注册表
  - worktree/ 自动隔离

Phase 3:
  - provider/ Pi/OMP/ACP-generic(扩到 5+ provider)
  - provider-subagents/ 跟踪 + UI(可选 web)
  - 更多编排原语(join 策略、loop 条件)

Phase 4:
  - Hub: 多 Daemon 注册与路由
  - 可选 Web UI
  - 可选 Postgres 后端
```

## 13. 设计不足 / 自我审视

> 这是与用户原始分层逐条对照的诚实清单,不是宣传。

1. **PDCA 的"引擎"位置含糊** —— §8.3 给的三选项里,默认走方案 A,但**方案 A 在持久化恢复、超时取消、回放调试上要自己重做**。Temporal/Restate 这类成熟引擎省事但重。如果用户对"workflow 跑 30 分钟被中断还能续跑"有硬要求,直接方案 B 更稳。

2. **跨 provider 子 agent 未实现** —— 故意只做跟踪不做实执行。如果用户业务里真的有"Codex 必须调 Claude"的硬需求,§6.1 末尾提到的 Conductor-owned subagent 必须做,但代价是重复造 Claude Task 的轮子,**ROI 待评估**。

3. **Provider-native subagent 的可见性差异** —— Claude Task 工具和 OMP task 工具产生的子 agent,我们通过 store 跟踪;但子 agent 的真实事件流受限于 provider SDK。Codex app-server 的子 agent 事件归一化成本可能不低。

4. **Context 传递边界** —— Worker 层设计成"显式传参,不共享内存",导致跨步骤上下文需要序列化。如果用户需要"长上下文累积"(把 plan 阶段所有产出塞给 do 阶段),需要引入显式的 `contextBus` 对象,**当前未设计**。

5. **多 workspace / 多 repo** —— 单 host 单 cwd 是 Phase 1 默认行为。如果用户要把 Conductor 跑在 monorepo 上同时编排多个 package,需要 workspace 隔离(类似 Paseo `workspace-labels`),**当前未设计**。

6. **Quota / Auth 抽象** —— Paseo 有 `services/quota-fetcher`,我们 Phase 1 不做。意味着用户得自己在 provider 配置里管 API key。

7. **Hub / 多 Daemon 路由** —— 推迟到 Phase 4。当前"player registry"只是单 host 进程内注册表,**不是**多 host 注册中心。如果用户原始诉求里"player registry"指的是后者,本设计需要前置 Hub。

8. **测试与并发模型** —— Provider 进程是外部 spawn,Runner 事件流是异步迭代,Worker 图节点并发执行 —— 死锁、cancellation 传播、idempotency 必须从第一天设计进去,**当前尚未给出 cancellation 协议**。

9. **Provider 版本兼容** —— Claude Code SDK、Codex app-server 都在快速迭代。Provider 实现必须把"哪些字段是稳定的、哪些会变"显式标注,**当前未约定 deprecation 策略**。

10. **不与 LLM 直接耦合** —— 这是 Conductor 的定位选择,但也意味着"用一个 LLM 当 planner 来动态生成图节点"的能力被限定在 Worker 层之上。如果未来需要"LLM 在线重规划图结构",引擎层必须支持热替换 NodeRef,**当前未实现**。

## 14. 待用户确认的关键决策

1. **PDCA 实现选 A / B / C**(§8.3):A=自研轻量、B=Temporal/Restate、C=prompt-only 退化。
2. **"Player registry" 含义**(§10):进程内单 host(默认)/ 多 host Hub。
3. **持久化后端**(§11.1):文件 JSON(默认)/ SQLite / Postgres。
4. **是否需要跨 provider 子 agent**(§13.2):否 / 是 → 引入 Conductor-owned subagent。
5. **Phase 1 是否同步出 Web UI**:否(默认)/ 是。

---

## 附录 A:已读的竞品源文件清单(供追溯)

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
