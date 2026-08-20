# Conductor — 多 Agent 协作服务 设计方案 (v0.3)

> 状态: 草案(v0.3)。v0.1 → v0.2 → v0.3 主要变化见末尾"版本变更"。本仓库刚刚创建,本文件是第一份设计记录。所有架构决策以"对照 Paseo / Multica 真实代码 + 用户原始分层"为依据,后续讨论请直接编辑本文件或在 Issue 中附 diff。

## 0. 一句话定位

Conductor 是一个 **本地常驻的 Player Daemon**,把 Claude Code / Codex / Pi 等"已经能跑的 agent CLI"包成统一接口,在此之上提供:
- **可编排的工作流**(单 agent / 串行 / 并行 / 条件 / 循环,以及 PDCA 周期)
- **可观测的运行时**(统一日志、timeline、附件、子 agent 关系)
- **可扩展的 Provider 层**(内置 SDK + ACP 基类 + HTTP/RPC 自定义)

Conductor **不**自研 LLM agent 框架,**不**训练模型,**不**做模型路由优化。它只做"宿主 + 编排 + 观测"。

## 0.5 版本决策记录(v0.12.1 锁定)

**当前定论(2026-08)**:
- Backend:**Node.js 单栈**(Paseo 同语言,SDK / fork 优势)
- Provider:**Codex only + OpenRouter 配置**(OpenAI 维护 + OpenAI-compatible 协议)
- Spec 模型:**per-spec HOME**(spec 创建时写 config.toml,invoke 共享)
- HOME 隔离:`$CONDUCTOR_HOME/specs/<specId>/home/` per spec
- 取消协议:`AbortController` 三路合一
- 不做:**跨 host session migration** / **"无缝"边界** / **跨 provider 子 agent**

**v0.12.1 决策 — 后端锁定 Node.js**:
考虑过"Codex + Pi combo 时是否改回 Go",结论**保留 Node.js**,理由:
- Node 路线 provider 适配 ~150 行(Pi SDK / Codex SDK 直接 import)
- Go 路线 ~350 行(两个手写 JSON-RPC)
- Node 编辑即跑,Go 需 compile
- Node 生态契合(MCP SDK / zod / ws / fastify 直接用)
- 后悔成本低:Phase 4+ 真要 Go,接口已预留

Go 不是不能用,只是**这次保留 Node.js**。

## 1. 竞品对照(已落到代码的事实)

| 维度 | Paseo | Multica | Conductor (拟) |
|---|---|---|---|
| 形态 | 本地 daemon + 跨端 client | Go 服务 + Web/Electron/Expo + 自托管 | Go Daemon/Hub + JS WebUI + 单 binary CLI |
| 语言栈 | TypeScript (Node) | Go (server) + TS (前端) | **Go (server) + JS (webui)**,二进制发行 |
| Provider 抽象 | `AgentClient` 接口 + ACP 基类 | `Runtimes` 模块 + 健康度派生 | `AgentClient` 接口 + ACP 基类 + `os/exec` subprocess,见 §5 |
| 编排层 | prompt 模板 + provider-native subagent | `Autopilots`(定时/触发器)+ workflow DSL | Worker 图 + 软工作流引擎(§8),PDCA/GSD/自定义 |
| 持久化 | 文件 JSON + Zod | Postgres + sqlc | 文件 JSON + SQLite 一等切换(`modernc.org/sqlite`,无 cgo),见 §11.1 |
| 子 agent | `ProviderSubagentStore` 仅做跟踪,实执行交给 provider | 多 assignee + runtime binding | 同 Paseo,Store 仅跟踪,执行交给 provider |
| 远程 | 可选 E2E relay | 服务端 + Hub | 多 host PlayerHub(WS + 心跳),见 §10.2 |
| 取消原语 | AbortSignal + 手工协议 | context.Context | **`context.Context` 原生映射** §14 协议,见 §17 |
| MCP | 是 MCP host + 允许 provider 暴露 MCP | `MCP support` 模块 | 同 Paseo,Server 可作为 MCP host |

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
├─ server/                       # Go (单 binary 含 daemon/hub/cli)
│  ├─ cmd/conductor/             #   main.go: subcommand dispatch
│  │  ├─ daemon.go               #     `conductor daemon`
│  │  ├─ hub.go                  #     `conductor hub`
│  │  └─ cli.go                  #     `conductor run/send/ls/logs/...`
│  ├─ internal/
│  │  ├─ protocol/               #   wire 类型(Go structs + JSON Schema 导出)
│  │  ├─ provider/               #   AgentClient/AgentSession 接口 + 实现
│  │  │  ├─ base/                #     ACPAgentClient, SubprocessClient 基类
│  │  │  ├─ claude/              #     Claude Code CLI subprocess 适配
│  │  │  ├─ codex/               #     Codex app-server JSON-RPC 适配
│  │  │  ├─ pi/                  #     Pi RPC 适配
│  │  │  ├─ omp/                 #     OMP 适配
│  │  │  └─ acpgeneric/          #     通用 ACP(供 copilot/cursor/kimi/kiro)
│  │  ├─ runner/                 #   Agent Runner:生命周期、事件流、resume
│  │  ├─ worker/                 #   编排图节点:single/seq/par/switch/loop
│  │  ├─ workflow/               #   软工作流引擎(PDCA/GSD/自定义 phases)
│  │  ├─ registry/               #   PlayerRegistry(进程内)+ PlayerHub(跨 host)
│  │  ├─ storage/                #   JsonFileStorage + SqliteStorage(双实现)
│  │  ├─ gateway/                #   HTTP + WS(coder/websocket)
│  │  ├─ provider_subagents/     #   Provider-Native subagent 跟踪
│  │  └─ acp/                    #   ACP JSON-RPC 客户端(供 provider 用)
│  ├─ go.mod
│  └─ Makefile
├─ webui/                        # JS workspace(pnpm)
│  ├─ apps/web/                  #   Next.js 14 App Router + React 18
│  ├─ packages/
│  │  ├─ ui/                     #     design system(基于 shadcn/ui,参考 Multica)
│  │  ├─ api-client/             #     从 server OpenAPI 生成的 TS types + fetch
│  │  └─ conductor-protocol/     #     protocol 的 TS types(从 JSON Schema codegen)
│  ├─ package.json
│  └─ pnpm-workspace.yaml
├─ shared/                       # protocol 单一来源
│  └─ protocol/                  #   JSON Schema(Go + TS 都从这里 codegen)
├─ examples/                     # 工作流样例(.json + Go example code)
├─ docs/                         # 设计文档(本目录)
└─ README.md
```

**子命令形态**(TS monorepo,单 daemon binary):
```bash
conductor daemon                  # 启动 Player Daemon(默认前台)
conductor daemon --hub            # 启动 Player Hub(跨 host 注册中心)
conductor run "implement auth"    # CLI:通过 daemon 起 agent(默认 Pi,可指定)
conductor ls                       # CLI:列本地 agents
conductor cancel <runId>          # CLI:取消(对应 §14)
conductor workflow ./my.json      # CLI:启动工作流
conductor auth init/reset         # CLI:管理 .auth/(§6.2)
conductor init                    # CLI:首次跑,建 $CONDUCTOR_HOME/.auth/
```

**为何 TS 单栈**:
- 一个语言、一份类型(从 protocol 到 webui)
- **直接 fork Paseo TS 代码**(Paseo 是 TS,同语言)——这是 v0.9 选 TS 的最大动机
- 直接用 Pi SDK / Claude Agent SDK
- `npm i -g conductor` 跟 `brew install` 一样方便
- 代价:需要 Node runtime(>= 18);失去单 binary(放弃这个 trade-off 可接受)

**为何 protocol 共享**:
- TS:`zod` schema 单一来源,server + webui 都从同一 schema 引用
- Phase 1 简化:hand-written Zod schemas,server 和 webui 都用
- Phase 2+:从 Zod 派生 OpenAPI 给 webui fetch wrapper

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
>
> **v0.11 修订**:此形状与 Pi CLI flags 一一对应。Conductor 内部 AgentSpec 直接映射到 Pi session 配置参数(`provider`, `model`, `skills`, `mcpConfig`, `tools`, `thinking`, `cwd` 等)。详见 §5.2 Pi 集成层。

## 5. Codex 集成层(Codex Integration Layer)

> **v0.12 重大简化**:Conductor **只有 Codex 这一个 provider**。多模型能力通过 Codex 原生的 `[model_providers.<id>]` 配置实现,默认指向 OpenRouter。
>
> **v0.12 关键洞察**:Codex 不止支持 OpenAI 模型——通过 `~/.codex/config.toml` 的 `[model_providers.<id>]` 块,可以挂任意 OpenAI-compatible API(Mistral / Ollama / OpenRouter / LiteLLM / 自建代理)。OpenRouter 一次性覆盖 Claude / Gemini / Llama / Mistral 等 100+ 模型。

### 5.0 集成策略

**Conductor 复用 Codex 全套机制**,而不是重新发明:

| Conductor 需要 | Codex 已有 | Conductor 如何复用 |
|---|---|---|
| Session 持久化 + resume | Codex session JSONL(`~/.codex/sessions/`) | 直接读 |
| MCP 配置 | `--mcp-config` JSON | 直接传 |
| 工具权限 / approval | Codex approval model | Conductor UI 包一下 |
| Skills / 扩展 | Codex `AGENTS.md` / 配置文件 | 直接读 |
| Compaction | Codex `/compact` | 直接调 slash command |
| 取消 | `codex app-server` interrupt | `codexAppServer.interrupt()` |
| Provider / Model | `[model_providers.<id>]` + `--model` | 直接传 OpenAI-compatible 模型名 |
| 多模型 | OpenRouter 配置 | Codex provider 指向 OpenRouter |

**Conductor 在 Codex 之上加的价值**:
- PDCA / GSD workflow 引擎(§8)
- 多 host Hub 调度(§10)
- Per-task per-host contextBus(§12)
- HOME 隔离 + `.auth/` 共享(§6.2)
- WorkflowContext 持久化 + cursor 恢复
- 取消三协议合一(§14)

### 5.1 Codex 集成(Go)

```go
// internal/provider/codex/client.go —— Codex app-server JSON-RPC 客户端
package codex

import (
    "bufio"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "os/exec"
    "sync"
)

// CodexSession 是 Conductor 对 Codex app-server 的会话抽象
type CodexSession struct {
    cmd    *exec.Cmd
    stdin  io.WriteCloser
    stdout *bufio.Reader
    events chan AgentStreamEvent
    done   chan struct{}
    mu     sync.Mutex
    closed bool
}

type ConductorLaunchSpec struct {
    // Codex app-server 参数
    Model        string  // "anthropic/claude-opus-4-6" | "openai/gpt-5" | ...
    // 上下文
    Cwd          string
    Worktree     *WorktreeSpec
    // §6.2 隔离 HOME
    IsolatedHome string
    // 透传 Codex
    MCPConfig    string
    SystemPrompt string
    Thinking     string  // "minimal" | "low" | "medium" | "high"
    ToolsAllow   []string
    ToolsExclude []string
}

func CreateCodexSession(
    ctx context.Context,        // §14 取消入口
    spec ConductorLaunchSpec,
) (*CodexSession, error) {
    // 1. 读 spec.HomeDir/.codex/config.toml 解析 [model_providers] 配置
    cfg, err := parseCodexConfig(spec.IsolatedHome)
    if err != nil { return nil, err }

    // 2. 找对应 provider
    provider, ok := cfg.ModelProviders[cfg.ModelProvider]
    if !ok { return nil, fmt.Errorf("provider %q not found", cfg.ModelProvider) }

    // 3. spawn `codex app-server` subprocess(绑 ctx)
    cmd := exec.CommandContext(ctx, "codex", "app-server")
    cmd.Dir = spec.Cwd
    cmd.Env = append(os.Environ(),
        "HOME="+spec.IsolatedHome,
        "CODEX_MODEL_PROVIDER="+cfg.ModelProvider,
        "OPENAI_BASE_URL="+provider.BaseURL,
    )
    stdin, _ := cmd.StdinPipe()
    stdout, _ := cmd.StdoutPipe()

    if err := cmd.Start(); err != nil { return nil, err }

    sess := &CodexSession{
        cmd:    cmd,
        stdin:  stdin,
        stdout: bufio.NewReader(stdout),
        events: make(chan AgentStreamEvent, 64),
        done:   make(chan struct{}),
    }
    go sess.pumpEvents()  // goroutine: 读 stdout → events channel

    return sess, nil
}

// Send: 同步发送 prompt,等 Codex 响应
func (s *CodexSession) Send(ctx context.Context, prompt AgentPrompt) error {
    req := map[string]any{
        "method": "prompt",
        "params": prompt,
    }
    return s.writeJSON(req)  // JSON over stdin
}

// Events: 消费 event 流(多 turn)
func (s *CodexSession) Events() <-chan AgentStreamEvent {
    return s.events
}

// Cancel: §14 三路合一的 signal 入口
func (s *CodexSession) Cancel(ctx context.Context) error {
    // SIGTERM → grace → SIGKILL
    _ = s.cmd.Process.Signal(syscall.SIGTERM)
    select {
    case <-s.done:
    case <-time.After(30 * time.Second):
        _ = s.cmd.Process.Kill()
    }
    return nil
}

// Close: 清理 subprocess
func (s *CodexSession) Close(ctx context.Context) error {
    s.mu.Lock()
    if s.closed { s.mu.Unlock(); return nil }
    s.closed = true
    s.mu.Unlock()
    _ = s.stdin.Close()
    <-s.done
    return s.cmd.Wait()
}

func (s *CodexSession) pumpEvents() {
    defer close(s.done)
    defer close(s.events)
    for {
        line, err := s.stdout.ReadString('\n')
        if err != nil { return }
        var ev AgentStreamEvent
        if err := json.Unmarshal([]byte(line), &ev); err != nil { continue }
        select {
        case s.events <- ev:
        case <-s.done:
            return
        }
    }
}
```

> **v0.13 重写为 Go**:
> - `exec.CommandContext(ctx, ...)` 自动绑 §14 取消信号
> - `events chan AgentStreamEvent` 取代 `AsyncIterable`
> - `context.Context` 取代 `AbortSignal`(Go 原生一等公民)
> - 单 binary 发行(`go install` / `go build`)

### 5.2 Codex provider 配置(用户侧)

```toml
# ~/.codex/config.toml —— 用户自己配置,Conductor 不维护
# 默认:OpenAI 直连
[model_providers.openai]
# Codex 内置,无需配置

# OpenRouter 代理(覆盖 100+ 模型)
[model_providers.openrouter]
name = "OpenRouter"
base_url = "https://openrouter.ai/api/v1"
env_key = "OPENROUTER_API_KEY"

