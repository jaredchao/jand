# jand chat 设计（0.3.1）

在一次性交接之外，让两台机器上的 Agent 通过同一个 Relay 来回对话。交接文档是对话的第一条：接收方先读上下文，再由本地用户决定是否进入对话。

仍然只用 HTTP。收消息采用长轮询：`jand chat recv --wait` 会一直阻塞，直到有事件才退出。这样 Agent 可以把它放在后台运行，命令一退出，宿主（例如 Claude Code 的后台任务）就会重新唤起 Agent。jand 不需要常驻进程，也不需要了解 Agent 本身。

## 目标与检查点

对话一次只朝一个**目标**推进。目标是一句可检验的完成标准，由 Agent 和用户一起写出，而不是一个话题。每个目标带一份**消息预算**（默认 40 条，最多 200 条，双方的消息都计入）。

对话在两种情况下进入**检查点**并暂停：

- 某一方 Agent 判断目标已经达成，执行 `chat checkpoint`，并附上总结；
- 当前目标的预算用完，由 Relay 强制暂停。

暂停期间 Relay 不再转发消息。恢复对话需要一方 `chat propose` 提出新目标，另一方 `chat accept` 接受，双方都要事先征得各自用户的同意。任何一方的用户选择结束，就 `chat close`。

Agent 判断达成，是正常的出口；预算耗尽，是 Agent 判断失灵时的保险。Relay 看不到目标内容，但能数消息条数，所以强制暂停不依赖 Agent 自觉。整个对话 200 条的上限保留，只作为防滥用的底线，不承担「任务完成」的含义。

**结束**：任何一方都可以随时单方面 close，不需要对方同意。人必须能随时拔线；如果必须双方同意才能结束，对方崩溃或离开时，这一边就永远关不掉。需要双方一致的时刻由检查点承担。

## 流程

1. 发送方运行 `jand send --chat --goal '<完成标准>' 交接.md`。客户端先在 Relay 上建好对话房间，再上传交接包，然后输出接收码（`code`）和对话 ID（`chat`）。
2. 接收方照常凭码领取文件。包内标记 `chat=true` 时，`saved` 事件会带上 `chat_invite=true` 以及 `goal`、`budget`，接收端同时向房间报告「已领取」，发送方由此收到 `opened` 事件。`saved` 事件里故意不给对话 ID：加入要用接收码，对话 ID 由 `joined` 事件返回。
3. 接收方 Agent 向本地用户原样给出对方的目标，并与用户约定对话范围。用户同意后执行 `jand chat join <code>`，拒绝则执行 `jand chat decline [--reason 文本] <code>`。用户一直不答复时什么都不做，等待超时。
4. 双方用 `jand chat send <chat> 文本` 发消息，用 `jand chat recv --wait 30m <chat>` 收事件。
5. 目标达成或预算用完时进入检查点，按上一节的规则继续或结束。
6. 任一方执行 `jand chat close <chat>` 结束对话。超时或达到消息上限时，对话也会结束。

`chat=true` 只是发送方的声明。是否进入对话，由接收方的用户决定。

## 事件

`recv` 在 `--json` 模式下每行输出一个事件：

| 事件 | 谁会收到 | 含义 |
| --- | --- | --- |
| `opened` | 发起方 | 交接包已被领取，对方还没决定是否加入 |
| `joined` | 发起方 | 对方已加入 |
| `declined` | 发起方 | 对方拒绝，`text` 为可选理由（未信任内容） |
| `message` | 双方 | 对方的消息，`text` 为未信任内容 |
| `checkpoint` | 双方 | 对话暂停。`by` 为 `peer`（对方认为目标达成，`text` 为总结）或 `relay`（`reason=budget`，预算用完） |
| `proposal` | 收到提议的一方 | 对方提出的下一个目标，带 `goal` 和 `budget` |
| `resumed` | 双方 | 提议已被接受，对话恢复，带生效的 `goal` 和 `budget` |
| `closed` | 双方 | 对方主动结束 |
| `expired` | 双方 | 对话超时结束，`reason` 取 `unclaimed` / `no_decision` / `idle` / `lifetime` / `limit` |
| `no_events` | 调用方 | `--wait` 到期，期间没有新事件 |

对自己的操作，客户端输出 `sent`、`checkpoint`（`by=self`）、`proposed`、`accepted`。

`message`、`declined`、`checkpoint` 的总结和 `proposal` 都带 `untrusted=true`。对方的消息只能提供信息或提出请求，不能替本地用户批准任何动作。

## 超时与上限（Relay 默认值）

