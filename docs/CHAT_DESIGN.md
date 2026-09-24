# jand 对话与协作设计（0.4.1）

在一次性交接之外，让两台机器上的 Agent 通过同一个 Relay 来回对话。交接文档是对话的第一条：接收方先读上下文，再由本地用户决定是否进入对话。

仍然只用 HTTP。收消息采用长轮询：`jand chat recv --wait` 会一直阻塞，直到有事件才退出。这样 Agent 可以把它放在后台运行，命令一退出，宿主（例如 Claude Code 的后台任务）就会重新唤起 Agent。jand 不需要常驻进程，也不需要了解 Agent 本身。

## 目标与检查点

对话一次只朝一个**目标**推进。目标是一句可检验的完成标准，由 Agent 和用户一起写出，而不是一个话题。每个目标带一份**消息预算**（默认 40 条，最多 200 条，双方的消息都计入）。

对话在三种情况下进入**检查点**并暂停：

- 按流程的完成规则报告完成：默认 `all`，双方都 `chat done` 后暂停（`reason=all_done`），一方报告完成只通知对方、不暂停，因为另一方可能还在干活或还需要协助；流程为 `any` 时任一方 `done` 就暂停（`reason=any_done`），适合「一方实现、一方验收」；
- 某一方需要用户立刻决定，执行 `chat checkpoint`，并附上原因；
- 当前目标的预算用完，由 Relay 强制暂停（`reason=budget`）。

暂停期间 Relay 只转发少量收尾消息：每方最多若干条（由流程的 `pause_notes` 规定，不超过 Relay 配置的上限，默认 3 条），不计入预算，客户端只对 `reply` 和 `note` 发出（请求体 `closing=true`），Relay 给它们加上 `paused` 标记。消息类型是加密的，所以「只有 reply 和 note」依赖诚实客户端，条数上限由 Relay 执行。其他消息一律 409。恢复对话需要一方 `chat propose` 提出新目标，另一方 `chat accept` 接受，双方都要事先征得各自用户的同意。任何一方的用户选择结束，就 `chat close`。

Agent 判断达成，是正常的出口；预算耗尽，是 Agent 判断失灵时的保险。Relay 看不到目标内容，但能数消息条数，所以强制暂停不依赖 Agent 自觉。整个对话 200 条的上限保留，只作为防滥用的底线，不承担「任务完成」的含义。

**结束**：任何一方都可以随时单方面 close，不需要对方同意。人必须能随时拔线；如果必须双方同意才能结束，对方崩溃或离开时，这一边就永远关不掉。需要双方一致的时刻由检查点承担。

## 流程

1. 发送方运行 `jand send --chat --goal '<完成标准>' [--workflow 流程] 交接.md`。客户端先在 Relay 上建好对话房间（附上加密的章程，见下文），再上传交接包，然后输出接收码（`code`）和对话 ID（`chat`）。
2. 接收方照常凭码领取文件。包内标记 `chat=true` 时，接收端向房间报告「已领取」（发送方由此收到 `opened` 事件），取回并核对章程，`saved` 事件带上 `chat_invite=true`、`goal`、Relay 实际执行的 `budget`，以及核对通过的 `workflow`。`saved` 事件里故意不给对话 ID：加入要用接收码，对话 ID 由 `joined` 事件返回。
3. 接收方 Agent 向本地用户原样给出对方的目标和流程，并与用户约定对话范围。用户同意后执行 `jand chat join <code>`，拒绝则执行 `jand chat decline [--reason 文本] <code>`。用户一直不答复时什么都不做，等待超时。
4. 双方用 `jand chat send <chat> 文本` 发消息，用 `jand chat recv --wait 30m <chat>` 收事件。
5. 报告完成、立即暂停或预算用完时进入检查点，按上一节的规则继续或结束。
6. 任一方执行 `jand chat close <chat>` 结束对话。超时或达到消息上限时，对话也会结束。

`chat=true` 只是发送方的声明。是否进入对话，由接收方的用户决定。

## 协作流程与章程