# Ollama 本地(可选)
[model_providers.local_ollama]
name = "Ollama"
base_url = "http://localhost:11434/v1"

# 自定义 proxy(可选)
[model_providers.proxy]
name = "OpenAI via LLM proxy"
base_url = "http://proxy.example.com"
env_key = "OPENAI_API_KEY"

# 默认 model_provider
model_provider = "openrouter"
```

**用户用法**:
```bash
# Claude via OpenRouter
conductor run --model anthropic/claude-opus-4-6 "implement auth"

# GPT-5 via OpenRouter(也走代理)
conductor run --model openai/gpt-5 "review this PR"

# Gemini via OpenRouter
conductor run --model google/gemini-2.5-pro "write tests"

# 直接 OpenAI(如果用户设 model_provider = openai)
conductor run --model gpt-5 "..."

# 本地 Ollama
conductor run --provider ollama --model llama3 "..."
```

> Conductor 不写死 model_provider;运行时从 `~/.codex/config.toml` 读,允许 user 在 Codex 配置层切。

### 5.3 与 Pi 集成的对比

| 维度 | v0.11 Pi 深度集成 | **v0.12 Codex + OpenRouter** |
|---|---|---|
| Provider 数 | 1 (Pi) | 1 (Codex) + 配置层 |
| 集成代码量 | ~30 行 | **~150 行(Codex JSON-RPC 完整实现)** |
| 多模型 | ✅ Pi 20+ 原生 | ✅ OpenRouter 100+ 代理 |
| 直连 Claude | ✅ Pi 原生 | ❌ 走 OpenRouter |
| 直连 GPT | ✅ Pi 原生 | ✅ Codex 直连 |
| 稳定性 | ⚠️ Mario 个人 | ✅ OpenAI 维护 |
| 配置入口 | Conductor / Pi 配置 | **Codex config.toml(用户标准配置)** |
| Provider 替换 | 改 Conductor 代码 | **改 config.toml,Conductor 不动** |

> **v0.12 关键让步**:多模型从"Conductor 层"移到"Codex config 层"。用户改 `~/.codex/config.toml` 就够了,Conductor 不参与 provider 配置。这让 Conductor 代码更小、更稳定。



```typescript
// packages/provider/pi/index.ts —— 直接 re-export Pi SDK
import {
  AgentSession,
  AgentProvider,
  ModelDefinition,
  // Pi SDK exports
} from "@earendil-works/pi-coding-agent";

// Conductor 不再包一层接口,直接用 Pi 类型
export type ConductorSession = AgentSession;        // alias
export type ConductorProvider = AgentProvider;      // "anthropic" | "openai" | ...
export type ConductorModel = string;                // "claude-opus-4-6" | "gpt-5" | ...

// Pi SDK 包装(只是构造参数)
export interface ConductorLaunchSpec {
  provider: AgentProvider;       // Pi provider
  model: string;                 // Pi model
  cwd: string;                   // Pi cwd
  skills?: string[];             // Pi skill 路径
  mcpConfig?: string;            // Pi MCP config 路径
  systemPrompt?: string;         // Pi system prompt 追加
  thinking?: "off" | "low" | "medium" | "high";
  tools?: { allow?: string[]; exclude?: string[] };
  // Conductor 注入
  isolatedHome: string;          // §6.2 隔离 HOME 路径
  sessionDir: string;            // Pi session 存放位置(在 $CONDUCTOR_HOME/runs/<runId>/pi-sessions)
}

export async function createPiSession(
  spec: ConductorLaunchSpec,
  signal?: AbortSignal,          // §14 取消
): Promise<AgentSession> {
  // Pi SDK 启动 session
  // env 注入隔离 HOME(§6.2)
  // AbortSignal → Pi session 的 cancel()
}
```

> **v0.11 vs v0.10 对比**:v0.10 有通用 `AgentClient/AgentSession` 接口 + `PiAgentClient` 实现;v0.11 **删除接口**,Pi 类型直接用。



| Conductor 概念 | Pi 等价物 | Conductor 如何复用 |
|---|---|---|
| AgentSpec.provider | `--provider` (Pi) | 直接传 |
| AgentSpec.model | `--model` (Pi) | 直接传 |
| AgentSpec.systemPrompt | `--append-system-prompt` (Pi) | 直接传 |
| AgentSpec.skills | Pi skill 目录 | `$CONDUCTOR_HOME/skills/<name>/SKILL.md` 软链到 Pi 路径 |
| AgentSpec.mcp | `--mcp-config` (Pi) | 直接传 JSON 路径 |
| AgentSpec.tools | `--tools` / `--exclude-tools` (Pi) | 直接传 |
| AgentSpec.thinking | `--thinking` (Pi) | 直接传 |
| Session JSONL | Pi 自带 session 目录 | **直接读 Pi JSONL**,不复制 |
| Resume | Pi session resume | 直接用 Pi API |
| Compaction | Pi compaction | 直接用 Pi (`/compact` slash command) |
| Skill (workflow) | Pi skill | Conductor skill 注册即 Pi skill |
| Approval | Pi approval model | 直接用 Pi `await approval()`,Conductor 在上面包装 UI |
| Slash command | Pi slash command | 直接暴露 |

> **结论**:Conductor 的"skill / session / approval"等概念**全是 Pi 的概念**。Conductor 主要工作是:
> 1. 包装 Pi SDK 启动逻辑
> 2. 注入隔离 HOME(§6.2)
> 3. 包装 Pi session → Conductor Runner
> 4. 在 Pi 上 加 workflow / Hub / contextBus

```typescript
// packages/protocol/src/provider.ts —— 跨 provider 抽象

export type AgentProvider =
  | "pi" | "claude" | "codex" | "omp"
  | `acp:${string}`     // 任意 ACP agent
  | `custom:${string}`; // 自定义

export interface AgentClient {
  readonly provider: AgentProvider;
  readonly capabilities: AgentCapabilityFlags;

  createSession(
    config: AgentSessionConfig,
    launch?: AgentLaunchContext,
    opts?: AgentCreateSessionOptions,
    signal?: AbortSignal,         // ← §14 取消入口
  ): Promise<AgentSession>;

  resumeSession(
    handle: AgentPersistenceHandle,
    overrides?: Partial<AgentSessionConfig>,
    launch?: AgentLaunchContext,
    signal?: AbortSignal,
  ): Promise<AgentSession>;

  fetchCatalog(opts: FetchCatalogOptions, signal?: AbortSignal): Promise<ProviderCatalog>;
  isAvailable(signal?: AbortSignal): Promise<boolean>;

  // 可选:导入历史会话
  listImportableSessions?(opts?: ListImportableSessionsOptions, signal?: AbortSignal): Promise<ImportableProviderSession[]>;
  importSession?(input: ImportProviderSessionInput, ctx: ImportProviderSessionContext, signal?: AbortSignal): Promise<ImportedProviderSession>;
}

export interface AgentSession {
  readonly id: string;
  readonly provider: AgentProvider;

  send(prompt: AgentPrompt, opts?: SendOptions, signal?: AbortSignal): Promise<AgentTurnResult>;

  // 流式订阅(多 turn event stream)
  events(): AsyncIterable<AgentStreamEvent>;

  // §14 取消
  cancel(signal?: AbortSignal): Promise<void>;
  rewind(opts?: RewindOptions, signal?: AbortSignal): Promise<void>;
  persist(signal?: AbortSignal): Promise<AgentPersistenceHandle>;
  close(signal?: AbortSignal): Promise<void>;
}
```

> **v0.9 关键变更**:取消原语从 `context.Context` 改为 **`AbortSignal`**(Node 原生)。所有 API 接收 `signal?: AbortSignal`,消费端:
>
> ```typescript
> for await (const ev of session.events()) {
>   if (signal.aborted) break;
>   // 处理 ev
> }
> ```
>
> `AbortController` 多源合并(`AbortSignal.any([user, hub, workflow])`)是 §14 三路合一在 Node 下的答案。
>
> **v0.10 关键变更**(取消 escape hatch):Pi 不是 LLM wrapper,**Pi 是完整 coding agent**(内置 read/write/edit/bash 工具、session 管理、skill/extension 体系、MCP 支持)。它本身就能替代 Claude Code / Codex / OpenCode 的角色,且内置支持 20+ 模型。**Phase 1 完全不实现 Claude SDK escape hatch**——见 §5.2 Pi 实现细节。



## 6. Agent Runner 层

职责单一:**管好一个 AgentSession 的生命周期**。

- 进程所有权:谁 `spawn` 谁负责清理。**绝对不要**把 spawned process 留在 readiness 里——Paseo `providers.md` 明确警告过这条。Go 实现里用 `cmd.Process.Kill()` + `defer cmd.Wait()` 保证清理。
- 事件流:把 subprocess / SDK 的事件归一为 `AgentStreamEvent`(text/tool_call/permission/subagent/finish/error)。消费端用 `for ev := range runner.Events()`。
- 持久化:每次 `Persist()` 返回 `AgentPersistenceHandle`,崩溃后可 `client.ResumeSession(ctx, handle, ...)`。
- **取消语义**:`runner.Cancel(ctx)` 内部调用 `session.Cancel(ctx)`(底层 SIGTERM → grace → SIGKILL),详见 §14。
- **上下文边界**:`AgentRunner` 只持有自己的 timeline;跨 runner 的上下文通过 Worker 编排层显式传参,不共享内存。

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

> 这是 Conductor **不重造子 agent 引擎**的核心保证。

### 6.2 Subprocess Environment Isolation(Phase 1 必做)

> v0.7 新增。用户问题:"Claude Code 和 Codex 支持 HOME 环境变量,是不是在 Conductor 中使用隔离的 HOME,防止干扰用户自己的使用?"
>
> **答案:是的——必须 per-run 隔离 HOME**。不隔离会导致 session 污染、配置冲突、并发写冲突、审计混乱。

#### 6.2.1 不隔离的冲突类型

| 冲突 | 影响 |
|---|---|
| **Session 污染** | Conductor 跑的 session JSONL 写到用户 `~/.claude/projects/.../`,用户在终端跑 Claude Code 时看到"鬼魂" session |
| **配置冲突** | 用户改全局设置 → Conductor 下次 spawn 用新设置,行为不可预期 |
| **并发写同一文件** | OAuth refresh、设置更新可能撞车 |
| **审计混乱** | 用户的 `~/.claude/logs/` 混了 Conductor 活动 |
| **Provider config 冲突(v0.12 新增)** | **Codex runtime 锁定一个 provider**,两个 spec 想用不同 provider 时,共享 HOME 会导致 config.toml 冲突。**per-spec 隔离 HOME 解决**(不同 spec 不同 HOME) |

#### 6.2.2 设计:Per-Spec 隔离 HOME + Auth Symlink + 动态 config.toml

```go
// internal/runner/isolated_home.go
type IsolatedHome struct {
    Dir       string      // $CONDUCTOR_HOME/runs/<runId>/home/
    AuthLinks []AuthLink  // 软链到用户真实 HOME 的 auth 文件
}

type AuthLink struct {
    Provider string  // "claude" | "codex" | ...
    Target   string  // 真实 HOME 中的文件
    Link     string  // 隔离 HOME 中的对应路径
}

func NewIsolatedHome(provider, runID string) (*IsolatedHome, error) {
    iso := &IsolatedHome{
        Dir: filepath.Join(conductorHome(), "runs", runID, "home"),
    }
    // **v0.8 优化**:auth 链到共享 $CONDUCTOR_HOME/.auth/ 而不是用户 HOME
    // → 所有 run 共享一个 auth 源 → 单一重置点
    authDir := filepath.Join(conductorHome(), ".auth")
    switch provider {
    case "claude":
        iso.AuthLinks = append(iso.AuthLinks, AuthLink{
            Provider: "claude",
            Target:   filepath.Join(authDir, ".claude.json"),      // ← .auth 里的 symlink(指向用户)
            Link:     filepath.Join(iso.Dir, ".claude.json"),
        })
        // ~/.claude/(session + logs)→ 不软链,在隔离 HOME 里新建
    case "codex":
        iso.AuthLinks = append(iso.AuthLinks, AuthLink{
            Provider: "codex",
            Target:   filepath.Join(authDir, "codex", "auth.json"), // ← .auth/codex/auth.json
            Link:     filepath.Join(iso.Dir, ".codex", "auth.json"),
        })
    }
    return iso, nil
}

// 新增 $CONDUCTOR_HOME/.auth/ 管理
func EnsureAuthDir() error {
    authDir := filepath.Join(conductorHome(), ".auth")
    if err := os.MkdirAll(authDir, 0700); err != nil { return err }

    // 首次 init:把用户 HOME 的 auth 文件链入 .auth/
    claudeAuth := filepath.Join(authDir, ".claude.json")
    if _, err := os.Lstat(claudeAuth); os.IsNotExist(err) {
        if _, err := os.Stat(filepath.Join(userHome(), ".claude.json")); err == nil {
            _ = os.Symlink(filepath.Join(userHome(), ".claude.json"), claudeAuth)
        }
    }
    // codex 同理
    codexAuthDir := filepath.Join(authDir, "codex")
    _ = os.MkdirAll(codexAuthDir, 0700)
    codexAuth := filepath.Join(codexAuthDir, "auth.json")
    if _, err := os.Lstat(codexAuth); os.IsNotExist(err) {
        userCodexAuth := filepath.Join(userHome(), ".codex", "auth.json")
        if _, err := os.Stat(userCodexAuth); err == nil {
            _ = os.Symlink(userCodexAuth, codexAuth)
        }
    }
    return nil
}

// CLI 命令
//   conductor init      → 首次跑,建 .auth/
//   conductor auth reset → rm -rf .auth/(完全脱离用户 auth)

func (h *IsolatedHome) Setup() error {
    if err := os.MkdirAll(h.Dir, 0700); err != nil { return err }
    for _, l := range h.AuthLinks {
        _ = os.MkdirAll(filepath.Dir(l.Link), 0700)
        if err := os.Symlink(l.Target, l.Link); err != nil && !os.IsExist(err) {
            return err
        }
    }
    return nil
}

func (h *IsolatedHome) Env() []string {
    return []string{
        "HOME=" + h.Dir,
        "XDG_CONFIG_HOME=" + filepath.Join(h.Dir, ".config"),
        "XDG_DATA_HOME=" + filepath.Join(h.Dir, ".local/share"),
        "XDG_CACHE_HOME=" + filepath.Join(h.Dir, ".cache"),
    }
}