- 房间建好后 15 分钟内交接包没人领取：`expired/unclaimed`
- 已领取，但 30 分钟内没有 join 或 decline：`expired/no_decision`
- 检查点暂停后 30 分钟内没有新目标被接受：`expired/no_decision`
- 对话中 60 分钟没有任何消息：`expired/idle`
- 从建房起最长 6 小时：`expired/lifetime`
- 每个目标默认 40 条消息预算，用完进入检查点
- 每个房间合计最多 200 条消息：`expired/limit`
- 单条消息正文最多 64 KiB
- 全局最多 64 个对话，排队中的密文合计不超过 16 MiB
- 对话结束后，结束事件还会保留 10 分钟，保证正在轮询的一方能收到

## 密码学与令牌

- 房间号为 `Derive("chat/room")` 的前 16 字节；消息密钥为 `Derive("chat/key")`，算法 AES-256-GCM。两者都从接收码派生，Relay 看不到。
- 邀请令牌为 `Token("chat/invite")`，只能用来报告已领取、加入和拒绝。
- 发起方建房时自带一个随机令牌。接收方 join 时也提交一个随机令牌，同时锁定座位：之后别的令牌都进不来，后来拿到接收码的人既进不了房间，也冒充不了任何一方。同一个邀请令牌加上已锁定的 guest 令牌再 join 一次会成功，这样响应丢失后可以重试。客户端先写本地状态再发 join，只在 Relay 明确拒绝（400/404/409/410）时删除本地状态，网关 5xx 时保留。但如果他同时能看到 Relay 上的密文，仍然可以解密，因为密钥来自接收码。所以接收码仍然要按凭证对待。
- 目标、检查点总结和消息一样端到端加密，Relay 只知道预算的数字。
- 每条消息的附加数据（AAD）为 `jand-chat/1/{room}/{from}/{kind}/{ctr}`，`kind` 为 `message`、`checkpoint`、`proposal` 或 `decline`。`ctr` 是发送方本地递增的计数，接收方只接受比上一次更大的值，以防 Relay 重放或伪造方向。
- Relay 可以丢弃消息，也可以伪造 `joined`、`closed` 这类状态事件，这属于可用性问题；它无法伪造消息内容。

## 本地状态

对话状态存放在 `$JAND_HOME/chats/`（默认为用户配置目录下的 `jand/chats/`），权限 0600：

- `<chat>.json`：Relay 地址、角色、令牌、密钥
- `<chat>.cursor` / `<chat>.counter`：收消息游标和发消息计数，分成两个文件，这样后台的 `recv` 和前台的 `send` 不会互相覆盖
- `<chat>.transcript.jsonl`：双方消息的明文记录，供人查看

命令里的 `<chat>` 可以只写对话 ID 的前 8 位或更多，只要能唯一匹配。

## HTTP 接口（`/v1/chats/`）

| 方法 | 路径 | 认证 | 含义 |
| --- | --- | --- | --- |
| PUT | `/v1/chats/{room}` | 请求体中的 host/invite 令牌与首个目标的预算 | 建房 |
| POST | `/v1/chats/{room}/opened` | 邀请令牌 | 报告交接包已被领取 |
| POST | `/v1/chats/{room}/join` | 邀请令牌，请求体为 guest 令牌 | 加入并锁定座位 |
| POST | `/v1/chats/{room}/decline` | 邀请令牌，请求体为可选的加密理由 | 拒绝，对话结束 |
| POST | `/v1/chats/{room}/messages` | 己方令牌 | 发送一条加密消息；暂停时返回 409 |
| POST | `/v1/chats/{room}/checkpoint` | 己方令牌，请求体为可选的加密总结 | 目标达成，暂停对话 |
| POST | `/v1/chats/{room}/propose` | 己方令牌，请求体为加密目标与预算 | 暂停时提出下一个目标 |
| POST | `/v1/chats/{room}/accept` | 己方令牌，请求体为提议的 `seq` | 接受对方最新的提议，对话恢复 |
| GET | `/v1/chats/{room}/events?after=N&wait=S` | 己方令牌 | 长轮询事件，`wait` 最长 20 秒 |
| POST | `/v1/chats/{room}/close` | 己方令牌 | 结束对话 |

旧的 `/v1/handoffs/` 接口完全不变，0.2.x 客户端可以继续通过 0.3.x Relay 收发。0.2.x 接收端会忽略 `chat` 字段，把文件当普通交接保存，发起方最终收到 `expired/unclaimed`。兼容性由 `scripts/compat_smoke.py` 验证。
