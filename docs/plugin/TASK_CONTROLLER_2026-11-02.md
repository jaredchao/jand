# 本地协作任务控制器与 DSH 适配 — 2026-11-02

本次范围：仅两个已授权仓库的本地实现、隔离自动测试、文档及审查工件。没有提交、推送、安装、发布、部署、UI 操作、真实模型联调、Codex queue 探测或旧房间收发。既有改动保留。

## 已实现的分工

| 层 | 负责内容 | 程序边界 |
| --- | --- | --- |
| 主对话 | 理解用户、讨论、解释结果、局部授权与关键决定 | 同一 chat 的唯一 inbox owner；查询紧凑状态，不接执行日志或自动提醒 |
| 独立统筹 session | 拆任务、排序、分派和验收 | DSH 原生独立 session；工具 guard 仅放行任务派发、查询和证据审查 |
| 执行者 | 一个有界任务 | 原生 subagent；仅能调用受管前台命令工具，禁止递归分派及接管 inbox |
| 普通程序 | 身份、记录、容量、版本、控制及恢复 | Jand `internal/taskcontrol`；DSH `src/tasks.js` / `src/task-process.js` 管实际生命周期 |

任务拆分质量、授权理解和证据是否足以业务验收仍属于 Agent 行为和用户判断。控制器只验证明确的状态条件，不能从任意文件路径或一句“通过”推出真实业务成功。

## 契约和存储

入口：`jand task --root ABS_DIR`，stdin 一个不超过 1 MiB 的 JSON request，stdout 一个 JSON response。`jand task --capabilities` 可只读查询契约版本及本平台锁支持。它不读取全局 Jand 配置、Relay、token、页面或网络。无效操作、损坏、身份冲突、持久化失败均返回非零和 `outcome:error`。`full`、`halted`、`capacity` 等是可区分的正常控制结果，不能只看进程退出码。

```json
{"op":"init","scope":{"host":"dsh","side":"profile-A","chat":"chat-id","phase":"goal-phase-id"},"owner":"conversation-session","mode":"three_level"}
```

- scope 是准确的宿主身份、本机侧/profile、chat 和目标阶段四元组；文件名为该元组 JSON 的 SHA-256。目标文字从不参与身份推断。owner 是原主对话 session；DSH 从实际 caller 和现有 binding 核对角色。
- 根目录和记录默认 0700 / 0600；所有进程在同一内核 `flock` 下完成读、校验、变更、临时文件 fsync、rename、目录 fsync。崩溃释放 OS 锁；不靠内存 Map、超时租约或 PID 猜锁过期。
- 每个 scope 一个 schema 1 ledger：任务、版本、执行 attempt、预留原生 session ID、统筹身份、输入回执、关键快照。创建身份先持久预留，再调用宿主。重复或重启只见现有身份，绝不超时重建。
- `submit`、`revise`、`cancel`、`notify`、`coordinator_wake` 以精确 event ID 去重。相同 ID 不同 payload 拒绝；同文不同 ID 保留为不同合法输入。满载或已停止的输入也保留原 request 和结果，可以显式查看，不能默默丢掉或自动重试。
- 当前进程不持有可恢复的旧 native handle、启动结果不明确或记录损坏时保守阻塞。重启只核对准确 live identity；缺证据保持 unknown，不能自动释放容量、重派、重建或把“查无进程”解释为已停止。

## 硬上限和实际派发

scope 默认 `running:1, queued:8`，可在初次 init 显式传 `limits`。根目录同时限制所有 chat/phase 的总量，默认 **1 个统筹 session、1 个执行/待审查/中断待决定槽、8 个待执行槽**。本地 `controller.json` 可以配置：

```json
{"running":1,"queued":8,"coordinators":1}
```

允许范围：running 1–16，queued 1–1024，coordinators 1–8。变更容量不会自动派发。其他 scope 的不确定记录继续占槽；其他 ledger 损坏会阻止新 admission，但不阻止当前 scope 的停止控制。

- 宿主调用前，`coordinator` 和 `reserve` 在跨进程事务内检查实际容量。
- `starting`、`running`、`canceling`、`unknown` 都占执行槽。已退出但未验收的 `result_pending` 及暂停中断后等待决定的任务也占槽，防止把未审结果变成另一种无限积压。明确 accept、discard 或确认退出后释放对应槽；暂停中断任务还需显式取消或修订以解决重复副作用风险。
- 队列只保存待执行任务；输入回执是另一份可查询历史，不是无界执行队列。没有后台自动重试或超时清队列。
- 满载返回 full/capacity；未知或正在取消时不派新的冲突执行。根目录不同意味着不同控制域；本实现不宣称跨多个独立 dataDir 的全机资源配额。

## 变更、停止和验收