func (h *IsolatedHome) Cleanup() error { return os.RemoveAll(h.Dir) }
```

**spawn 时**:
```go
cmd := exec.CommandContext(ctx, "claude", args...)
cmd.Env = append(os.Environ(), iso.Env()...)  // 覆盖 HOME + XDG_*
cmd.Dir = spec.WorktreePath()
```

#### 6.2.3 关键设计决策

| 决策 | 理由 |
|---|---|
| **Per-spec 隔离 HOME**(不是 per-run,不是 per-invocation) | **v0.12 二次修正**(用户洞察)——spec 是用户定义的复用单元,HOME 与 spec 同生命周期;同 spec 多次 invoke 共享 HOME,session 可 resume |
| **Symlink auth 到 `$CONDUCTOR_HOME/.auth/<provider>/`**(按 provider 分目录) | 不同 provider 可能有不同 auth(OpenAI key vs OpenRouter key vs 本地无 auth) |
| **Symlink .auth/ 里的文件到用户 HOME** | 默认仍跟用户 OAuth 实时同步 |
| **动态生成 `~/.codex/config.toml`**(spec 创建时一次写) | **v0.12 新增**——Conductor 按 spec.provider 写 config.toml 到 HOME,user 不需要手动管;后续 invoke 只读 |
| **隔离 session / logs / MCP config** | Conductor 产生的状态,不该污染用户,也不该 spec 间互窜 |
| **位置:`$CONDUCTOR_HOME/specs/<specId>/home/`** | **v0.12 二次修正**——spec 级 HOME,跨 run 持久,session JSONL 可跨 invoke resume |
| **权限 0700** | auth token 敏感 |
| **默认保留 Cleanup**(不删) | 便于 inspect session JSONL;`conductor prune --spec <id>` 时清 |

#### 6.2.4 边界与副作用

1. **用户不能直接 `claude --resume <id>` 续 Conductor session**——JSONL 在隔离 HOME 不在 `~/.claude/`。这是 §12.0.5 的另一面:想续需知道路径(用户层操作,非 Conductor 责任)
2. **磁盘占用**:每 run <几 MB(除非 session 很长);在 run archive 时清
3. **OAuth 软链边界**:用户删除 `~/.claude.json` → Conductor run 断 auth(合理的)
5. **跨 run 共享 session**:Phase 1 不支持,每 run 新 session
6. **多 provider 同时跑**:不同 provider 用不同隔离 HOME

#### 6.2.5 v0.12 修订:Per-Spec HOME + 动态 config.toml

> **v0.12 关键修正**(用户洞察:per-invocation 走过头了,per-spec 才是正确粒度):Spec 是 AgentSpec 静态定义,**spec 创建时写 config.toml,后续 invoke 共享同一 HOME**。

**为什么 per-spec 而不是 per-invocation**:
- per-invocation:**每次 Codex spawn 都重写 config.toml,丢失 session 状态**
- per-spec:**spec 创建时一次性写 config.toml,多次 invoke 共享 session 可 resume**
- per-spec:**spec 是用户定义的复用单元,HOME 与 spec 同生命周期**

**Spec 生命周期**:
```bash
# 1. 创建 spec(写 HOME + config.toml)
conductor spec create --name claude-opus-planner \
  --provider openrouter \
  --model anthropic/claude-opus-4-6 \
  --skills [...] --mcp [...]
# → Conductor 生成 specId,创建 specs/<specId>/home/,写 config.toml

# 2. 多次 invoke 同一 spec(共享 HOME)
conductor run --spec claude-opus-planner "task 1"
conductor run --spec claude-opus-planner "task 2"      # 共享 session JSONL,Codex 可 resume
conductor run --spec claude-opus-planner --resume <id> "continue"

# 3. 不同 spec 用不同 HOME
conductor run --spec gpt5-coder "implement auth"        # specs/gpt5-coder/home/
conductor run --spec gemini-reviewer "review"           # specs/gemini-reviewer/home/
```

**目录结构(v0.12 修订)**:

```
$CONDUCTOR_HOME/
├── .auth/                                  ← 共享 auth(按 provider 分)
│   ├── openai/auth.json
│   ├── openrouter/auth.json
│   └── ollama/(本地无 auth)
├── specs/                                  ← per-spec HOME
│   ├── claude-opus-planner/                ← spec ID(用户命名或 hash)
│   │   ├── spec.json                       ← spec 定义
│   │   └── home/                            ← per-spec HOME
│   │       ├── .codex/
│   │       │   ├── config.toml             ← spec 创建时一次写
│   │       │   └── sessions/<s>.jsonl      ← Codex session 持久化
│   │       └── .codex.json → $CONDUCTOR_HOME/.auth/openrouter/auth.json
│   ├── gpt5-coder/
│   │   └── home/                            ← 不同 spec 独立 HOME
│   └── gemini-reviewer/
│       └── home/
└── runs/<runId>/                           ← per-run state(不变)
    ├── state.json
    ├── timeline.ndjson
    └── blobs/
```

**动态生成 config.toml 示例**(spec 创建时执行一次):

```typescript
async function createSpec(specDef: AgentSpec): Promise<SpecRecord> {
  const specId = hashSpec(specDef);  // 或用户命名

  // per-spec HOME(spec 创建时一次性建立)
  const home = NewIsolatedHome({
    provider: specDef.provider,
    specId,
  });
  await home.Setup();

  // 写 config.toml(只这一次,后续 invoke 共享)
  await home.WriteFile(".codex/config.toml", `
model_provider = "${specDef.provider}"

[model_providers.${specDef.provider}]
name = "${specDef.providerLabel}"
base_url = "${specDef.baseUrl}"
env_key = "${specDef.envKey}"
`);

  // 按 provider 软链 auth
  await home.LinkAuth(specDef.provider);

  // 保存 spec 定义(供后续 invoke 查找)
  await storage.SaveSpec({
    id: specId,
    definition: specDef,
    homePath: home.Dir,
    createdAt: Date.now(),
  });

  return { id: specId, home };
}

async function invokeSpec(specId, prompt, signal) {
  const spec = await storage.LoadSpec(specId);
  const home = IsolateHome.Open(spec.homePath);  // 复用
  const cmd = spawn("codex", ["app-server"], {
    env: { ...process.env, HOME: home.Dir, ...spec.definition.env },
    signal,
  });
  return new CodexSession(cmd, spec, signal);
}
```

**关键边界**:
- **不同 spec** → **不同 HOME**(防 provider / config 冲突)
- **同 spec 多次 invoke** → **共享 HOME**(config.toml 一次写,session JSONL 可跨 invoke resume)
- **同 spec 并发 invoke** → ⚠️ 共享 HOME,**config.toml 只读不冲突,session ID 唯一不冲突**,但其他 state 可能短暂 race(可加 file lock 或接受)
- spec 创建是一次性成本,长期复用

**Per-Spec vs Per-Invocation 对比**:

| 维度 | per-invocation(原) | **per-spec(现)** |
|---|---|---|
| config.toml 写次数 | N 次 invoke | **1 次 spec 创建** |
| Session 跨 invoke 状态 | ❌ 重启 | ✅ 可 resume |
| 磁盘占用 | N 个 HOME | **1 个 HOME per spec** |
| 同 spec 并发 invoke | 各自独立 | ⚠️ 共享,需注意 |
| spec 隔离(provider 不同) | ✅ | ✅ |
| 实现复杂度 | 中(每次写 config)| **低(spec 创建写一次)** |

**`specId` 来源**:
- 用户命名:`conductor spec create --name my-spec`(推荐,user-friendly)
- 字段 hash:`sha256(provider|model|skills|mcp|...)`(确定性,可复现)
- 推荐:**用户命名 + hash 校验**(用户命名优先,如果重名则追加 hash 后缀)

#### 6.2.6 集成到 SubprocessClient(§17.2 D2)

```go
type SubprocessClient struct {
    cmd  *exec.Cmd
    home *IsolatedHome  // 新增
    // ...
}

func NewSubprocessClient(ctx context.Context, name string, args []string, spec AgentSpec) (*SubprocessClient, error) {
    iso, err := NewIsolatedHome(spec.Provider, spec.RunID)
    if err != nil { return nil, err }
    if err := iso.Setup(); err != nil { return nil, err }

    cmd := exec.CommandContext(ctx, name, args...)
    cmd.Env = append(os.Environ(), iso.Env()...)
    cmd.Dir = spec.WorktreePath()

    return &SubprocessClient{cmd: cmd, home: iso, /* ... */}, nil
}

func (c *SubprocessClient) Close(ctx context.Context) error {
    err := c.shutdown(ctx)
    _ = c.home.Cleanup()  // log if fails
    return err
}
```

#### 6.2.7 与 §6.1(不干预 subagent)的关系

- §6.1:Conductor 不干预 provider 内部 subagent
- §6.2:Conductor 通过隔离 HOME 给 provider 一个"干净沙箱",**但 auth 共享**
- 两者正交:一个是行为约束,一个是环境约束

#### 6.2.9 v0.8 优化:共享 `.auth/` 目录(替代 v0.7 直链用户 HOME)

> v0.7 设计每 run symlink auth 到用户 HOME——会有 N 个 symlink 指向同一文件,v0.8 引入中间层 `$CONDUCTOR_HOME/.auth/`。

**对比**:

```
v0.7 (per-run symlink → user home):
$CONDUCTOR_HOME/runs/run1/home/.claude.json → ~/.claude.json
$CONDUCTOR_HOME/runs/run2/home/.claude.json → ~/.claude.json
$CONDUCTOR_HOME/runs/run3/home/.claude.json → ~/.claude.json
...(N 个 symlink 到同一文件)

v0.8 (per-run symlink → shared .auth):
$CONDUCTOR_HOME/.auth/.claude.json → ~/.claude.json  (1 个 symlink)
$CONDUCTOR_HOME/runs/run1/home/.claude.json → $CONDUCTOR_HOME/.auth/.claude.json
$CONDUCTOR_HOME/runs/run2/home/.claude.json → $CONDUCTOR_HOME/.auth/.claude.json
$CONDUCTOR_HOME/runs/run3/home/.claude.json → $CONDUCTOR_HOME/.auth/.claude.json
```

**为什么更好**:
- **Auth 单一来源**:`.auth/` 是 Conductor 唯一关心的 auth 位置
- **重置简单**:`conductor auth reset` = `rm -rf $CONDUCTOR_HOME/.auth/`,再 `conductor init` 重建
- **可切断与用户 auth 关联**:Phase 2 可加 `conductor auth copy-from-user`,把 symlink 换成真实 copy,Conductor 独立管 auth
- **更易备份**:备份 Conductor auth = 备份 `$CONDUCTOR_HOME/.auth/`

**磁盘影响**:
- v0.7:每 run 1 个 auth symlink(~100B),100 run = 10KB
- v0.8:1 个 `.auth/` symlink + 每 run 1 个 auth symlink(基本同 v0.7)
- **实际差异微乎其微**,真正的价值是**清晰的管理边界**

**完整目录结构**:

```
$CONDUCTOR_HOME/
├── .auth/                          ← 共享 auth(v0.8 新增)
│   ├── .claude.json    → ~/.claude.json  (symlink)
│   └── codex/auth.json  → ~/.codex/auth.json (symlink)
├── runs/
│   ├── <runId-1>/
│   │   ├── state.json
│   │   ├── timeline.ndjson
│   │   ├── blobs/
│   │   └── home/
│   │       ├── .claude.json    → $CONDUCTOR_HOME/.auth/.claude.json
│   │       └── .claude/projects/...   (per-run sessions)
│   └── <runId-2>/
│       └── home/
│           └── ...
```

**为什么仍 per-run session/logs**:
- 两个 run 在同一 cwd → 同一 project hash → session 文件名不同(用 session ID),但 logs / settings 文件会冲突
- per-run 隔离 → 零冲突 + 一刀切清理 + 易 debug

#### 6.2.8 e2e 测试

| 测试 ID | 场景 | 期望 |
|---|---|---|
| `T_isolated_home_created` | spawn Claude Code | `$CONDUCTOR_HOME/runs/<runId>/home/.claude.json` 是 symlink(指向 `.auth/`),`~/.claude/projects/` 是新 dir |
| `T_no_user_pollution` | run 完成后 | 用户 `~/.claude/` 无新增 session/log |
| `T_auth_via_symlink` | 用户 OAuth 改后,Conductor 新 run | 新 run 用新 auth(通过 .auth/ 链) |
| `T_auth_reset` | `conductor auth reset` | `.auth/` 被删,下次 run 自动重建 |
| `T_cleanup` | `conductor prune --run <runId>` | 隔离 HOME 被删除,.auth/ 保留 |
| `T_concurrent_runs` | 同时跑 3 个 task | 各自隔离 HOME,session 互不可见 |
| `T_xdg_respected` | spawn 检查 env | `HOME` / `XDG_*` 都正确覆盖 |





#### 6.2.10 为什么 Per-Spec(而不是 Per-Run 或 Per-Invocation)

> **v0.12 立场反转**(用户洞察)。早期版本 §6.2.10 反对 per-spec,后来改为支持 per-spec。本节重写反映 v0.12 定论。

**为什么 per-spec 是正确的复用边界**:

- Spec = 用户定义的 agent 模板(provider + model + skills + mcp + worktree ...)
- Spec 是**长寿命实体**,run 是**短寿命事件**
- Spec 自然复用 → 同 spec 多次 invoke → **共享 HOME 才是正确语义**
- Spec 改 provider → 新 specId → 新 HOME(用户明确升级动作)

**Per-Run 的问题(早期选择的局限)**:

| 问题 | 后果 |
|---|---|
| 同一 run 不同 stage 不同 provider | ❌ 一个 HOME 不能放两个 config.toml |
| 同 spec 多次 run 不复用 HOME | ❌ 每次重新建 HOME,config.toml 重复,浪费 |
| Session 不能跨 run 续 | ❌ JSONL 在 `runs/<runId>/...`,run 结束清理就没了 |

**Per-Invocation 的问题(我上一轮走过头)**:

| 问题 | 后果 |
|---|---|
| 每次 spawn 重写 config.toml | ❌ 浪费,失去 spec 作为复用单元的意义 |
| Session 不能跨 invoke 续 | ❌ JSONL 在 `invocations/<id>/...`,每次重起 |
| 磁盘 N 个 HOME per spec | ❌ 浪费 |

**Per-Spec 解决了所有这些**:

| 场景 | Per-Spec 行为 |
|---|---|
| Run A stage 1 用 spec P1(OpenRouter/Claude)| `specs/P1/home/` → config.toml 写 openrouter |
| Run A stage 2 用 spec P2(OpenAI/GPT-5)| `specs/P2/home/` → 不同 HOME,config.toml 写 openai |
| 同 spec 多次 invoke | 共享 `specs/P1/home/`,session JSONL 可 resume |
| 并发不同 run 用同 spec | 共享 `specs/P1/home/`(假设不发生,见下)|
| Spec 改 provider | 用户新建 spec,新 specId,新 HOME |

**v0.12 假设**:**同一 spec 不会并发 invoke**。

理由:
- Spec 是 managed 资源(CLI/UI 创建,持久)
- `conductor run --spec <id>` 是 invocation 的唯一入口
- 管理页面保证不会同时启动两个相同 spec 的 invocation(UX 层防)
- 真要并发,用户显式创建两个不同 spec(自然隔离)

**为什么不做并发保护**:
- 用户场景里几乎不会出现"同 spec 并发 invoke"
- Codex 内部 state 写冲突是边缘 case,加 file lock 复杂度不值得
- 如果 Phase 3+ 真有需求,加 file lock 是 ~10 行代码

**Spec 持久化结构**:

**`specId` 来源**(v0.12 决定):

- **用户命名**为主:`conductor spec create --name my-claude-planner`
- **内容 hash** 校验:spec 字段 hash,确保相同字段同名 = 同一 spec
- **冲突解决**:用户命名 + hash 后缀(`my-claude-planner-abc123`)

**Spec 持久化结构**:

```
$CONDUCTOR_HOME/specs/<specId>/
├── spec.json              ← spec 定义(provider, model, skills, mcp, worktree)
└── home/                   ← per-spec HOME
    ├── .codex/config.toml  ← spec 创建时一次写
    ├── .codex/sessions/<s>.jsonl
    └── .codex.json → $CONDUCTOR_HOME/.auth/<provider>/auth.json