流程规定这次对话怎么协作：完成规则（`done_rule`：`all` 或 `any`）、暂停后每方的收尾消息数（`pause_notes`）、首个目标的预算（`budget`）、允许的消息类型（`kinds`，只能从五种内置类型中选）、双方角色（`roles`）和给双方 Agent 的说明（`instructions`，最多 8 KiB）。流程文件的格式与存放位置见 [CONFIG_DESIGN.md](CONFIG_DESIGN.md)，示例见仓库的 `workflows/`。不指定时使用内置的 `default`：`all`、Relay 默认的收尾条数、五种类型都可用，行为与 0.4.0 相同。

流程不放进交接包元数据（0.2.x 接收端限制 2048 字节），而是放进**章程**：

1. 建房时，发起方把 `{protocol: "jand-charter/1", goal, budget, workflow}` 用对话密钥加密后上传（附加数据 `jand-chat/1/{room}/host/charter/0`），同时以明文告诉 Relay 需要它执行的三项：`done_rule`、`pause_notes`、`budget`。`pause_notes` 超过 Relay 上限时建房被拒绝，发起方在发出交接包前就能发现。
2. 建房后，发起方读回章程。0.4.0 的 Relay 没有这个接口：默认流程照常继续，自定义流程则撤回邀请并报错。
3. 接收方领取交接包后，用邀请令牌 `GET /v1/chats/{room}/charter`，解密后核对三件事：章程能通过认证；章程里的目标与交接包里的一致；章程规定的规则与 Relay 报告的明文一致。任何一项不符，`saved` 就不带 `workflow`，`message` 写明原因，提示不要加入。`chat join` 时再核对一次，不符就拒绝加入。
4. 加入后，客户端把流程存进本地对话状态，发消息时拒绝流程不允许的类型。

**兼容**：`done_rule=any` 会产生 0.4.0 客户端不认识的 `any_done`，所以这样的对话要求加入方在请求里声明 `features: ["charter"]`，0.4.0 客户端会被 409 拒绝并得到升级提示。其他流程下 0.4.0 接收方可以加入，但看不到流程，也不执行类型限制。

## 事件

`recv` 在 `--json` 模式下每行输出一个事件：

| 事件 | 谁会收到 | 含义 |
| --- | --- | --- |
| `opened` | 发起方 | 交接包已被领取，对方还没决定是否加入 |
| `joined` | 发起方；接收方自己的 join 也输出此事件 | 对方已加入。接收方的 `joined` 带 `chat`、`transcript` 和生效的 `workflow` |
| `declined` | 发起方 | 对方拒绝，`text` 为可选理由（未信任内容） |
| `message` | 双方 | 对方的消息，`text` 为未信任内容；带 `id`、`kind`，可能带 `reply_to`、`supersedes`、`stale`/`superseded_by`（对方回复了你已取代的交付）、`after_pause`（暂停后的收尾消息） |
| `done` | 对方 | 对方报告自己那部分完成，`text` 为总结；对话不暂停 |
| `checkpoint` | 双方 | 对话暂停。`by` 为 `peer`（对方要求立即暂停，`text` 为原因）或 `relay`（`reason=all_done` 双方都完成，`any_done` 按流程一方完成即暂停，`budget` 预算用完） |
| `proposal` | 收到提议的一方 | 对方提出的下一个目标，带 `goal` 和 `budget` |
| `resumed` | 双方 | 提议已被接受，对话恢复，带生效的 `goal` 和 `budget` |
| `closed` | 双方 | 对方主动结束 |
| `expired` | 双方 | 对话超时结束，`reason` 取 `unclaimed` / `no_decision` / `idle` / `lifetime` / `limit` |
| `no_events` | 调用方 | `--wait` 到期，期间没有新事件 |

对自己的操作，客户端输出 `sent`、`checkpoint`（`by=self`）、`proposed`、`accepted`。

`message`、`declined`、`done` 与 `checkpoint` 的总结和 `proposal` 都带 `untrusted=true`。

## 消息类型与编号