| 操作 | 含义 |
| --- | --- |
| `revise` | 检查预期版本，版本加一；旧执行先进入取消，新版本受阻，确认旧退出后才入队 |
| `cancel` | 取消一个任务，保留记录和不确定退出，不能完成新版本 |
| `stop` | 先持久 halted，取消本 scope 的待执行和运行任务，再走宿主取消/退出核对 |
| `pause` | 同样先禁止新派发并停止当前执行，保留原待执行队列；中断任务受阻，不自动重跑 |
| `resume` | 只在所有运行/启动及统筹停止已确认后解除 halted；不恢复收件或远端对话 |
| `observe` | 适配器提交实际启动、失败、不确定、退出或结果依据；终态不能被旧返回复活 |
| `accept` | 精确匹配当前任务版本、attempt、待审状态和明确验收证据才完成 |

停止接收沿用 `jand_stop`；关闭远端对话仍是另一项明确本地授权操作。普通 peer 文本不会直接调用任何控制操作。

DSH 的普通程序控制监视器读取持久 control ID；本地 CLI 写入 stop/pause 后，无需任何模型先接收或处理消息，就会执行取消与核对。监视器按控制 ID 去重，不唤醒模型，不排普通任务；宿主运行时不可达时保留 unknown，不能承诺已经退出。

DSH 停止先写截止，再中断尚未完成的 start，排空准确执行者的后代并释放原生 handle，结束所有关联受管命令。主对话可以直接调用停止工具，不依赖统筹模型先回复。超时、缺 handle、宿主失败、进程退出未确认都记 unknown，并保留 halted。晚到的 start receipt 会被释放，晚到旧结果不能完成新版本。

受管命令由独立 Node supervisor 启动一个 Unix process group。IPC 断开时也请求终止；TERM 后必要时 KILL，检查组退出。macOS 上 kill(0) 可能返回 EPERM，使用只读取 PGID/PID 的 `ps` 独立核对，只有确实无组成员才确认退出，不能把 EPERM 当成功。输出上限 1 MiB，返回模型至多 64 KiB；超量、命令失败或状态不明不会转成成功结果。

**范围限制：** process group 约束覆盖受管前台命令及留在组内的子进程。禁止 detached daemon、setsid、手工逃逸进程组；本实现不是恶意代码沙箱、macOS 全机进程隔离或容器。外部手工进程、其他插件执行入口不在本控制器的停止保证内。执行者的工具 guard 阻止它使用非受管工具，但不能代替操作系统安全隔离。

## DSH 能力和通知

本机 DSH `0.2.1-rc.5` 默认 maxDepth 为 1。隔离测试验证的是：程序创建并保留**独立原生统筹 session**，它下面派一个直接 subagent。没有把该结果称作嵌套 subagent 三级验收。若宿主允许真实嵌套，也有有限深度适配分支；这条分支的真实生命周期尚未验收。

`jand_task_capabilities` 区分完整角色分工、两层降级和手动模式，依据实际 create/resume、subagent、drain、guard、持久化、深度和平台能力；API 存在与运行验证分开。Windows 缺本控制器 OS 锁/进程组适配，明确退到 manual，不声称有一键停止。

统筹者自然 idle 时保留独立 session；后续本地要求使用 `jand_task_continue`，同一 event 只获得一次持久唤醒 admission。确认 stop 后可以显式 resume，再从持久 session 恢复同一身份。启动/恢复不确定则 unknown，不盲重试。不以 idle 证明消息已处理。

主对话通知采用 **query-only**：详细原生子任务结果到统筹者，主对话不自动收模型执行日志；本地任务每个 ID 一份最新关键快照。`jand_task_status` 给计数和最近记录，详细依据显式查询。没有可靠处理回执时不创建自动唤醒或 reminders。`notice_ack` 契约仅接受明确处理凭据；当前适配器不根据 recv、队列消失、idle 自动调用它。Codex watch 的 `agent_notify_blocked/host_receipt_unavailable` 保护不变。

## 开发接入与分发边界

发布内核仍是原固定版本；未更换 manifest、FINAL_CANDIDATE 或用户安装。新控制器通过独立配置 `taskControllerPath` + `taskControllerSha256` 显式指定绝对路径及 SHA-256，每次执行校验哈希。不配置时受管任务不可用，保留手动降级。插件仍从原干净固定 revision 获取网络内核，不会复制 Jand 脏工作树到可分发 kernel。

`npm run test:tasks` 在临时目录从当前源码构建测试控制器，使用实际 DSH runtime 和确定性 fixture，结束删除测试二进制与临时状态；不会改实际 profile、全局安装或旧房间。`JAND_SOURCE_ROOT` 可指定开发源码根目录。

具体本次结果及审查工件见 DSH 项目 `docs/TASK_CONTROLLER_2026-11-02.md`。真实模型遵守、用户实际 profile 可见能力、UI 响应和跨设备行为仍未验收；没有为此恢复旧监听或联调。