```

**CLI 命令**(Phase 1):

```bash
conductor spec create --name my-claude-planner \
  --provider openrouter \
  --model anthropic/claude-opus-4-6 \
  --skills [...] --mcp [...]

conductor spec list                  # 列出所有 spec
conductor spec show <specId>         # 查看 spec 详情
conductor spec rm <specId>           # 删除 spec + HOME

conductor run --spec <specId> "..." # invoke spec
conductor run --spec <specId> --resume <sessionId> "..."  # 续 session
```

---

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

**Go 实现要点**(并发原语):
- parallel stage 用 `golang.org/x/sync/errgroup`,`g, ctx := errgroup.WithContext(parentCtx)`,`g.Go(func() error { ... })`。
- `errgroup` 自带取消传播——任何一个 stage 失败,其他 stage 的 ctx 自动 Done。
- loop 用 `for { ... if until { break } }`,`until` 表达式求值从 `ctx.Prev()` 取数据。
- 所有 stage 函数签名:`func(ctx context.Context, s StageContext) (Result, error)`——`ctx` 是取消入口,`s` 是业务上下文。

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

### 10.2 Registry — 进程内(Phase 1)+ Hub as Dispatcher(Phase 2+)

> **v0.6 重大调整**:用户明确"task 不要跨机器,Hub 只是分发不同 task 到不同 host"。
>
> **结论**:
> - ❌ SessionMigrator(§10.4)—— **彻底不做**
> - ❌ "无缝"边界(§10.5)—— **彻底不做**
> - ✅ Hub(Phase 2+)——**只做 task dispatcher**,不做迁移
> - ✅ Task 永远绑定一个 host,host 死 → task 死,用户重试

**Phase 1**:只有进程内 `PlayerRegistry`,无 Hub。

```go
// internal/registry/player.go —— 进程内注册表
type PlayerRegistry struct {
    mu        sync.RWMutex
    agents    map[AgentID]*AgentRunner
    providers map[ProviderID]AgentClient
    workflows map[WorkflowID]*WorkflowInstance
    worktrees map[WorktreeID]*WorktreeHandle
}

func (r *PlayerRegistry) RegisterAgent(a *AgentRunner) { ... }
func (r *PlayerRegistry) Heartbeat() HealthSnapshot { ... }
func (r *PlayerRegistry) Prune(ctx context.Context) error { ... }

// internal/hub/hub.go —— 跨 host 注册中心
type PlayerHub struct {
    players   map[PlayerID]*PlayerEndpoint
    workflows map[WorkflowID]*WorkflowHandle
    router    *Router  // 选 host 路由策略
}

func (h *PlayerHub) Register(p *PlayerEndpoint) error { ... }
func (h *PlayerHub) Heartbeat(p PlayerID, snap HealthSnapshot) error { ... }
func (h *PlayerHub) RouteWorkflow(wf WorkflowHandle, sel PlayerSelector) (*PlayerEndpoint, error) { ... }
func (h *PlayerHub) MigrateWorkflow(wf WorkflowID, from, to PlayerID) error { ... }

type PlayerSelector interface {
    Select(players []*PlayerEndpoint) (*PlayerEndpoint, error)
}
// Phase 4+ 实现:
//   AnySelector{}           // 任意健康 host
//   TagSelector{Tags:...}   // host tag(架构、GPU、地域)
//   ProviderSelector{Name}  // 必须有某 provider
//   PinnedSelector{Player}  // 钉死某 host
```

> **Phase 1 简化**:Hub 整个 §10.2 后半段不做,`PlayerHub` 类型保留在代码里但 phase 1 不实例化。

> Hub 与 Player 间是 WS 长连接 + 心跳;Player 离线后 Hub 把 run 迁到其他健康 host(§11.6 恢复)。

### 10.3 Worktree 隔离(Phase 1 必做)

并行 agent 在同一 repo 上工作时必须隔离(否则 git 工作树互相覆盖)。直接复用 Paseo `server/worktree/` 的模式:每条并行 branch 自动 `git worktree add` 到 `.conductor/worktrees/<branch>`。

**Phase 1 也需要**:即使没有 Hub,本机并行 agent 仍要 worktree 隔离(parallel stages 共享同一 host)。

> ~~v0.4 §10.4 跨 Host Session 迁移 / v0.4 §10.5 "无缝"边界 —— v0.6 彻底删除,不进入设计。~~

## 11. 持久化与可观测

### 11.1 持久化

v0.2 决策:**默认 JSON + SQLite 一等切换**(Go 实现)。

**Storage Interface**(Go):

```go
// internal/storage/storage.go
type Storage interface {
    // 原子写入
    PutWorkflow(ctx context.Context, state WorkflowState) error
    GetWorkflow(ctx context.Context, runID string) (*WorkflowState, error)
    ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowSummary, error)

    PutAgent(ctx context.Context, spec AgentSpec, instance AgentInstance) error
    GetAgent(ctx context.Context, id string) (*AgentRecord, error)

    // timeline 永远 append-only
    AppendTimeline(ctx context.Context, agentID string, item TimelineItem) error
    ReadTimeline(ctx context.Context, agentID string, q TimelineQuery) (<-chan TimelineItem, error)

    // Blobs(大对象 offload)
    PutBlob(ctx context.Context, sha256 string, r io.Reader) (int64, error)
    GetBlob(ctx context.Context, sha256 string) (io.ReadCloser, error)
}

type WorkflowFilter struct {
    Status   []string
    Since    *time.Time
    Limit    int
    // ...
}
```

**两个实现**(同一接口):
- **`JsonFileStorage`** (Phase 1 默认):`$CONDUCTOR_HOME` 下分目录,workflow 存 `runs/<id>/state.json`,timeline 用 append-only NDJSON,blob 走文件系统。
- **`SqliteStorage`** (Phase 1 同步实现):`modernc.org/sqlite`(纯 Go,无 cgo);schema 与 JSON 等价。

**运行时切换**:
```bash
CONDUCTOR_STORAGE=json  # 默认
CONDUCTOR_STORAGE=sqlite # 切 SQLite(同一文件:$CONDUCTOR_HOME/conductor.db)
```

切换时机:数据量到 10k+ runs 或并发写多 stage 时切 SQLite;小规模调试用 JSON(更易人肉读)。

### 11.1.1 wire schema 不变

两个 backend 共享同一个 Go struct + JSON Schema;JSON 是直接序列化,SQLite 是 schema-as-table。读路径必须返回同一类型(由 struct tags 在边界校验),业务代码无感。

### 11.2 可观测

- **Timeline**:每个 agent 一条 timeline(text/tool_call/permission/subagent/finish/error),统一格式。
- **Logs**:结构化日志(参考 Paseo `daemon.log`),由 pino 或类似 logger 输出。
- **Replay**:因为 timeline + persistence handle 都在,可以重放任意时刻。

## 12. Workflow Context — 跨步骤传递(详细设计)

v0.2 重点新增。Context 是 PDCA 工作流的脊柱,设计错会让长跑 workflow 不可恢复、跨 host 不可迁移。

> **v0.6 作用域澄清**:
> - `WorkflowContext` = **per-task, per-host**——一个 task 的所有 stage 共享同一 context,但**不同 task 之间的 context 完全独立**
> - Ref 永远指向**本机资源**(文件、worktree、session、blob)
> - 没有跨 task / 跨 host 的 context bus
> - Host 死了 → context 随 task 一起死;用户重试 → 新 task,新 context

详见 §12.0 边界说明。

### 12.0 边界(Per-Task, Per-Host)

> v0.6 新增。在 v0.5 的 "本地优先" 基础上,进一步明确 contextBus 的物理边界。

#### 12.0.1 物理边界

```
              Host A                           Host B
         ┌─────────────────┐             ┌─────────────────┐
         │ WorkflowState   │             │ WorkflowState   │
         │  HostID: A      │             │  HostID: B      │
         │  Stages: {...}  │             │  Stages: {...}  │
         │  Refs: {...}    │             │  Refs: {...}    │
         │  BlobStore: ... │             │  BlobStore: ... │
         └─────────────────┘             └─────────────────┘
                ↑                                ↑
                │ 完全独立                        │
                │ (不同 task)                    │
                └────────────────────────────────┘
                         (无共享)
```

**强不变量**:
- `WorkflowState.HostID` 一旦设定,**整个 task 生命周期不变**
- 同 task 的所有 stage 共享同一 context(同 host)
- 跨 task 的 context **不共享**(Phase 1 默认)
- 跨 host 的 context **不共享**

#### 12.0.2 Ref 系统本机化

| Ref kind | 物理位置 | 跨 host? |
|---|---|---|
| `file` | 本机绝对路径 | ❌ |
| `worktree` | 本机 git worktree | ❌ |
| `session` | 本机 provider session handle | ❌ |
| `blob` | 本机 `$CONDUCTOR_HOME/runs/<runId>/blobs/<sha>` | ❌ |

**没有跨 host Ref**。如果 task A 想给 task B 数据,**Phase 1 不支持**;Phase 2+ 可走 Hub blob store(§10.2 已留接口但 Phase 1 不实现)。

#### 12.0.3 跨 Task 数据共享(Phase 2+ 可选)

如果将来需要"task B 依赖 task A 的输出":

| 方案 | 机制 | Phase |
|---|---|---|
| **A. Hub blob store** | Hub 维护 S3-compatible blob;task A 写,Hub 把 URL 给 task B | Phase 3+ |
| **B. Task chaining** | Hub 调度 task B 时把 task A 的 stage output 序列化成 inputs | Phase 2+ |
| **C. 不支持(用户手动)** | task 完全独立,数据用户 `cp` / `git push` | **Phase 1** |

**Phase 1 选 C**——task 完全独立,context 完全隔离。

#### 12.0.4 Checkpoint / Resume 语义

```go
// Phase 1 的 resume(同 host 内)
func ResumeWorkflow(ctx context.Context, runID string) error {
    state, err := storage.Load(runID)  // 从本机磁盘读
    if err != nil { return err }

    // 不变量校验
    if state.HostID != currentHostID() {
        return ErrWrongHost  // 本 task 绑了 host,新 host 上不能 resume
    }

    return runFrom(state.Cursor)
}
```

**跨 host 续跑的合法方式**:
- 方式 1:新 host 上**重新启动 task**(`conductor run ...`),从头跑
- 方式 2(高级):用户 `git push` worktree + `claude --resume <session-id>`,**用户层面**,不是 Conductor 责任

#### 12.0.5 Provider Session 也 Per-Host

```go
type SessionRef struct {
    Provider  string     // "claude"
    HostID    string     // session 在哪
    SessionID string     // provider session id
    Handle    AgentPersistenceHandle
}
```

**Session 不能跨 host resume**。技术上 Claude `--resume <id>` 可以,但要求新 host 的 `~/.claude/projects/...` 有对应 JSONL。Conductor **不做**自动跨 host session 搬运。

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

```go
type WorkflowState struct {
    RunID         string                     `json:"runId"`
    Workflow      string                     `json:"workflow"`
    Spec          WorkflowSpec               `json:"spec"`            // 描述(可热重载新版)
    Inputs        RunInputs                  `json:"inputs"`
    Stages        map[string]StageOutput     `json:"stages"`          // 只存 inline 部分
    Refs          RefIndex                   `json:"refs"`
    Cursor        Cursor                     `json:"cursor"`
    Meta          WorkflowMeta               `json:"meta"`
    SchemaVersion int                        `json:"schemaVersion"`  // 迁移用
}

type Cursor struct {
    Stage   string `json:"stage"`
    Attempt int    `json:"attempt"`
}

type WorkflowMeta struct {
    StartedAt    time.Time         `json:"startedAt"`
    Hosts        []string          `json:"hosts"`
    TotalCost    CostBreakdown     `json:"totalCost"`
    SchemaFields map[string]any    `json:"schemaFields,omitempty"`
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

### 12.11 Handoff & Session Transfer

> v0.4 补充。用户问题:"CLI 方式是不是没法实现 Paseo 的 handoff 逻辑?"

#### 12.11.1 Paseo 的 handoff 是什么(从源码确认)

读完 `references/paseo/skills/paseo-handoff/SKILL.md`,Paseo 的 handoff 是:

> **A prompt template + MCP tool invocation, NOT an SDK protocol feature.**
> "The receiving agent starts with zero context, so the handoff prompt must be a self-contained briefing."

具体步骤(SKILL.md 原文):
1. 调 `list_profiles` 选接收方 profile
2. 写结构化 briefing:Task / Context / Relevant files / Current state / What was tried / Decisions / Acceptance criteria / Constraints
3. 调 `create_agent` 启动新 agent,initialPrompt = briefing
4. 传 workspaceId(可选 worktree 隔离)
5. 不等结果,返回 agent 给用户

#### 12.11.2 三种 handoff 形态的可行性

| 形态 | Paseo | Conductor (CLI 模式) | 备注 |
|---|---|---|---|
| **显式 briefing** (人类结构化 prompt 传给新 agent) | ✅ | ✅ | prompt 工程,与协议无关 |
| **同 provider session 续跑** (`claude --resume <id>` / app-server resume) | ✅ | ✅ | CLI 自身能力 |
| **跨 provider 状态深传** (Claude 多轮 + tool 结果 → Codex) | ❌ | ❌ | open problem;SDK 也不解决 |

> **关键边界**:前两种 CLI 完全够用;第三种本质是 "把 A 的对话翻译成 B 能消费的 prompt"——这是 LLM 工作,不是协议工作。

#### 12.11.3 Conductor 的 handoff 设计

Conductor 比 Paseo 多两个 handoff 优势,因为 §12 已设计好:

**1) Briefing 模板 + 结构化输入**