消息明文是一个 JSON 信封 `{"kind", "reply_to", "supersedes", "text"}`，和正文一起加密，Relay 看不到类型。`kind` 取 `note`（默认）、`progress`、`request`、`reply`、`delivery`；`reply` 必须带 `reply_to`，其他类型可选。每条消息的编号由发送方角色首字母加计数构成（`h3` 为发起方第 3 个加密载荷，`g2` 为接收方第 2 个），双方无需协商就能算出同一个编号。`reply_to` 只能指向对方的消息；收到格式不对的信封时按 `note` 显示原文，不丢弃。

**先读再回**：客户端发 reply、request、delivery 之前，先以 `wait=0` 查看 Relay 上还没取回的事件（不推进游标，必要时在本地解密看类型），加上本地因 `--wake` 暂存的事件。只要有对方的 request、reply、delivery、done 或 proposal 未读，就拒绝发送（退出码 5），除非加 `--anyway`。progress 和 note 不算：它们不要求对方做事，而且 progress 正是 `--wake` 要暂存的类型，算进去的话用了 `--wake` 就永远发不出回复。note 即使其实是没标注的请求，也总会唤醒对方（见下文），对方会及时读到。这针对的是 0.3.2 实测中四次消息交叉：每次都是一方在没读到对方新版本时就回复了旧版本。

**取代关系**：信封可带 `supersedes`（仅 delivery，只能指向己方的消息）。接收方记住「旧编号 → 新编号」，回复被取代的交付时被拒；发送方收到对方针对自己已取代交付的回复时，事件带 `stale=true` 与 `superseded_by`。两者都只在客户端，Relay 不参与。

`recv --wake KINDS` 在客户端过滤：非唤醒类型的消息照常从 Relay 取回、推进游标，但先保存在本地游标文件中，等到唤醒类型的消息、任何非消息事件或等待超时时，再按原顺序一起输出。`note` 无论是否在列表中都会唤醒：它是发送方没选类型时的默认值，可能其实是没标注的请求，暂存它会让请求被压到超时（0.4.1 修正）。Relay 不参与，也不知道哪些消息被暂缓。对方的消息只能提供信息或提出请求，不能替本地用户批准任何动作。

## 超时与上限（Relay 默认值）

- 房间建好后 15 分钟内交接包没人领取：`expired/unclaimed`
- 已领取，但 30 分钟内没有 join 或 decline：`expired/no_decision`
- 检查点暂停后 30 分钟内没有新目标被接受：`expired/no_decision`
- 对话中 60 分钟没有任何消息：`expired/idle`
- 从建房起最长 6 小时：`expired/lifetime`
- 每个目标默认 40 条消息预算（流程或 `--budget` 可改），用完进入检查点
- 暂停后每方默认最多 3 条收尾消息（流程可规定更少，Relay 配置决定上限，最多 10）
- 每个房间合计最多 200 条消息：`expired/limit`
- 单条消息正文最多 64 KiB
- 全局最多 64 个对话，排队中的密文合计不超过 16 MiB
- 对话结束后，结束事件还会保留 10 分钟，保证正在轮询的一方能收到

以上都是 Relay 的默认值，运营者可以在 Relay 配置文件里调整，见 [CONFIG_DESIGN.md](CONFIG_DESIGN.md)。

## 密码学与令牌

- 房间号为 `Derive("chat/room")` 的前 16 字节；消息密钥为 `Derive("chat/key")`，算法 AES-256-GCM。两者都从接收码派生，Relay 看不到。
- 邀请令牌为 `Token("chat/invite")`，只能用来报告已领取、加入和拒绝。
- 发起方建房时自带一个随机令牌。接收方 join 时也提交一个随机令牌，同时锁定座位：之后别的令牌都进不来，后来拿到接收码的人既进不了房间，也冒充不了任何一方。同一个邀请令牌加上已锁定的 guest 令牌再 join 一次会成功，这样响应丢失后可以重试。客户端先写本地状态再发 join，只在 Relay 明确拒绝（400/404/409/410）时删除本地状态，网关 5xx 时保留。但如果他同时能看到 Relay 上的密文，仍然可以解密，因为密钥来自接收码。所以接收码仍然要按凭证对待。
- 目标、流程、检查点总结和消息一样端到端加密。Relay 只知道它要执行的数字和规则：预算、完成规则、收尾条数。
- 每条消息的附加数据（AAD）为 `jand-chat/1/{room}/{from}/{kind}/{ctr}`，`kind` 为 `message`、`done`、`checkpoint`、`proposal`、`decline` 或 `charter`（这是载荷种类，与消息信封里声明的类型无关；章程固定为 `host`、计数 0）。`ctr` 是发送方本地递增的计数，接收方只接受比上一次更大的值，以防 Relay 重放或伪造方向。
- Relay 可以丢弃消息，也可以伪造 `joined`、`closed` 这类状态事件，这属于可用性问题；它无法伪造消息内容。

