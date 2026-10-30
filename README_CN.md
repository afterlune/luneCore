# Lune Core

Lune Core 是供 Lune 前端（TUI 终端、行式 CLI、Windows 桌面端及 A2A 服务端）使用的 Go 运行时核心库。宿主应用在自身进程内构建 `*core.Core`，通过公开 API 管理会话、队列轮次、跨轮次持久化目标（Goal）、权限审批、流式事件订阅、Token 预算追踪、MCP 扩展与工具调用。Lune Core 不运行外部后台守护进程，也不暴露任何 IPC 或网络监听服务。

## 安装

```sh
go get github.com/afterlune/luneCore@latest
```

## 快速上手

```go
package main

import (
	"context"
	"encoding/json"
	"log"

	core "github.com/afterlune/luneCore"
)

func main() {
	ctx := context.Background()
	// Core 默认从工作区根目录读取 config.json；也可通过 core.Config 传入编程式配置
	client, err := core.New(core.Config{Workspace: "."})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close(context.Background())

	session, found, err := client.LatestSession(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if !found {
		session, err = client.CreateSession(ctx, json.RawMessage("null"))
		if err != nil {
			log.Fatal(err)
		}
	}

	input, err := core.NewTaskInput("审查当前工作区代码。")
	if err != nil {
		log.Fatal(err)
	}
	_, err = client.RunTask(ctx, core.TaskRequest{SessionID: session.ID, Input: input})
	if err != nil {
		log.Fatal(err)
	}
}
```

工作区提供 `config.json` 来配置模型凭据、运行时参数及外部服务。模型提供商与 Profile 位于 `models`；执行默认参数位于 `runtime`；多模态附件限制位于 `attachments`；外部 MCP 服务位于 `mcpServers`：

```json
{
  "models": {
    "providers": {
      "deepseek": {
        "protocol": "responses",
        "baseURL": "https://api.deepseek.com",
        "apiKey": "your-api-key",
        "serverCompaction": true
      },
      "primary": {
        "protocol": "responses",
        "baseURL": "https://api.example.com",
        "apiKey": "your-api-key",
        "serverCompaction": true
      },
      "anthropic": {
        "protocol": "anthropic-messages",
        "baseURL": "https://api.anthropic.com",
        "apiKey": "your-api-key"
      }
    },
    "profiles": {
      "main": {
        "provider": "primary",
        "model": "model-id",
        "displayName": "主模型",
        "contextWindow": 1000000,
        "maxOutputTokens": 8192,
        "supportsInputAudio": false
      }
    },
    "defaultProfile": "main"
  },
  "runtime": {
    "contextWindow": 0,
    "maxParallelAgents": 4,
    "maxTeamChildrenPerAgent": 4,
    "maxTeamDepth": 2,
    "autoCompact": true,
    "autoCompactThreshold": 0.8,
    "reserveOutputTokens": 0
  },
  "attachments": {
    "maxFiles": 5,
    "maxFileBytes": 10485760,
    "maxTotalBytes": 20971520
  },
  "mcpServers": {
    "local-tools": {
      "transport": "stdio",
      "command": "tool-binary",
      "args": ["--mode", "server"],
      "env": {
        "SERVER_TOKEN": "secret-value"
      }
    },
    "remote-tools": {
      "transport": "streamable-http",
      "url": "https://mcp.example.com/api",
      "clientMetadataURL": "https://example.com/oauth/client.json"
    }
  }
}
```

此分组配置结构为强制规范。旧版平铺配置已被拒绝，详见附带的 `config.example.json`。`responses` 协议的提供商可配置 `"serverCompaction": true` 启用服务端会话上下文压缩。

## 存储与会话运行时