```go
// internal/handoff/briefing.go
type Briefing struct {
    Task             string   `json:"task"`
    Context          string   `json:"context"`
    RelevantFiles    []string `json:"relevantFiles"`
    CurrentState     string   `json:"currentState"`
    WhatWasTried     []Attempt `json:"whatWasTried"`
    Decisions        []Decision `json:"decisions"`
    AcceptanceCriteria []string `json:"acceptanceCriteria"`
    Constraints      []string `json:"constraints"`
    // 自动从 §12 contextBus 提取
    AutoAttachments  []Ref    `json:"autoAttachments"`  // 工作文件、worktree、session handles
}

func ComposeBriefing(ctx StageContext) Briefing { ... }
```

**2) 跨 stage handoff(原生,不需要显式 skill)**

§12.2-12.4 已经把 handoff 做了——上一 stage 的 `StageOutput`(data + refs)就是天然 briefing。**Workflow engine 跨 stage 传递 = 结构化 handoff**,比 Paseo 的"自己手写 briefing"更系统。

**3) 显式 CLI handoff(用户主动触发)**

```go
// internal/handoff/transfer.go
type TransferRequest struct {
    To            AgentSpec         // 接收方 spec
    WorkspaceID   string            // 可选,worktree 隔离
    Briefing      Briefing          // 显式 briefing
    InheritRefs   []Ref             // 继承的 ref(工作文件 / session handle)
    ResumeSession bool              // 同 provider 是否续 session
}

func (e *Engine) Handoff(ctx context.Context, req TransferRequest) (*AgentRunner, error) {
    // 1. 拼装 initialPrompt = briefing markdown + 自动段(从 inherit refs)
    initialPrompt := composeInitialPrompt(req)

    // 2. 创建 workspace(如指定)
    if req.WorkspaceID == "" { req.WorkspaceID = createWorktree(ctx) }

    // 3. 启动新 agent(走 §5.1 AgentClient.CreateSession)
    runner, err := e.registry.CreateAgent(ctx, req.To, initialPrompt, req.WorkspaceID)

    // 4. 标记旧 agent 为 "handed-off"(可选自动 archive)
    if old, ok := e.registry.GetAgent(req.FromAgentID); ok {
        old.MarkHandedOff(runner.ID())
    }

    return runner, nil
}
```

**4) Session handle 作为 ref 传递**

```go
// Ref 系统中已有 session kind(§12.5):
{ kind: "session", provider: "claude", handle: AgentPersistenceHandle }

// 接收方 agent 启动时:
//   如果 spec.resumeSession && ref 是同 provider:
//     → ResumeSession(ctx, ref.handle) — 直接续 session
//   否则:
//     → CreateSession(...) — 新 session,briefing 作 initialPrompt
```

#### 12.11.4 与 Paseo 对比

| 维度 | Paseo handoff | Conductor handoff |
|---|---|---|
| 触发方式 | Skill(用户或 LLM 主动) | CLI / Workflow 自动 / Skill |
| Briefing 结构 | 固定模板(Markdown) | 固定模板 + autoAttachments |
| 跨 stage 自动 handoff | ❌(要 LLM 自己写 briefing) | ✅(`StageContext.Prev` 自动) |
| 跨 provider | ❌(只能 briefing 形式) | ❌(同 Paseo,open problem) |
| 同 provider session resume | ❌(handoff 不传 session handle) | ✅(传 session ref,可选 resume) |
| Worktree 隔离 | ✅(`create_workspace` 工具) | ✅(§10.3 同样支持) |

> **Conductor 优势**:同一 provider 的 session resume 通过 Ref 系统原生支持;跨 stage 的结构化 handoff 通过 WorkflowContext 原生支持。**Paseo 的 handoff 反而是"绕过 SDK 自己写 prompt 模板",Conductor 是把这种 handoff 内化为引擎能力。**

#### 12.11.5 CLI 形态

```bash
# 1) 用户主动 handoff
conductor handoff --from <agent-id>   --to codex/gpt-5.5   --briefing ./briefing.md   --worktree feature-x

# 2) Workflow 自动 handoff(在 stage spec 里声明)
{
  "name": "review_pr",
  "type": "handoff",
  "to": "claude/opus-4.6",
  "inheritRefs": ["diff", "worktree"],
  "resumeSession": false,
  "briefing": {
    "template": "./templates/review-pr.md",
    "autoFill": ["pr.url", "pr.title", "diff.summary"]
  }
}

# 3) handoff 后的 agent 出现在 §10 PlayerRegistry
conductor ls
  # → 列出原 agent (状态: handed-off → archive) + 新 agent
```

#### 12.11.6 不做的事(诚实清单)

- **不做**跨 provider 状态翻译(把 Claude 多轮 JSONL 转成 Codex prompt)
- **不做**自动 LLM-driven handoff(由 planner 决定何时 handoff);交回 §8.5 planner 由用户 skill 定义
- **不做** handoff 反悔/撤销;transfer 是一旦创建就 forward-only

---

## 13. 实施路线图

```
Phase 1 (MVP):
  - protocol/ skeleton + Go structs + JSON Schema
  - provider/ base (SubprocessClient + ProtocolParser) + **Claude 单一实现**
  - runner/ 生命周期 + 事件流 + context.Context 取消
  - storage/ 文件 JSON + SQLite 一等切换
  - daemon/ 入口 + pid-lock + WS
  - hub/ 跨 host 注册 + **session 迁移(§10.4 必做)**
  - cli/ run/ls/logs/send/wait/cancel + handoff

Phase 2:
  - worker/ 5 种节点类型
  - workflow/ 软工作流引擎(自研,§8.6 方案 A)+ PDCA/GSD 默认预设
  - registry/ 进程内注册表
  - worktree/ 自动隔离
  - cancellation/ §14 协议落地 + 单 host e2e
  - **Hub as dispatcher(§10.2)** —— 多 host 分发不同 task
  - (不实现 Claude SDK,见 Phase 3+)

Phase 3+ (按需):
  - **其他 provider 仅在 Pi 不够时**——如 Claude SDK escape hatch(若 Pi 某些 feature 缺失)
  - provider-subagents/ 跟踪 + UI(可选 web)
  - 更多编排原语(join 策略、loop 条件)
  - 多个 ACP provider(若 Pi 不能满足某些 ACP 场景)

Phase 4:
  - Hub HA(主备/共识)
  - 可选 Web UI
  - 可选 Postgres 后端
  - Tiered memory(Layer 4)
  - ~~SessionMigrator 跨 host 迁移~~ **不做**(用户决策 v0.6)
  - ~~"无缝"边界~~ **不做**
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

### 14.4 信号与超时(Go 实现)

```go
type CancelReason string

const (
    CancelUser            CancelReason = "user"
    CancelHub             CancelReason = "hub"
    CancelWorkflowGate    CancelReason = "workflow-gate"
    CancelRetryExhausted  CancelReason = "retry-exhausted"
    CancelLoopMaxIter     CancelReason = "loop-maxiter"
    CancelDependencyFail  CancelReason = "dependency-failed"
    CancelQuotaExceeded   CancelReason = "quota-exceeded"
)

type CancelOptions struct {
    Reason    CancelReason
    TimeoutMs int        // 默认 30000(grace period)
    Force     bool       // true → 立即 SIGKILL,跳过 grace
}

type CancelResult struct {
    Status        CancelStatus
    CancelledAt   time.Time
    Reason        CancelReason
    ProviderState ProviderResumeState  // "resumable" | "lost"
    ForcedKill    bool
}
```

**关键 Go 设计:`StageContext` 持有 `context.Context`**:

```go
type StageContext struct {
    Run    RunIdentity
    Self   WorkflowStage
    Prev   map[string]StageOutput   // 历史阶段输出(只读)
    Refs   RefMap
    Ctx    context.Context           // ← 取消入口(§14.4.1)
    Emit   func(StreamEvent)
    Store  func(key string, val any) error
}
```

#### 14.4.1 `context.Context` 是取消的 Go-native 答案

Go 的 `context.Context` 是为分布式取消设计的,完美映射 §14 协议:

- **三路合流**:把 `userCtx`(用户 cancel)、`hubCtx`(Hub cancel)、`stageCtx`(workflow gate/timeout/loop-maxIter)用 `context.WithCancel(parentCtx)` 派生——任一 cancel 触发,派生 ctx 自动 Done
- **取消传播**:Stage 函数签名 `func(ctx context.Context, s StageContext) (Result, error)` 中 `ctx.Done()` 就是 §14 的 `signal`
- **超时**:`context.WithTimeout(ctx, 30*time.Second)` 直接实现 `timeoutMs`
- **跨 host**:Hub 给远端发 cancel 消息后,**也调用对端 daemon 的 `ctx.Cancel()`**,语义一致

```go
// workflow engine 里 composite cancel context
parentCtx := mergeContexts(userCtx, hubCtx)   // 任一 Done → parent Done
stageCtx, cancel := context.WithTimeout(parentCtx, time.Duration(opts.TimeoutMs)*time.Millisecond)
defer cancel()

// stage function 应这样写:
func myStage(ctx context.Context, s StageContext) (Result, error) {
    for {
        select {
        case <-ctx.Done():
            return Result{}, ctx.Err()   // cancellation/timeout/deadline 都走这里
        case ev := <-someChannel:
            // 处理
        }
    }
}
```

> **收益**:`StageContext` 不需要单独的 `signal: AbortSignal` 字段——Go 里 `ctx` 就是 signal。所有 §14 协议字段(`reason / timeoutMs / force`)只作为**元数据**传给 cancel handler,实际机制是 ctx propagation。

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

```go
type CancelPolicy interface{ apply(...) }   // 或直接 type + switch