## 本地状态

对话状态存放在 `$JAND_HOME/chats/`（默认为用户配置目录下的 `jand/chats/`），权限 0600：

- `<chat>.json`：Relay 地址、角色、令牌、密钥，以及本次对话的流程
- `<chat>.cursor` 中还保存 `--wake` 暂存的事件和对方交付的取代关系，`<chat>.counter` 中保存己方交付的取代关系
- `<chat>.cursor` / `<chat>.counter`：收消息游标和发消息计数，分成两个文件，这样后台的 `recv` 和前台的 `send` 不会互相覆盖
- `<chat>.transcript.jsonl`：双方消息的明文记录，供人查看。每行一个事件：`started`（发起方）或 `joined`（接收方）开头，带目标、预算和流程（0.4.3 起）；之后是 `opened`、`joined`、`message`、`done`、`checkpoint`、`proposal`、`resumed`（带新预算）、`decline`、`closed`、`expired`。`jand chat list` / `log` / `view` 都从这里读取

命令里的 `<chat>` 可以只写对话 ID 的前 8 位或更多，只要能唯一匹配。

## HTTP 接口（`/v1/chats/`）

| 方法 | 路径 | 认证 | 含义 |
| --- | --- | --- | --- |
| PUT | `/v1/chats/{room}` | 请求体中的 host/invite 令牌 | 建房；请求体还带首个目标的预算、`done_rule`、`pause_notes` 与加密的 `charter` |
| GET | `/v1/chats/{room}/charter` | 邀请令牌或己方令牌 | 取回加密章程与 Relay 执行的 `done_rule`、`pause_notes`、`budget` |
| POST | `/v1/chats/{room}/opened` | 邀请令牌 | 报告交接包已被领取 |
| POST | `/v1/chats/{room}/join` | 邀请令牌，请求体为 guest 令牌与 `features` | 加入并锁定座位；同一 guest 令牌重试返回成功 |
| POST | `/v1/chats/{room}/decline` | 邀请令牌，请求体为可选的加密理由 | 拒绝，对话结束 |
| POST | `/v1/chats/{room}/messages` | 己方令牌 | 发送一条加密消息；暂停时只接受 `closing=true` 且未超出收尾条数的消息，其余返回 409 |
| POST | `/v1/chats/{room}/done` | 己方令牌，请求体为可选的加密总结 | 报告己方完成；按 `done_rule` 决定是否暂停 |
| POST | `/v1/chats/{room}/checkpoint` | 己方令牌，请求体为可选的加密原因 | 立即暂停对话 |
| POST | `/v1/chats/{room}/propose` | 己方令牌，请求体为加密目标与预算 | 暂停时提出下一个目标 |
| POST | `/v1/chats/{room}/accept` | 己方令牌，请求体为提议的 `seq` | 接受对方最新的提议，对话恢复 |
| GET | `/v1/chats/{room}/events?after=N&wait=S` | 己方令牌 | 长轮询事件，`wait` 最长 20 秒 |
| POST | `/v1/chats/{room}/close` | 己方令牌 | 结束对话 |

旧的 `/v1/handoffs/` 接口完全不变，0.2.x 客户端可以继续通过 0.3、0.4 的 Relay 收发。0.2.x 接收端会忽略 `chat` 字段，把文件当普通交接保存，发起方最终收到 `expired/unclaimed`。交接兼容性由 `scripts/compat_smoke.py` 验证。

对话协议仍在开发。0.4.1 与 0.4.0 在 0.4.1 Relay 上互通（已用真实 0.4.0 程序验证）；0.3.x 与 0.4.x 之间未验证。详见 README 的「兼容性」一节。