Core 在 `StateDir`（默认为工作区下 `.luneagent`）存储会话、队列轮次、安全持久化运行时事件日志、Graph 检查点及工具调用流水。团队分支共享所属运行的 Graph 检查点。当前格式使用 `executions-v2.db` 保存 Graph format 10 检查点。旧版状态目录必须先通过离线升级命令升级后才能打开（见[离线执行存储升级](#离线执行存储升级)）。

打开会话级运行时以获得异步控制能力：

```go
runtime, err := client.OpenSessionRuntime(ctx, session.ID)
if err != nil {
	log.Fatal(err)
}
defer runtime.Close(ctx)

events, err := runtime.SubscribeEvents(ctx, 0)
if err != nil {
	log.Fatal(err)
}
go func() {
	for event := range events {
		// 处理持久化生命周期事件与实时模型流式增量
	}
}()

submission, err := runtime.SubmitTurn(ctx, core.TurnRequest{Input: input}, core.StartIfIdle)
if err != nil {
	log.Fatal(err)
}
// submission.ExecutionID 可用于 runtime.Interrupt 与 WaitTurn
result, err := runtime.WaitTurn(ctx, submission.ExecutionID)
if err != nil {
	log.Fatal(err)
}
if result.Status == core.TaskCompleted {
	// result.Response 包含完整模型响应
}
```

### 事件订阅与历史重放边界

- `runtime.SubscribeEvents(ctx, afterCursor)`：从游标 `afterCursor` 之后重放持久化事件，随后持续监听实时生命周期与模型增量事件。
- `runtime.SubscribeEventsWithReplayComplete(ctx, afterCursor)`：重放历史事件并在历史回放追平、实时流即将开始的交界处发送一个瞬态标记事件 `core.EventRuntimeReplayComplete`。该标记不会持久化到数据库中。

## 轮次提交模式与队列精细控制

`SessionRuntime` 提供三种轮次提交模式：

- `core.StartIfIdle`：若当前会话正在运行、存在排队或处于暂停状态，则立即拒绝并返回 `core.ErrSessionBusy`。
- `core.QueueInput`：将用户输入作为 FIFO 轮次追加至会话队列，待先前任务完成后按序执行。
- `core.SteerInput`：将用户输入直接注入到当前正在运行的执行流程中（转向引导）。Core 在当前轮次工具调用完成且尚未发起下一次模型请求时注入；若模型已在生成最终回复，Core 会在定稿前拦截注入。触发 `EventTaskInputInjected` 事件。若当前执行已关闭注入边界，则退回为排队轮次。

### 队列管理与编辑 API

宿主无需重启会话即可操作排队轮次：

- `runtime.CancelQueuedTurn(ctx, executionID)`：取消一个尚未启动的排队轮次。若轮次已开始执行，返回 `core.ErrQueuedTurnStarted`；若不存在返回 `core.ErrTurnNotFound`。
- `runtime.CancelPendingTurns(ctx)`：原子取消会话队列中所有尚未启动的排队轮次，并中断当前正在运行的任务，等待清理完成后返回被取消的 `ExecutionID` 列表。
- `runtime.ReplaceQueuedTurnInput(ctx, executionID, newInput)`：原地替换未启动排队轮次的用户输入，保留其在队列中的位置，并原子取消其后的所有排队轮次。

## 持久化跨轮次目标 (Goal)

Lune Core 提供了跨任务轮次持续存在的高级目标体系，在目标完成、显式暂停、遭遇阻断或预算耗尽前自主持续推进：

```go
budget := int64(100000)
goal, err := runtime.CreateGoal(ctx, core.GoalCreateOptions{
	Objective:              "实现 OAuth2 PKCE 登录流程并编写回归测试",
	TokenBudget:            &budget,
	NoInteractiveDecisions: false,
})
```

### 目标状态机

目标的生命周期由 `core.GoalStatus` 定义：

- `core.GoalActive`：目标处于激活状态。当前轮次执行完毕后，运行时会自动投递后续推进轮次（`enqueueGoalContinuation`），驱动模型自发验证并达成目标。
- `core.GoalPaused`：目标被用户显式暂停（`runtime.PauseGoal`）或任务发生暂停。自动续轮停止。
- `core.GoalBlocked`：目标受阻（如连续 3 轮既无文本输出也无工具进展、发生未恢复异常、或模型判断缺少关键外部条件）。
- `core.GoalBudgetLimited`：非缓存 Token 消耗已达到或超过设定的 `TokenBudget`。
- `core.GoalComplete`：模型已验证并达成完整目标。

### 目标管理 API

`SessionRuntime` 提供以下目标管理方法：

- `CreateGoal(ctx, options core.GoalCreateOptions) (*core.Goal, error)`：创建或重设会话当前目标。
- `GetGoal(ctx) (*core.Goal, error)`：获取当前目标详情、状态、Token 消耗统计与停顿原因。
- `PauseGoal(ctx) (*core.Goal, error)`：暂停当前目标并中断正在运行的自动推进轮次。
- `ResumeGoal(ctx, options ...core.GoalResumeOptions) (*core.Goal, error)`：恢复已暂停、受阻或预算超额的目标，支持调整预算或交互模式。
- `EditGoal(ctx, objective string) (*core.Goal, error)`：修改目标文本描述，保留历史消耗统计。
- `SetGoalBudget(ctx, budget *int64) (*core.Goal, error)`：调整或取消 Token 预算上限。
- `ClearGoal(ctx) error`：清除当前目标并释放被暂停的用户排队输入。

目标状态变更触发 `core.EventGoalUpdated` 与 `core.EventGoalCleared`。

### 内置目标工具

Core 为模型注册了专用的会话控制工具：

- `get_goal`：任何 Agent 均可调用，读取当前目标、状态、已用 Token、预算上限及停滞原因。
- `update_goal`：**仅限 Lead Agent** 调用。在全面验证目标后将其标记为 `complete`，或在确无推进可能时标记为 `blocked` 并附带具体原因。目标的创建、暂停、编辑与预算调整权限属于用户和宿主。

## 澄清问题与审批流

Lead Agent 可使用内置的 `ask_user` 工具暂停当前任务，向用户发起批量结构化提问：

```json
{
  "questions": [
    {
      "id": "target",
      "question": "应使用哪种数据库存储？",
      "options": [
        {"label": "PostgreSQL", "description": "生产级高可用集群", "recommended": true},
        {"label": "SQLite", "description": "本地轻量级单文件数据库"}
      ]
    }
  ]
}
```

每道问题须有唯一 ID 与题干；`options` 为空允许自由文本作答；每道题最多一个选项可设置 `"recommended": true`。

### 问题解答与草稿持久化

1. 宿主通过 `runtime.PendingRequests(ctx)` 获取挂起的澄清请求及所属 ID。
2. 调用 `runtime.ResolveUserQuestion(ctx, core.UserQuestionAnswer{...})` 提交答案，自动恢复模型执行。
3. 宿主保存未提交的前端表单、草稿或选项：
   ```go
   err := runtime.SaveUserQuestionDraft(ctx, wait, draft)
   // 或保存任意非结构化前端视图状态载荷：
   err := runtime.SaveUserQuestionDraftPayload(ctx, wait, rawJSONBytes)
   ```
   `UserQuestionDraft.RawPayload` 使得宿主 UI 在重启后能完整复原未提交的视图状态。

## Plan 模式与执行阻断

Core 支持只读架构规划（Plan）与非交互式计划执行的分离：

1. 规划阶段，模型输出被包裹在 `<proposed_plan>...</proposed_plan>`（或兼容的 `<proposed_plan_end/>`）中的计划内容。
2. 任务完成后，`TaskResult.ProposedPlan` 提供结构化计划内容。
3. 宿主确认后通过 `runtime.ImplementPlan` 执行计划：

```go
impl, err := runtime.ImplementPlan(ctx, result.ExecutionID, core.PlanImplementationOptions{
	Mode:           core.PlanImplementSameContext, // 或 core.PlanImplementNewContext
	PermissionMode: core.PermissionWorkspace,
	Authorization: core.PlanAuthorization{
		Paths:                    []tool.PathGrant{{Path: "/external/dir", Access: tool.PathReadWrite}},
		AllowNetwork:             true,
		AllowedTools:             []string{"read_file", "write_file", "edit_file", "command"},
		ApprovedHighRiskCommands: []string{"npm install"},
	},
})
```

- `PlanImplementSameContext`：在原会话上下文中继续执行。
- `PlanImplementNewContext`：分支出仅包含计划文本的干净新会话执行。

### 执行阻断器 (Execution Blocker)

在计划执行或自动化运行（`NoInteractiveDecisions = true`）期间，交互式权限弹窗与提问均被关闭。若模型调用未授权的危险操作，任务立即安全暂停，发出 `core.EventExecutionBlocked` 事件，并在 `runtime.PendingRequests` 中呈现 `core.PendingExecutionBlocker`。宿主应提示用户重新规划，而非弹出运行时授权窗。

## 权限控制、显式审批与工具链

所有工具注册均划分权限等级：
- `workspace-read`：工作区内只读。
- `workspace-write`：工作区内修改。
- `command`：运行命令。
- `network`：外部网络/HTTP 请求。
- `session-control`：会话生命周期控制。

### 权限模式

通过 `Core.SetPermissionMode(mode)` 设置：
- `read-only`：只读工作区文件；访问外部路径、网络或未分类工具须询问；阻止文件写入与命令执行。
- `workspace`：读写工作区文件；执行命令、访问外部路径、网络或未分类工具须询问。
- `auto`：读写工作区文件；对命令与外部操作执行单次模型结构化安全评审（`AutoReviewApprove` / `AutoReviewAskUser`），仅在存在安全风险时向用户弹窗询问。
- `full-access`：允许注册工具与当前用户权限文件而无需提示；针对极其危险指令的窄黑名单依然生效。

### 审批决策与会话授权记忆

当产生 `EventApprovalRequired` 时，宿主调用 `runtime.ResolveApprovalDecision(ctx, id, decision)`：
- `tool.ApprovalAllowOnce`：仅批准本次单次执行。
- `tool.ApprovalAllowSession`：在当前会话生命周期内永久批准该操作。基于工具名称、规范化参数与调用指纹持久化为 `SessionApproval`。
- `tool.ApprovalDeny`：明确拒绝本次调用，向模型反馈拒绝信息。

### 外部路径授权与工具链路径

- **会话外部路径授权**：`tool.PathRead`（外部目录只读）、`tool.PathReadWrite`（外部目录读写）。
- **沙箱工具链环境授权**：通过 `core.Config.CommandToolchainPaths` 或运行时 API `client.CommandToolchainPaths()` / `client.SetCommandToolchainPaths(paths)` 为命令沙箱动态授予编译器或语言环境路径的只读+可执行权限。

## 内置工具完整参考

| 类别 | 工具名 | 主要参数 | 权限等级 | 常驻可见 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `files` | `read_file` | `path`, `start_line` (默认 1), `limit` (默认 200, 最大 500) | `workspace-read` | 是 | 按行号分页读取文本文件 |
| `files` | `write_file` | `path`, `content` | `workspace-write` | 是 | 写入覆盖文件全文 |
| `files` | `edit_file` | `path`, `old_text`, `new_text` | `workspace-write` | 是 | 精确文本块匹配与替换 |
| `files` | `read_image` | `path` | `workspace-read` | 是 | 读取 PNG/JPEG/WebP 并转换为视觉输入 |
| `files` | `list_dir` | `path`, `limit` (最大 500) | `workspace-read` | 是 | 列出目录结构 |
| `files` | `glob` | `pattern` | `workspace-read` | 是 | 文件名通配符快速检索 |
| `files` | `grep` | `query`, `path` | `workspace-read` | 是 | 正则/关键字文件内容搜索 |
| `command` | `command` | `command`, `background` (bool), `yield_time_ms`, `timeout_ms` | `command` | 是 | 内核级沙箱中运行 Shell 指令 |
| `command` | `command_status`| `command_id`, `cursor`, `wait_ms` (0-5000) | `workspace-read` | 是 | 分页轮询增量标准输出 |
| `command` | `command_stop` | `command_id` | `command` | 是 | 强制终止正在运行的命令进程 |
| `web` | `web_search` | `query`, `limit` | `network` | 否 | 互联网信息检索 |
| `web` | `web_fetch` | `url` | `network` | 否 | 获取网页并提取正文内容 |
| `memory` | `search_memory`| `query`, `limit` | `workspace-read` | 否 | 检索工作区持久化记忆 |
| `team` | `delegate_team`| `mode` (`"task"` 或 `"fork"`), `teammates`, `minimum_successes` | `session-control`| 否 | 团队图分支委派与协同调度 |
| `goal` | `get_goal` | _(无)_ | `session-control`| 是 | 读取当前会话持久化目标状态与预算 |
| `goal` | `update_goal` | `status` (`"complete"` 或 `"blocked"`), `reason` | `session-control`| 是 | 更新目标状态（仅限 Lead Agent） |
| _lead_ | `ask_user` | `questions: [{id, question, options}]` | _(保留)_ | 是 | 澄清提问与决策收集 |

## 长命令执行与沙箱机制

编译配套沙箱执行辅助程序：
```sh
go build -trimpath ./cmd/lune-sandbox
```
将其置于宿主二进制同级目录、系统 `PATH` 或由 `core.Config.SandboxHelperPath` 指定。
- Linux 使用 Landlock 与 seccomp 隔离，持久化 Go 模块与编译缓存至工作区专属目录。
- Windows 使用 AppContainer 隔离，完整访问模式下使用剥离特权的 Medium Integrity Token。
- 命令输出由系统在内存中保留最近 1 MiB。
- 传入 `background: true` 可启动持久常驻服务。后台服务归属于会话，任务完成后保持运行，直到被显式停止或会话关闭。

## MCP 扩展与认证体系

Core 支持标准 `stdio` 与远程 `streamable-http` 两种 MCP 传输方式：

### 配置文件定义

```json
"mcpServers": {
  "cli-tools": {
    "transport": "stdio",
    "command": "/usr/local/bin/tools-server",
    "args": ["--listen"],
    "env": {"DEBUG": "0"}
  },
  "remote-service": {
    "transport": "streamable-http",
    "url": "https://mcp.service.com/mcp",
    "clientMetadataURL": "https://service.com/oauth/metadata.json"
  }
}
```

### OAuth 认证与宿主定制

- 调用 `client.AuthenticateMCPServer(ctx, serverName)` 发起 OAuth PKCE 授权。
- `core.Config.BrowserOpener` 定制浏览器唤起：支持 `core.SystemBrowserOpener`（系统浏览器）、`core.HeadlessBrowserOpener`（无头模式）或自定义实现。
- `core.Config.CredentialStorage` 定制凭据存储：支持 `core.KeyringStorage`（系统安全凭据环）、测试用的内存凭据 `core.NewMemoryCredentialStorage()` 或自定义实现。
- 调用 `client.OpenMCPURL(serverName, target)` 唤起交互式 URL。

### 宿主受控资源与提示词

MCP 资源与提示词始终由宿主控制，绝不会自动污染模型上下文：
- `runtime.ListMCPPrompts`, `runtime.GetMCPPrompt`
- `runtime.ListMCPResources`, `runtime.ListMCPResourceTemplates`, `runtime.ReadMCPResource`
- 返回类型为 `*model.ResponseInput`，宿主可按需封装后作为输入提交给任务。

## 工作区持久记忆

工作区记忆默认开启。每轮任务完成后，Core 通过一次轻量级模型调用提取工作区公用偏好、项目规范与事实：
- 与会话数据一同保存在 SQLite 中。删除来源会话会自动清理相应提取的记忆；手动添加的记忆保持持久。
- 通过 `ListMemories`、`SearchMemories`、`AddMemory`、`UpdateMemory`、`DeleteMemory`、`ClearMemories` 统一管理。
- 宿主可通过 `core.Config.Memory.Embedder` 注入语义向量嵌入模型。嵌入配置变更后调用 `client.ReindexMemories(ctx)` 重建索引。

## 离线执行存储升级

对旧版本状态目录进行离线迁移：

```go
report, err := core.UpgradeState(ctx, stateDir)
```
或通过 CLI 执行：`lune migrate --state-dir <directory>`。

升级器获取独占排他锁，通过 SQLite `VACUUM INTO` 备份 `sessions.db` 至 `upgrade-backup-*` 目录，并将检查点升级至 execution format 2 / Graph format 10。旧的历史、设置与 Token 记录完整保留；旧的进行中目标置为暂停。

在任务执行期间，若遭遇排队消费或数据库持久化故障，排队消费工作协程将安全挂起，杜绝立即重试引发的风暴。修复存储后重新打开 `SessionRuntime` 或重试 `WaitTurn` 即可安全对齐恢复。