// fail-fast:取消兄弟 stage(parallel 默认)
type CancelPolicyFailFast struct{}
// continue-siblings:兄弟继续跑(sequence 默认)
type CancelPolicyContinueSiblings struct{}
// drain:给兄弟 timeoutMs 收尾,然后取消
type CancelPolicyDrain struct{ TimeoutMs int }
```

Worker 编排层用 `errgroup.WithContext` 实现 fail-fast:
```go
g, gCtx := errgroup.WithContext(stageCtx)
for _, branch := range parallelBranches {
    branch := branch
    g.Go(func() error {
        return runBranch(gCtx, branch)  // 任一 branch err,gCtx.Done()
    })
}
return g.Wait()   // 等所有 branch,gCtx 取消时正在跑的也尽快返回
```

### 14.7 幂等性

Stage 需要 `idempotencyKey`(Go 风格):
```go
// "runID:stageName:attempt"  → sha256 截前 16 hex
func IdempotencyKey(runID, stageName string, attempt int) string {
    h := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", runID, stageName, attempt)))
    return hex.EncodeToString(h[:8])
}
```
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

2. **Phase 1 不做跨 host session 迁移(§10.4) — v0.5 重要修正** —— 用户问题"无缝迁移是不是伪需求"刺中要害。诚实评估:99% 场景下 checkpoint + restart / `claude --resume` 够用,真正的 session migration 是为罕见极端场景(host 宕机、长跑 SLA)设计的复杂机制。Phase 1 砍掉 Hub + SessionMigrator,代码量少 40%,99% 用户体验不变。设计本身(v0.4 §10.4 / §10.5)保留,Phase 4+ 复用。

3. **跨 provider 子 agent 不做** —— §6.1 已硬约束:agent 是 peer,不做 Conductor-owned subagent。需要"Codex 调 Claude"协作时,只能走 Worker 编排层(让 Claude 作为独立 stage 跑),不能伪装成 provider 子 agent——这会损失一些"由 provider 维护的多轮上下文"语义,但换来清晰的边界。

4. **Provider-native subagent 的可见性差异** —— Claude Task 工具和 OMP task 工具产生的子 agent,我们通过 store 跟踪;但子 agent 的真实事件流受限于 provider SDK。Codex app-server 的子 agent 事件归一化成本可能不低。

5. **Context 长上下文累积** —— §12.10 已给出 4 层策略分层(Ref / 声明式 reads / agent retrieve / provider 摘要),但**实际效果依赖 provider 摘要质量**(Layer 3 兜底层)和 agent 协作智能( Layer 2)。Phase 2 必须有 e2e 测试覆盖"50+ stage workflow 不爆 context"。

6. **多 workspace / 多 repo** —— 单 host 单 cwd 是 Phase 1 默认行为。如果用户要把 Conductor 跑在 monorepo 上同时编排多个 package,需要 workspace 隔离(类似 Paseo `workspace-labels`),**当前未设计**。

7. **Quota / Auth 抽象** —— Paseo 有 `services/quota-fetcher`,我们 Phase 1 不做。意味着用户得自己在 provider 配置里管 API key。

8. ~~**Hub 的可靠性是单点**~~ —— **v0.6 彻底撤销**(Hub 现在是 Phase 2+ dispatcher,且不做迁移;可靠性要求大幅降低)。

9. **测试与并发模型** —— §14 已给出 cancellation 协议。Phase 1 无跨 host,验证范围缩小;但 §8 parallel stage 仍要死锁检测 + `go test -race`(Go 优势:`-race` 内置)。Phase 2+ Hub dispatcher 的"host 调度一致性"需要 e2e 测试。

10. **Provider 版本兼容** —— Claude Code SDK、Codex app-server 都在快速迭代。Provider 实现必须把"哪些字段是稳定的、哪些会变"显式标注,**当前未约定 deprecation 策略**。

11. **不与 LLM 直接耦合** —— 这是 Conductor 的定位选择,但也意味着"用一个 LLM 当 planner 来动态生成图节点"的能力被限定在 Worker 层之上。如果未来需要"LLM 在线重规划图结构",引擎层必须支持热替换 NodeRef,**当前未实现**。

12. **过度工程风险(自我警告 — v0.5 新增)** —— "Player registry" / "Multi-host Hub" / "Seamless migration" 这些概念**容易被术语诱惑**(Multica 是团队协作平台所以需要这些,但 Conductor 是 dev 工具,用户场景不同)。v0.5 已自我修正砍掉 Hub/Migration,但 Phase 2+ 设计时仍要警惕:**新术语新抽象不要堆砌,先问"99% 用户场景真的需要吗?"**

13. **Pi 依赖风险(v0.11 新增 — 关键)** —— Conductor v0.11 完全依赖 Pi(`@earendil-works/pi-coding-agent`)。Pi 是 Mario Zechner 个人项目(同一人做了 OpenCode),不是 OpenAI/Anthropic 那种基础设施级项目。**风险**:维护者精力转移 / 项目被弃 / 突然 breaking change / 安全漏洞无人修。**对策**:
- 锁定 Pi 精确版本,跑回归测试防升级破坏
- 跟踪 Pi upstream,贡献修复
- 文档化 "Pi Pivot 退出计划"(如果 Pi 死了怎么办)
- §5.2 保留接口设计的"可替换性"痕迹(虽然只跑 Pi,但代码不与 Pi 类型强耦合太深)
- **诚实声明**:Conductor 项目寿命部分取决于 Pi 项目寿命

11. **过度工程风险(自我警告)** —— "Player registry" / "Multi-host Hub" / "Seamless migration" 这些概念**容易被术语诱惑**(Multica 是团队协作平台所以需要这些,但 Conductor 是 dev 工具,用户场景不同)。v0.5 已自我修正砍掉 Hub/Migration,但 Phase 2+ 设计时仍要警惕:**新术语新抽象不要堆砌,先问"99% 用户场景真的需要吗?"**

## 16. 已确认的关键决策(v0.12)

| 决策点 | 选定方案 | 章节 |
|---|---|---|
| Server 语言栈 | **Go**(单 binary `~30MB`;context.Context 原生支持 §14) | §0, §17 |
| WebUI 语言栈 | **TypeScript/Next.js 14** | §3 |
| Phase 1 形态 | **本地优先**(local-first),单 host daemon,Paseo 模型;无 Hub | §10 |
| Provider 策略 | **Codex only + OpenRouter 多模型**——OpenAI 维护的 app-server;`~/.codex/config.toml` 配 `[model_providers.openrouter]` 覆盖 100+ 模型 | §5 |
| Conductor 范围 | **只在 Codex 之上加价值**:workflow engine、Hub 调度、contextBus、HOME 隔离、persistence | §5.0 |
| Provider 配置入口 | **Conductor 动态生成**到 per-spec HOME,user 不手动配 | §5.2, §6.2.5 |
| **Spec 模型** | **Spec 是用户定义的复用单元**;HOME 与 spec 同生命周期;同 spec 多次 invoke 共享 HOME,session 可跨 invoke resume | §6.2.5 |
| HOME 粒度 | **Per-spec**(不是 per-run,不是 per-invocation) | §6.2.5 |
| 多 host task 模型 | **task 不跨机器**;Hub(Phase 2+)= dispatcher,分发不同 task 给不同 host | §10.2 |
| 跨 host session 迁移 | **彻底不做**(用户决策 v0.6) | §10.4 ~~删除~~ |
| "无缝"边界 | **彻底不做**(用户决策 v0.6) | §10.5 ~~删除~~ |
| 跨 provider 子 agent | **不做**,agent 间对等(peer) | §6.1 |
| Provider 内部 subagent | **不干预**,纯观测 | §6.1 |
| **Subprocess 环境隔离** | **Per-run 隔离 HOME + Auth symlink**;Phase 1 必做,防 session 污染与配置冲突 | §6.2 |
| Workflow 引擎 | **A. 自研轻量软工作流引擎**(否掉 Temporal/Restate 重方案,否掉 prompt-only 退化) | §8.6 |
| Player registry | **多 host Hub**(进程内 + 跨 host 注册中心,Phase 2+) | §10.2 |
| 持久化默认 | **文件 JSON + SQLite 一等切换**(better-sqlite3,运行时可切) | §11.1 |
| Context 跨步骤 | **强类型 StageSpec + Ref offload + Zod 校验** | §12 |
| Protocol 共享 | **Zod 单一来源**(server + webui 都引用同 schema) | §17.2 D7 |
| 长上下文策略 | **4 层分层**:Ref / 声明式 reads / agent retrieve / provider 摘要 | §12.10 |
| Workflow 阶段 | **软工作流 + 语义标签**:PDCA(Plan→Do→Check→**Apply**)是默认预设,非强制 | §8 |
| 取消协议 | **三路合一**:Hub / 用户 / workflow 自身 → AbortSignal.any([...]) | §14 |
| Web UI | Phase 1 不出,Phase 3+ 可选 | §9.1 |

仍待确认的非阻塞项(实施时再定):
- Hub 的鉴权模型(token / mTLS / SSH-like)(Phase 2+ 才相关)
- Web UI 是 Phase 1 还是 Phase 3+
- Provider hook `onContextPressure` 的具体 contract
- 隔离 HOME 大小监控与自动 prune 策略

---

## 17. Go 实现的特定设计考量

> v0.4 新增。Server 切换为 Go 后,有几个原生收益与代价值得展开。

### 17.1 原生收益

| 维度 | Go 实现 | 若用 TS 实现 |
|---|---|---|
| 取消传播 | `context.Context` 一等公民,§14 直接映射 | AbortSignal + 手工协议 |
| 并发原语 | `errgroup` 一行实现 parallel stage + 取消传播 | Promise.allSettled + 手工 race condition 处理 |
| subprocess 管理 | `os/exec` + `cmd.Process.Kill()` + `defer cmd.Wait()`,**所有 provider 统一** | child_process,但 provider 接法不统一(SDK/ACP/app-server 三种) |
| 持久化 | `modernc.org/sqlite`(纯 Go,无 cgo)+ sqlc | better-sqlite3 + Kysely/Prisma |
| 部署 | 单 binary 静态链接(~30MB),无 runtime | Node binary + node_modules + 版本管理 |
| 内存/启动 | daemon 启动 < 100ms,常驻 50-80MB | Node 启动 200-500ms,常驻 100-150MB |
| 类型安全 | 编译期 + `go vet` + 静态分析 | TS 编译期 + ESLint,运行时仍可能逃逸 |
| Provider SDK | **无需依赖**——直连 CLI,统一 SubprocessClient(§17.7) | 依赖各 provider TS SDK(@anthropic-ai/...) |

### 17.2 关键 Go 设计决策

**D1. `context.Context` 是一等公民,贯穿所有 API**

§14 已多次强调。`AgentClient`、`AgentSession`、`Stage.Run`、`WorkflowSpec.planner` 全部接收 `ctx context.Context`。这消除了"单独 signal 字段"的复杂性。

**D2. Subprocess 生命周期 = `*exec.Cmd` 包装**

```go
type SubprocessClient struct {
    cmd     *exec.Cmd
    cancel  context.CancelFunc
    stdin   io.WriteCloser
    stdout  io.ReadCloser
    events  chan AgentStreamEvent
}

func NewSubprocessClient(ctx context.Context, name string, args []string) (*SubprocessClient, error) {
    cmd := exec.CommandContext(ctx, name, args...)
    stdin, _ := cmd.StdinPipe()
    stdout, _ := cmd.StdoutPipe()
    if err := cmd.Start(); err != nil { return nil, err }

    cli := &SubprocessClient{
        cmd:    cmd,
        stdin:  stdin,
        stdout: stdout,
        events: make(chan AgentStreamEvent, 64),
    }
    go cli.pumpEvents(stdout)   // JSON-RPC / NDJSON → events channel
    return cli, nil
}

func (c *SubprocessClient) Close(ctx context.Context) error {
    close(c.events)
    if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
        c.cmd.Process.Kill()
    }
    return c.cmd.Wait()
}
```

> **关键**:`exec.CommandContext(ctx, ...)` 自动绑 ctx——ctx cancel 时 subprocess 自动 SIGKILL。这是 Paseo `providers.md` 警告"不要把 spawned process 留在 readiness promise 里"的 Go-native 解。

**D3. 进程所有权 = 谁 `Start` 谁 `Wait`/`Kill`**

每次 `cmd.Start()` 必须配 `defer cmd.Wait()` + cancel handler。Runner 在 cleanup 路径保证 subprocess 不留孤儿。

**D4. WS 用 `nhooyr.io/websocket`(现 `github.com/coder/websocket`)**

- 比 `gorilla/websocket` API 更现代
- 内置 `context.Context` 支持(`ws.Read(ctx)`)
- 跨平台一致(`runtime` 抽象好)
- 单文件,零依赖

**D5. Schema 验证用 struct tags + `go-playground/validator/v10`**

```go
type AgentSpec struct {
    Provider  string `json:"provider" validate:"required,oneof=claude codex pi omp"`
    Model     string `json:"model"    validate:"omitempty"`
    Cwd       string `json:"cwd"      validate:"required,dir"`
    Skills    []string `json:"skills" validate:"dive,required"`
    // ...
}

func (s *AgentSpec) Validate() error {
    return validator.New().Struct(s)
}
```

> Zod 风格的运行时校验在 Go 里就是 `validator/v10`。Phase 1 够用,Phase 2 可升级到基于 JSON Schema 的 codegen。

**D6. CLI 单 binary + 子命令**

```go
// cmd/conductor/main.go
func main() {
    if len(os.Args) < 2 {
        runDaemon(os.Args[1:])
        return
    }
    switch os.Args[1] {
    case "daemon":   runDaemon(os.Args[2:])
    case "hub":      runHub(os.Args[2:])
    case "run":      cliRun(os.Args[2:])
    case "ls":       cliLs(os.Args[2:])
    case "logs":     cliLogs(os.Args[2:])
    case "cancel":   cliCancel(os.Args[2:])
    case "workflow": cliWorkflow(os.Args[2:])
    default:         runDaemon(os.Args[1:])   // backward-compat
    }
}
```

`conductor` 不带参数 = 默认启动 daemon(便于开机自启)。

**D7. 协议共享:JSON Schema source of truth + 双向 codegen**

```bash
# shared/protocol/*.schema.json → 单一来源
shared/protocol/agent-spec.schema.json     # AgentSpec shape
shared/protocol/events.schema.json         # AgentStreamEvent enum
shared/protocol/workflow.schema.json       # WorkflowSpec shape

# Go 端(从 schema 生成或 hand-write)
#   server/internal/protocol/types.go     # struct + json tags + validate tags
#   server/internal/protocol/schema.go   # 导出 schema(可选)

# TS 端(从 schema 生成)
#   webui/packages/conductor-protocol/index.ts   # 生成的 types
#   webui/packages/conductor-protocol/runtime.ts # Zod-style 校验(可选)
```

Phase 1 简化:**hand-written Go structs + hand-written Zod schemas,CI 断言 wire JSON 序列化等价**。Phase 2 引入 codegen。

**D8. Hub ↔ Player 长连接 = gorilla/websocket 或 coder/websocket**

```go
// hub-side
conn, _, err := websocket.Dial(ctx, player.URL, nil)
defer conn.Close(websocket.StatusNormalClosure, "bye")

// 每 5s 发心跳
ticker := time.NewTicker(5 * time.Second)
for {
    select {
    case <-ctx.Done(): return
    case <-ticker.C:
        conn.Write(ctx, websocket.MessageText, []byte(`{"type":"heartbeat"}`))
    }
}
```

### 17.3 关键 Go 库选型

| 用途 | 推荐 | 备选 |
|---|---|---|
| HTTP 路由 | `net/http` Go 1.22+ ServeMux | `go-chi/chi` |
| WebSocket | `github.com/coder/websocket` | `gorilla/websocket` |
| SQLite | `modernc.org/sqlite`(纯 Go) | `mattn/go-sqlite3`(cgo) |
| SQL codegen | `sqlc` | 手写 sqlx |
| 并发 | `golang.org/x/sync/errgroup` | 手写 WaitGroup |
| 校验 | `go-playground/validator/v10` | ozzo-validation |
| 日志 | `log/slog`(stdlib,Go 1.21+) | `uber-go/zap` |
| Config | `spf13/viper` 或手写 env | koanf |
| CLI flags | `spf13/cobra` | stdlib `flag` |
| Testing | stdlib `testing` + `github.com/stretchr/testify` | gocheck |
| Mocks | `uber-go/mock`(gomock 后续) | 手写 stub |
| JSON Schema export | `invopop/jsonschema` | 手维护 |

### 17.4 部署与发行

- **单 binary 静态链接**:`CGO_ENABLED=0 go build -ldflags="-s -w" -o conductor`
- **跨平台**:linux/amd64、linux/arm64、darwin/arm64(Apple Silicon)、windows/amd64
- **homebrew tap**(可选):`brew install conductor/tap/conductor`
- **systemd / launchd unit**(可选):`conductor daemon` 开机自启
- **Docker**(可选):scratch 镜像,binary 拷贝进去,~30MB

### 17.5 测试策略(Go 特定)

- **单元测试**:`go test ./internal/...` + testify
- **Provider subprocess mock**:`mock_load_test_agent`(Paseo 同款,Go 版)
- **真实 provider e2e**:`_test.go` 文件名带 `.real.e2e.`(Paseo 约定,只在 CI 跑需要真 binary 的测试)
- **Hub e2e**:多 process 模拟多 host,用 `t.Helper()` + `t.TempDir()` 隔离
- **Race 检测**:`go test -race ./...` 是 CI 必跑
- **Coverage**:`go test -coverprofile=cover.out`,门槛 80%+

### 17.6 webui ↔ server 协议边界

Server 暴露 OpenAPI 3.1 规范;webui 用 `openapi-typescript` codegen:

```
server/internal/gateway/openapi.yaml     # 生成的 OpenAPI 规范
webui/packages/api-client/               # openapi-typescript 产物
   src/types.ts                          # 全自动生成
   src/client.ts                         # 手写 fetch wrapper
```

WS 消息 schema 单独维护:`shared/protocol/events.schema.json`。

### 17.7 Provider SDK 不依赖性(Go 后端关键澄清)

> v0.4 追加。用户问题:"如果 server 是 Go,是不是 Claude Code 无法用 Paseo 那种 SDK 方式?"

**答案**:是的,但这不是损失,是简化。

#### 17.7.1 Claude Agent SDK 是 TS-only

Paseo 的 `packages/server/src/server/agent/providers/claude/agent.ts` 调 `@anthropic-ai/claude-agent-sdk`(TypeScript SDK)。SDK 不能从 Go 直接调。**Anthropic 没有官方 Go SDK**。

但 SDK 的本质是**薄包装**:
1. `spawn` `claude` CLI 子进程
2. 写 prompt 到 stdin 或 `-p` 参数
3. 解析 stdout 的 `--output-format stream-json`(NDJSON 事件流)
4. 管理 `--session-id` / `--resume` 持久化
5. 转发 MCP 配置(`--mcp-config`)
6. 工具权限(`--allowedTools` 等)

**真正的能力在 CLI 里**。SDK 只是把这些暴露成 Promise API + typed events。

#### 17.7.2 Go 直接走 CLI,所有 provider 统一"subprocess + 协议解析"

| Provider | Go 接入方式 | 协议层 |
|---|---|---|
| **Claude Code** | `exec.Command("claude", ["-p", ..., "--output-format", "stream-json"])` | NDJSON(每行一 JSON event) |
| **Codex** | `exec.Command("codex", ["app-server"])` | JSON-RPC over stdio |
| **Pi / OMP / ACP** | `exec.Command("pi", [...])` | JSON-RPC over stdio(Agent Client Protocol) |
| **自定义 HTTP agent** | `net/http` client | HTTP/JSON |

**意外但关键的收益**:Conductor 在 Go 里**所有 provider 是同一种模式**——`SubprocessClient`(§17.2 D2)+ 协议解析层。Paseo 因为是 TS,**三种 provider 接法不一样**(SDK / ACP / app-server);Conductor 反而**代码更统一**。

#### 17.7.3 真实代价(可控)

SDK 提供的便利里,以下需要 Conductor 自己实现,都是几百行级:

| SDK 能力 | Go 替代实现 | 工作量 |
|---|---|---|
| Session hooks(PreToolUse / PostToolUse) | CLI 通过 stream-json 暴露,Go 解析 event 类型 | 小 |
| 类型化事件 | Go struct + `json.Unmarshal` + `validate:"..."` tag | 小 |
| CLI 版本检测 | 自己跑 `claude --version` 比对 | 微 |
| MCP 配置加载 | JSON 解析,$CONDUCTOR_HOME/mcp.json | 小 |
| 权限回调 | CLI 把权限请求当 event emit,Conductor 订阅 channel 转给 Workflow 层 | 小 |

这些**本来就属于 Conductor 想控的边界**(§6.1 已硬约束"不干预 provider 内部")。

#### 17.7.4 SubprocessClient 的统一接口

```go
// internal/provider/base/subprocess.go —— 所有 provider 共享
type SubprocessClient struct {
    cmd    *exec.Cmd
    stdin  io.WriteCloser
    stdout io.ReadCloser
    stderr io.ReadCloser
    events chan AgentStreamEvent
    parser ProtocolParser  // 协议解析器:NDJSON / JSON-RPC / ...
}

func (c *SubprocessClient) Send(ctx context.Context, prompt AgentPrompt) error {
    return c.parser.WriteRequest(c.stdin, prompt)  // 不同 provider 不同实现
}

func (c *SubprocessClient) Events() <-chan AgentStreamEvent { return c.events }

func (c *SubprocessClient) Cancel(ctx context.Context) error {
    // §14: SIGTERM → grace → SIGKILL
    _ = c.cmd.Process.Signal(syscall.SIGTERM)
    select {
    case <-time.After(30 * time.Second):
        _ = c.cmd.Process.Kill()
    case <-c.cmdDone():
    }
    return nil
}

func (c *SubprocessClient) Close(ctx context.Context) error {
    close(c.events)
    return c.cmd.Wait()
}

type ProtocolParser interface {
    WriteRequest(w io.Writer, req any) error
    ReadEvent(r io.Reader, ch chan<- AgentStreamEvent) error  // goroutine 里跑
}
```

#### 17.7.5 三个具体 provider 实现要点

**Claude Code**(NDJSON parser):
```go
// Claude parser: stream-json → AgentStreamEvent
// CLI flags: -p "<prompt>" --output-format stream-json --session-id <id> --resume <id>
type ClaudeParser struct{}

func (p *ClaudeParser) ReadEvent(r io.Reader, ch chan<- AgentStreamEvent) error {
    return jsonl.NewDecoder(r).Decode(func(ev ClaudeEvent) {
        switch ev.Type {
        case "assistant":    ch <- AgentStreamEvent{Kind: "text", Text: ev.Message.Content}
        case "tool_use":     ch <- AgentStreamEvent{Kind: "tool_call", ...}
        case "tool_result":  ch <- AgentStreamEvent{Kind: "tool_result", ...}
        case "permission_request":
            ch <- AgentStreamEvent{Kind: "permission_request", ...}
        case "result":       ch <- AgentStreamEvent{Kind: "finish", ...}
        }
    })
}
```

**Codex**(JSON-RPC parser):
```go
// Codex app-server: JSON-RPC 2.0 over stdio
// CLI flags: app-server
type CodexParser struct{}

func (p *CodexParser) ReadEvent(r io.Reader, ch chan<- AgentStreamEvent) error {
    return jsonrpc.NewStream(r).Read(func(msg jsonrpc.Message) {
        // map thread.started / turn.started / item.completed / ...
    })
}
```

**ACP**(JSON-RPC + ACP spec):
```go
// 任意 ACP-compatible agent
type ACPParser struct{ /* 与 Codex 同,但方法名是 ACP 规范的 */ }
```

#### 17.7.6 与 Paseo 对比

| 维度 | Paseo (TS) | Conductor (Go) |
|---|---|---|
| Claude 接入 | TS SDK(封装 CLI) | 直接调 CLI(stream-json) |
| Codex 接入 | TS SDK 调 app-server | JSON-RPC over stdio |
| ACP 接入 | ACP base class | ACP base class(同构) |
| Provider 接法数量 | **3 种**(SDK/ACP/app-server) | **1 种**(subprocess + parser) |
| 运行时依赖 | Node + provider CLI binaries | 仅 provider CLI binaries |
| Session resume | SDK 内置 | CLI `--resume <id>` + 自己管 handle |
| Hooks / 权限 | SDK callback | stream-json event + Go channel |
| 版本兼容 | SDK 跟随 CLI | 直跟 CLI,需自己 `--version` 检测 |

> **结论**:Go 后端不"丢失" Claude SDK,而是**绕开 SDK 直连 CLI**——SDK 的价值在 Go 里被 §17.7.4 的 `SubprocessClient` + `ProtocolParser` 替代,且**统一性更高**。

**关于 handoff**:Paseo 的 handoff(见 `skills/paseo-handoff/SKILL.md`)是 prompt 模板 + `create_agent` 工具调用,**不是 SDK 协议能力**。CLI 模式完全支持——详见 §12.11 Handoff & Session Transfer。

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

### v0.11 → v0.12(本次更新 — Codex + OpenRouter,放弃 Pi 深度集成)

**用户决策**:**Codex only + OpenRouter 多模型配置**。Pi deep integration 路线放弃,原因是:
- Pi 稳定性风险 vs 收益不划算
- Codex 原生支持 OpenAI-compatible 协议(Mistral / Ollama / OpenRouter / 自建代理)
- OpenRouter 一次性覆盖 Claude / Gemini / Llama / Mistral 等 100+ 模型
- Codex = OpenAI 维护,稳定性高

**Codex 多模型能力核实**(OpenAI 官方文档):
- 原生支持 OpenAI(gpt-5 / gpt-4 / o3)
- 内置 Amazon Bedrock provider
- 原生支持任意 OpenAI-compatible API,via `~/.codex/config.toml` 的 `[model_providers.<id>]` 块
- 例:OpenRouter / Mistral / Ollama / LM Studio / 自建 proxy

**§5 整章重写**:
- §5.0 集成策略 — Codex 全套机制复用(MCP / approval / compaction / 多模型 / session)
- §5.1 Codex app-server JSON-RPC 客户端(~150 行)
- §5.2 Codex provider 配置示例(`~/.codex/config.toml`)
- §5.3 v0.11 Pi vs v0.12 Codex 对比表

**关键让步**:多模型配置从"Conductor 层"移到"Codex config.toml 层"。用户改自己的 `~/.codex/config.toml`,Conductor 不参与。

**§13 Phase 1**:
- 集成代码量:`~30 行` → `~150 行`(Codex JSON-RPC 客户端比 Pi SDK 包装代码多)
- 时间估算:`~2 周` → `~2-3 周`
- Pi 集成代码删除,Codex 集成代码新增

**§16 决策表更新**:
- Provider 策略:Pi deep integration → Codex + OpenRouter
- Conductor 范围:Pi 之上 → Codex 之上
- 新增决策:Provider 配置入口 = `~/.codex/config.toml`(用户标准 Codex 配置)

**§15 弱点更新**:
- 第 13 条 Pi 风险**删除**(不再用 Pi)
- 新增第 13 条 OpenRouter 风险(代理稳定性)
- 新增第 14 条 Codex config.toml 维护责任(版本兼容)

### v0.10 → v0.11(本次更新 — Pi 深度集成,完全去掉 Claude)

**用户决策**:**完全去掉 Claude 支持,Pi 深度集成**。这不只是"不写 Claude provider",而是**直接复用 Pi 的所有概念**(类型、skill、session、MCP、approval、cancellation、context files)。

**架构影响**(对比 v0.10):
| 维度 | v0.10 (Pi 包装) | v0.11 (Pi 深度集成) |
|---|---|---|
| Provider 抽象 | 通用 `AgentClient/AgentSession` | **删除,直接用 Pi SDK 类型** |
| Skill 系统 | Conductor 自己设计 | **直接复用 Pi skill 目录约定** |
| Session 格式 | Conductor state.json + Pi JSONL | **Pi JSONL 作为 canonical** |
| MCP 配置 | Conductor 自己管 | **直接用 Pi `--mcp-config`** |
| 取消传播 | Conductor 自己包 | **直接调 Pi `cancel()`** |
| 集成代码 | ~50 行 | **~30 行(直接 re-export Pi 类型)** |

**§5 整章改写为"Pi 集成层"**:
- §5.0 集成策略(Conductor 不 Re-invent Pi,而是直接消费)
- §5.1 Pi SDK 集成(直接 import Pi SDK,删除通用接口)
- §5.2 Pi 概念映射表(Conductor 概念 → Pi 等价物)
  - AgentSpec → Pi `--provider`/`--model`/`--skills`/`--mcp-config`
  - Session → Pi session JSONL(直接读)
  - Compaction → Pi `/compact` slash command
  - Approval → Pi approval model(直接 wrap)

**§4 Agent Spec 修订**:形状与 Pi CLI flags 一一对应

**§13 Phase 1 集成代码量**:`~50 行` → `~30 行`(直接 re-export Pi 类型,几乎无包装)

**§16 决策表更新**:
- "Phase 1 provider 策略" → "Provider 策略: Pi 深度集成"
- 新增决策 "Conductor 范围: 只在 Pi 之上加价值 (workflow / Hub / contextBus / HOME / persistence)"

**§15 自我审视新增第 13 条 — Pi 依赖风险**:
- Pi 是 Mario Zechner 个人项目,非基础设施级
- 对策:锁版本 + 回归测试 + 跟踪 upstream + Pivot 退出计划
- 诚实声明:Conductor 项目寿命部分取决于 Pi

**新风险与对策**(完整版见 §15.13):
- Pi 维护者精力转移 / 项目被弃
- Breaking change
- 安全漏洞无人修
- Phase 1 锁定精确 Pi 版本:`@earendil-works/pi-coding-agent@X.Y.Z`

### v0.9 → v0.10(本次更新 — 彻底简化:Pi only,无需 Claude SDK escape hatch)

**用户问题**:"用 Pi 的话,先忽略 Claude Code,是不是可以直接 API 方式使用 Codex 和其他大量模型?"
**答案**:**是**——Pi 本身就是完整 coding agent(类 Claude Code/Codex),已支持 20+ 模型,无需任何 escape hatch。

**Pi 不只是 LLM wrapper**:
- 内置 read/write/edit/bash 工具
- session 管理(branching、compaction)
- skill / extension / package 体系
- MCP 支持
- 多模型(20+ provider)
- RPC + SDK 双重集成方式

也就是说,**Pi 完全能替代 Claude Code / Codex / OpenCode 的角色**,Conductor 不需要为这些单独写 provider 适配。

**Phase 1 简化效果**:

| 维度 | v0.9 (Pi + Claude SDK) | v0.10 (Pi only) |
|---|---|---|
| Phase 1 provider 数 | 2 | **1** |
| 集成代码量 | ~300 行 | **~50 行** |
| 多模型 day-1 | ✅ Pi 覆盖 | ✅ Pi 覆盖 |
| Claude 直连 escape hatch | ✅ Phase 2 | ❌ Phase 3+ 视需求 |
| 维护负担 | 2 个 SDK | **1 个** |

**§5.1 注释更新**:v0.10 关键变更说明 Pi 不是 LLM wrapper 而是完整 coding agent,无需 Claude SDK escape hatch

**§13 Phase 1 路线图更新**:
- 集成代码量:~300 行 → **~50 行**(纯 Pi SDK 包装)
- 时间估算:~2 周保持
- Phase 2:不再列"加 Claude SDK"
- Phase 3+:仅在 Pi 真不够时考虑 escape hatch

**§16 决策表更新**:
- Phase 1 provider:**Pi only**(从"Pi-first + escape hatch"→ "Pi only")
- Phase 2+ provider:**不实现**(从"加 escape hatch"→ "按需")

### v0.8 → v0.9(本次更新 — Server 改 Node.js,Provider 改 Pi-first)

**用户决策**(双 pivot):
- ✅ Server:**TypeScript/Node.js**(原 Go)
- ✅ Provider 策略:**Pi-first**(1 个 provider = 20+ 模型),Claude SDK 作 escape hatch

**为何 v0.9 选 TS**:
- 直接用 `@earendil-works/pi-coding-agent` SDK(避免手写 JSON-RPC)
- 直接用 `@anthropic-ai/claude-agent-sdk`(escape hatch)
- 直接 fork Paseo TS 代码(同语言,最大加速点)
- npm 生态丰富(`ws`, `zod`, `fastify`, `@modelcontextprotocol/sdk`)
- 取消原语 `AbortController` / `AbortSignal` 完美映射 §14 协议

**Pi-first 影响**:
- Phase 1 从"单 provider (Claude only)"→ "Pi only" = 20+ 模型 day-1
- Phase 1 时间估算从 6-8 周 → **~2 周**(SDK 包装比 Go 手写 RPC 快 3 倍)
- Phase 2+ 仍可加 direct Claude / Codex(escape hatch)

**§3 仓库布局重写**:
- 旧: `server/`(Go) + `webui/`(JS) + `shared/`
- 新: `packages/`(TS monorepo)+ `apps/{daemon,web}`(TS)
- 单一语言,`pnpm-workspace.yaml` 一个 root

**§5.1 AgentClient/AgentSession 改 TS 版**:
- 取消从 `context.Context` → `AbortSignal?(可选参数)`
- 流式从 `Events() <-chan` → `events(): AsyncIterable`
- §14 三路合一:`AbortSignal.any([userSignal, hubSignal, workflowSignal])`

**§13 路线图更新**:
- Phase 1: "Go + Claude only (6-8 周)" → "TS + Pi SDK (~2 周)"
- Phase 2: 加 Claude SDK 作 escape hatch

**§16 决策表更新**:
- Server 语言栈:Go → TypeScript/Node.js
- Phase 1 provider 策略:Claude only → Pi-first
- Phase 2+ provider:Claude SDK(escape hatch)+ 其他

### v0.7 → v0.8(本次更新 — 共享 .auth/ 目录优化)

**用户问题**:"为啥每 run 一个 HOME dir,不能 claude code 全部共享一个吗?"
**诚实回答**:
- 完全共享一个 HOME 在技术上可行,但**会丢失 5 件事**:per-run 状态隔离、简单清理、调试隔离、并发 run 不打架、settings 不冲突——所以**不是优化,是反模式**。
- 真正的优化是**共享 auth 目录**(`.auth/`)+ per-run session/logs 目录。v0.8 引入 `$CONDUCTOR_HOME/.auth/` 作为 auth 单一来源。

**§6.2.2 设计修订**:
- `NewIsolatedHome` 中 auth symlink target 从 `~/.claude.json` 改为 `$CONDUCTOR_HOME/.auth/.claude.json`
- `.auth/` 里的文件才是指向用户 HOME 的 symlink(单层,所有 run 共享)

**新增 §6.2.8 共享 .auth/ 优化**:
- 完整目录结构对比图
- 4 个优势(单一来源 / 重置简单 / 可切断关联 / 易备份)
- 磁盘影响分析(实际差异微乎其微,价值是清晰管理边界)
- 为什么 session/logs 仍 per-run

**新增 `EnsureAuthDir()` + CLI 命令**:
- `conductor init` → 首次建 `.auth/`
- `conductor auth reset` → 清 `.auth/` 重建
- (Phase 2+) `conductor auth copy-from-user` → symlink 换 copy

**§6.2.3 决策表更新**:决策 "Symlink auth 到 .auth/" 替代 "Symlink auth 到用户 HOME"

**§6.2.7 e2e 测试新增**:T_auth_reset(`.auth/` 被删后下次 run 重建)

**§16 决策表**:维持"Subprocess 环境隔离"决策(§6.2 引用更新)

### v0.6 → v0.7(本次更新 — Subprocess HOME 隔离,Phase 1 必做)

**用户问题**:"Claude Code 和 Codex 用 HOME,Conductor 是不是应该隔离防止干扰用户?"
**答案**:**必须隔离**——per-run 隔离 HOME + auth 文件 symlink。

**新增 §6.2 Subprocess Environment Isolation**(7 小节,164 行):
- §6.2.1 不隔离的 4 类冲突(session 污染 / 配置冲突 / 并发写 / 审计混乱)
- §6.2.2 Per-Run 隔离 HOME + Auth Symlink 设计 + Go 代码
- §6.2.3 关键设计决策表(per-run / symlink / 0700 等)
- §6.2.4 边界与副作用(6 条)
- §6.2.5 集成到 §17.2 D2 SubprocessClient
- §6.2.6 与 §6.1 的关系(行为约束 vs 环境约束,正交)
- §6.2.7 e2e 测试矩阵(6 个测试 ID)

**§16 决策表新增 1 行**:Subprocess 环境隔离 = Per-run 隔离 HOME + Auth symlink,Phase 1 必做

**§16 待确认项调整**:
- 删除 "Provider 之间 auth/credential 隔离"(v0.7 已用 §6.2 解决)
- 新增 "隔离 HOME 大小监控与自动 prune 策略"

**集成点**:
- §17.2 D2 SubprocessClient 增加 `home *IsolatedHome` 字段
- §6.2.5 给出完整改造代码

### v0.5 → v0.6(本次更新 — 彻底删除 session migration,Hub 改为 dispatcher)

**用户决策**(核心):
- ❌ SessionMigrator(原 §10.4)**彻底不做**——不进入设计
- ❌ "无缝"边界(原 §10.5)**彻底不做**——不进入设计
- ✅ 多 host task 模型:**task 不跨机器**;Hub(Phase 2+)= **dispatcher** 分发不同 task 给不同 host
- ✅ Host 死 mid-task → task 死,用户重试(K8s Pod 模型)

**§10 重大清理**:
- §10.2 重写:Hub 新语义为 `DispatchTask(ctx, req) → HostID`,不是 `MigrateWorkflow`
- §10.4(原 SessionMigrator)**整段删除**
- §10.5(原 无缝边界)**整段删除**
- §10.3(原 worktree)保留作为最后小节

**新增 §12.0 contextBus 边界(Per-Task, Per-Host)**(5 小节):
- §12.0.1 物理边界(Host A / Host B 各自独立的 WorkflowState)
- §12.0.2 Ref 系统本机化(file/worktree/session/blob 全在本机)
- §12.0.3 跨 Task 数据共享 3 方案(Hub blob / Task chaining / 不支持)
- §12.0.4 Checkpoint / Resume 语义(同 host 内,跨 host 拒绝)
- §12.0.5 Provider Session 也 Per-Host(SessionRef 含 HostID)

**§13 路线图调整**:
- Phase 1:删 "Hub 跨 host 路由 + session 迁移" 行,加 "contextBus per-task per-host (§12.0)"
- Phase 2:加 "Hub as dispatcher(§10.2 新语义) + Provider 扩展"
- Phase 4:删除 "Hub + SessionMigrator 复活" 行

**§16 决策表调整**:
- 删除 "Phase 4+ 跨 host 迁移" 行
- 新增 "多 host task 模型:task 不跨机器,Hub = dispatcher" 行
- §10.4 / §10.5 标记"~~删除~~"

**§15 弱点调整**:
- #8 "Hub 可靠性" 改为 ~~v0.6 彻底撤销~~
- #9 "测试与并发" 收窄为 Phase 1 范围 + 提示 Phase 2+ Hub 调度一致性

### v0.4 → v0.5(本次更新 — Phase 1 范围重新评估)

**用户问题**:"无缝迁移是不是伪需求?"
**诚实回答**:**部分是的**。Phase 1 做过度工程;99% 场景下 checkpoint + restart 够用。

**Phase 1 范围重大调整**:
- ❌ 砍掉:Hub 跨 host 路由(§10.2 后半段)
- ❌ 砍掉:SessionMigrator 跨 host 迁移(§10.4)
- ❌ 砍掉:"无缝"边界表(§10.5)
- ✅ 保留:SessionMigrator / SessionSnapshot / ClaudeSessionMigrator 设计骨架,Phase 4+ 复用
- ✅ 保留:本机 worktree 隔离(§10.3,Phase 1 仍需要)
- ✅ 新增:Phase 1 workflow checkpoint + 本地 resume
- ✅ 新增(可选):E2E relay 跨设备访问(参考 Paseo)

**§10 大调整**:§10.2 / §10.4 / §10.5 全部标 "Phase 4+";§10.3 改为"Phase 1 必做"

**§13 路线图**:Phase 1 砍 hub + 跨 host 迁移,加 checkpoint + 本地 resume;Phase 4 复活 Hub + SessionMigrator

**§16 决策表新增 1 行**:Phase 1 形态 = 本地优先(Paseo 模型),无 Hub

**§15 自我审视新增第 11 条 — 过度工程风险**:
> 警惕被术语诱惑,先问"99% 用户场景真的需要吗?"

### v0.3 → v0.4(本次更新)

**用户决策**:Server 语言栈切换
- ✅ Server: **Go**(原 TypeScript/Node)
- ✅ WebUI: **JS** (Next.js + React)
- ✅ CLI:Go 单 binary(`conductor daemon|hub|run|ls|cancel|workflow`)
- ✅ 协议共享:JSON Schema 单一来源

**新增 §17 Go 实现的特定设计考量**(6 小节):
- §17.1 原生收益对比表(context.Context / errgroup / os/exec / 单 binary)
- §17.2 关键 Go 设计决策(D1-D8):context 一等公民、SubprocessClient 包装、WS 选 coder/websocket、CLI 子命令、protocol codegen
- §17.3 关键 Go 库选型表(11 类)
- §17.4 部署与发行(CGO_ENABLED=0、跨平台、Docker scratch)
- §17.5 测试策略(real.e2e 命名约定 + race 检测)
- §17.6 webui ↔ server 协议边界(OpenAPI + openapi-typescript)

**重写或大改的章节**(从 TS 改 Go 语法 / 加入 Go 上下文):
- §0 一句话定位 + Go 收益/代价表
- §1 竞品表 Conductor 行(语言栈/持久化/取消原语)
- §3 仓库布局(Go server + JS webui 双 workspace)
- §5.1 Provider 接口 Go 版(AgentClient / AgentSession)
- §6 Agent Runner 引入 context.Context 取消语义
- §8 末尾 Go 实现要点(errgroup + for loop + 函数签名)
- §10 PlayerRegistry / PlayerHub Go 语法
- §11.1 Storage interface + 双实现(改 Go)
- §12.8 WorkflowState Go struct
- §14.4 StageContext 引入 ctx,§14.4.1 详述 context 取消语义
- §14.6 CancelPolicy + errgroup fail-fast 实现
- §14.7 IdempotencyKey Go 风格(sha256 截断)

**§16 决策表扩展**:新增 Server 语言栈、WebUI 语言栈、Protocol 共享 3 行

**下一轮(待用户输入)**:
- webui 框架终选(Next.js 14 App Router vs SvelteKit vs Remix)
- webui 何时启动(Phase 1 同步 vs Phase 2 末 vs Phase 3)
- CLI flag 库终选(cobra vs stdlib flag)
- OpenAPI 生成工具(swag vs ogen vs hand-written)

**v0.4 后续追加(第四轮 — 同 provider 迁移边界澄清)**:

**用户问题**:"task 使用同一种 provider 是不是就可以无缝迁移?"
**答案**:**是的——这是 §10.4 SessionMigrator 设计的 happy path**。但"无缝"有清晰的三档边界:
- **对话/配置层**:完全无缝(用户零感知)
- **运行层瞬态**(MCP / watcher):几十秒级重连
- **运行层有状态**(DB migration / 长连接):无法迁移,需 workflow 设计避坑

**新增 §10.5 同 Provider Task 的"无缝"边界**(10 小节):
- §10.5.1 Happy path 流程(同 provider 跨 host)
- §10.5.2 "无缝"三档定义表
- §10.5.3 完全无缝(7 类状态)
- §10.5.4 短暂重新初始化(4 类,MCP / watcher / dev server / permission)
- §10.5.5 无法迁移(3 类,stateful op / background / 长连接)
- §10.5.6 Hub 减少迁移触发频率策略
- §10.5.7 多 Provider Task 复杂度(Phase 2+ 才有)
- §10.5.8 e2e 测试矩阵(7 个测试 ID)
- §10.5.9 用户 FAQ(5 个常见问题)
- §10.5.10 与 §10.4 的关系(范围 vs 机制)

§10.5 是 §10.4 的"边界表",告诉读者**哪些场景完美工作、哪些有降级、哪些不行**。

**v0.4 后续追加(第三轮 — 关键架构决策)**:

**用户决策**:Phase 1 只支持单一 provider(Claude Code),把跨 host session 迁移做扎实

**关键洞察**:Phase 1 想跑通"host A 的 Claude session 迁到 host B 接着跑",**只支持 Claude 就够**。多 provider v1 会引入"跨 provider session 翻译"——这是 open research problem,SDK 也不解决。

**新增 §10.4 跨 Host Session 迁移(核心价值)**(10 小节):
- §10.4.1 单 provider vs 多 provider 迁移复杂度对比表
- §10.4.2 抽象先行:interface 第一天就设计,实现 v1 单 provider
- §10.4.3 迁移的 5 类数据(session state / working dir / cursor / refs / meta)
- §10.4.4 SessionMigrator interface(Go 代码)
- §10.4.5 Claude v1 实现要点(读 JSONL + git bundle)
- §10.4.6 Hub 端迁移触发流程(cancel → export → transfer → import → resume)
- §10.4.7 传输层选择(WS vs HTTP multipart + 压缩 + 签名)
- §10.4.8 失败与回滚表(4 个阶段)
- §10.4.9 v1 严格单 provider 的代价(诚实承认)
- §10.4.10 §15 自我审视更新(新设计债务)

§13 路线图修订:
- Phase 1:移除"Codex"provider,明确"Claude 单一实现";**强调 hub + session 迁移必做**
- Phase 3:从"5+ provider"改为"3+ provider,迁移能力按 provider 扩展"

§16 决策表扩展:新增 3 行(Phase 1 provider / 跨 host session 迁移 / Phase 2+ provider)

**下一轮(待用户输入)**:
- Claude session JSONL 格式版本兼容策略(`FormatVersion` 演进)
- worktree git bundle 包含哪些 refs(只 HEAD / 含所有 branch / 含 stash?)
- 大 payload 传输的安全边界(snap 签名 / 加密 / Hub 是否需要持久化)

**v0.4 后续追加(第二轮)**:

**用户问题回应(SDK + handoff)**:
1. Codex 用 `app-server` 因为 OpenAI 官方提供;Claude Code 没有等价物,只有 CLI——这是 provider 决定的,不是 Conductor 选择的
2. Paseo 的 handoff(读完 `skills/paseo-handoff/SKILL.md` 确认)是 **prompt 模板 + `create_agent` 工具调用**,**不是 SDK 协议能力**。CLI 模式完全支持
3. 跨 provider 状态深传(Claude 多轮 → Codex)是 **open problem**,SDK 也不解决

**新增 §12.11 Handoff & Session Transfer**(6 小节):
- §12.11.1 Paseo handoff 是 prompt 工程(源码确认)
- §12.11.2 三种 handoff 形态的可行性表
- §12.11.3 Conductor handoff 设计(Briefing struct + TransferRequest + 4 类实现)
- §12.11.4 与 Paseo 对比(Conductor 在跨 stage 自动 handoff + 同 provider session resume 有优势)
- §12.11.5 CLI 形态(3 种触发方式)
- §12.11.6 不做的事(诚实清单)

§17.7 末尾更新,显式说"handoff 不依赖 SDK,详见 §12.11"。

**v0.4 后续追加(第一轮)**:

**新增 §17.7 Provider SDK 不依赖性**(6 小节):
- §17.7.1 SDK 本质是薄包装的说明
- §17.7.2 Go 直接走 CLI,所有 provider 统一模式表
- §17.7.3 真实代价(可控,几百行级)与对策表
- §17.7.4 SubprocessClient 统一接口 Go 代码
- §17.7.5 三个具体 provider 实现要点(Claude NDJSON / Codex JSON-RPC / ACP JSON-RPC)
- §17.7.6 与 Paseo 对比表(Go 直连 CLI vs TS 三种接法)

§17.1 表新增一行:**Provider SDK 无需依赖**——直连 CLI 统一 SubprocessClient。

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
