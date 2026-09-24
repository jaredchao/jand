# jand 0.2 本机验证记录

日期：2026-09-22。记录在本机工作副本中产生；源码由改名前的 handoff 工作副本复制而来，原目录没有改动。本记录写成时该工作副本尚未纳入 Git；首次提交后以仓库历史为准。

## 已验证

- `go test -race ./...` 与 `go vet ./...` 通过；测试覆盖一次性领取、过期和容量限制、错误码、密文篡改、伪造回执及回执失败等行为。
- `make smoke` 通过：三个独立 CLI 进程在本机 HTTP Relay 完成中文 Markdown 传输；发送端先退出，接收文件字节和 SHA-256 一致；旧码不能再次领取；Relay 工作目录没有新增业务文件。
- `make release` 生成 Mac ARM64/AMD64、Windows AMD64、Linux AMD64/ARM64 客户端包及 Linux 部署包。顶层六个归档的 SHA-256 与 `SHA256SUMS.txt` 一致；逐包核对 `BUILD-INFO.json` 的产品、模块、命令和二进制 SHA-256，包内有 `jand-template.md` 与 QUICKSTART；部署包含两个 Linux 归档、`jand-relay.supervisor.conf`、安装说明和 Nginx/Caddy 示例，内部两个归档的 SHA-256 也一致。
- 本机 `./bin/jand --help` 显示新命令与 `JAND_RELAY`，`./bin/jand --version` 返回 `0.2.0-dev`。
- 通过本机独立 Relay 进程验证 `JAND_RELAY` 可供发送与接收使用；同时设置无效的旧 `HANDOFF_RELAY` 时仍按新变量完成传输。
- 2026-09-22 新增 Supervisor 部署示例和 `scripts/remote_smoke.py`。使用临时 Supervisor 4.3.0 配置（将服务器专用 `jand` 用户和路径替换为本机值）验证 `STOPPED → RUNNING → STOPPED`；受 Supervisor 管理的本机 Relay 完成健康检查、`queued`、文件字节和 SHA-256、`delivered` 回执、旧码拒绝。不可达本机入口返回非零退出码和 `health/transport` 失败，未被报告为成功。该检查对应 `HCT-2026-09-08-01` 的失败路径要求。

## 兼容与分发边界

当前 0.2 线协议保留 `handoff/0.2` 版本串和派生标签、`/v1/handoffs/` 路径与 `X-Handoff-*` 请求头。历史 0.1 WebSocket 客户端仍与 0.2 不兼容。`HANDOFF_RELAY` 已改为 `JAND_RELAY`，旧环境变量不再读取。

这次 `make release` 使用默认示例地址 `http://203.0.113.10:8787` 生成 QUICKSTART；它不是已部署的服务器地址。要给实际联测对象分发时，由用户确认目标 Relay 后重新执行 `python3 scripts/release.py --relay <实际地址>` 并核对新校验和。

上述本机测试证明代码、临时 Supervisor 运行和本机打包结果；公网路径另见下节。仍未验证真实服务器上的 Supervisor 用户和包含目录、证书文件路径、两台真实设备、两端 Codex 的任务接续、代理配置或目标平台运行。交叉编译不能代替目标平台运行，密码学组合也尚无独立审计。

## 公网联测（2026-09-22）

用户报告已部署自有域名上的 Relay（此处记为 `https://<你的域名>/`，实际值不写入仓库）。本机 DNS 解析正常；使用系统证书验证的 HTTPS 请求访问 `/healthz` 返回 200。运行 `python3 scripts/remote_smoke.py --relay https://<你的域名>` 成功：`queued`、接收文件字节与 SHA-256、`delivered` 回执、旧码拒绝均通过，脚本未打印接收码。另一次公网传输中发送进程先正常退出，接收进程随后仍领取成功，文件字节与 SHA-256 一致。

这些证据证明当前 Mac 客户端经过该域名的 HTTPS 入口可完成 0.2 协议文件交付；没有读取服务器 Supervisor 状态、进程路径、Nginx 配置或证书文件，也没有验证两台真实设备和两端 Agent 的任务接续。`/healthz` 本身不能证明服务进程身份。

## 跨 Agent 人工确认试验（2026-09-22）

17:55:43（Asia/Shanghai）由本机 jand 发送无敏感信息的 `jand-claude-approval-test.md`，Relay 返回 `queued`，源文件 SHA-256 为 `900444fce40b0e481bbead3e78b41569066ca9de37d070a4e76fcdeaaf1a3368`，大小 3009 字节。发送端没有使用 `--wait`，因此没有产生 `delivered` CLI 事件。

用户转来 Claude 的返回接收码。本机 jand 将 Claude 回执保存在本地忽略目录 `received/claude-return/`：`saved` 事件给出 SHA-256 `0b6a636148050326c2f9b0371ac73c9a79c788a8cf93fd0763ecb27433df8710`、2174 字节和 `requires_user_approval=true`，落盘文件的 SHA-256 与之相同。回执文件自述 Claude 所收包的 SHA-256、大小与发送端源文件一致，并自述先向本地用户征得同意后才写回执；用户随后确认 Claude 确实先摘要并询问，获明确同意后才创建文件。Codex 未独立读取 Claude 会话，人工确认顺序以用户验收为准。

本机另用原发送码派生状态令牌查询 Relay 回执，HTTP 200，回执与按源文件 SHA-256、大小派生的预期令牌常量时间比较相等。这证明接收端提交了匹配的保存回执；它仍不等于发送端 CLI 曾输出 `delivered`。回执指出 `requires_user_approval=true` 为固定策略提示、不是运行时批准检测；已在源码注释、README、HTTP 设计和包内 QUICKSTART 中写明。

## 联测前加固（2026-09-23）

本轮修改 Relay 与客户端的可观测性和失败提示，版本升为 0.2.1-dev。线协议不变，已发布的 v0.2.0-dev 客户端可继续连接新 Relay。

- 过期日志：此前会话由 TTL 定时器删除时不写日志，`session expired` 只在后续请求顺带清理时才可能出现。探针以 50ms TTL 上传后不再发请求，日志只有 `stored`；修复后新增测试确认该行出现。
- 上传超时：此前 Go 服务只限制请求头读取时间，慢速上传可长期占用 8 个上传名额。现在每个上传体限时 2 分钟，超时返回 408 并关闭连接。首版实现在超时后清除了读截止时间，服务器随后试图读完剩余请求体而一直阻塞，408 发不出去；测试暴露后改为只在完整读取后清除。新增测试以单名额 Relay 和一个只发半截请求体的连接，确认名额先被占住、超时后收到 408、名额随即释放。
- 失败提示：发送端被拒时附带 Relay 返回的原因（如 `HTTP 503: relay full`）；领取失败提示码为一次性，下载中途断开时说明码已作废。
- `jand send --help` 与 `-h` 输出本程序用法并以 0 退出；未知参数输出用法并以 2 退出。
- `go.mod` 由 `go 1.26.7` 降为 `go 1.24`（`slog.DiscardHandler` 所需的最低版本）。以 Go 1.24.0 工具链实际运行 `go vet` 与 `go test -race ./...` 通过。

本机以 Go 1.26.7 运行 `go vet ./...`、`go test -count=1 -race ./...` 和 `make smoke` 均通过。编译后的二进制接本机 Relay（`--max-sessions 1`）实测：`send --help` 显示用法；第二次发送报 `relay rejected transfer: HTTP 503: relay full`；无效码领取报一次性提示；`/healthz` 返回 `uptime_seconds`；Relay 日志依次出现 `stored`、`relay full`（附水位）、`claim denied`。服务器上尚未部署本轮版本。

## 0.3.0：Agent 对话

新增 `send --chat` 与 `chat join/decline/send/recv/close`，Relay 新增 `/v1/chats/` 接口，设计见 CHAT_DESIGN.md。`/v1/handoffs/` 线协议不变。

- `go test -race ./...`、`go vet ./...` 通过，覆盖完整对话、拒绝、座位锁定、伪造/重放/篡改消息丢弃、各类超时、长轮询唤醒、等待者上限、消息上限与内存计数归零。
- `make smoke`：原三进程交接，加 `chat_smoke.py` 真实进程对话；后台 `recv --wait` 在对方发消息后约 0.03 秒退出。
- `compat_smoke.py` 以 main 上的 0.2.1-dev 构建为旧客户端：旧→旧、旧→新、新→旧、新→新四种组合经 0.3.0 Relay 均收到 `delivered`；新客户端 `--chat` 发给旧接收端时按普通交接保存。
- 尚未验证：两个真实 Claude Code 会话之间由后台 `recv` 退出唤起 Agent 的完整链路。
- 兼容测试中发现一个 0.2.x 就存在的问题：约 1/64 的接收码以 `-` 开头，会被命令行当成选项而报错。0.3.0 生成接收码时不再产生这种码，接收端遇到旧版发来的这种码也能正确识别。0.2.x 接收端如果碰到，在码前加 `--` 即可，例如 `jand --relay URL -- '-abc…'`。
- 两个真实 Claude Code 会话经公网 Relay 对话一轮（5 条消息，发起方 close）：发起方后台 `recv --wait` 依次在 opened、joined 和三条消息到达时退出并唤起 Agent，全程无人工转述。接收方领取前在 auto mode 下被安全分类器拦截（「Code from External」），由用户手动领取后继续。对话中接收方冷读文档，指出「接收码一次性」与「join 仍用该码」自相矛盾等问题。

## 0.3.1：目标与检查点（按上述联测结论修改）

- 对话必须带目标（`--goal`），每个目标有消息预算（`--budget`，默认 40，最大 200）。Agent 判断达成时 `chat checkpoint`；预算用完时 Relay 强制检查点。暂停期间不转发消息，需一方 `propose`、另一方 `accept` 才恢复，双方各自征得用户同意。任一方仍可随时 close。
- join 可安全重试：同一邀请令牌加已锁定的 guest 令牌再 join 返回成功；客户端只在 400/404/409/410 时删除本地状态，网关 5xx 时保留。
- 接收方 `saved` 事件不再带对话 ID，改带 `goal` 与 `budget`；join 误传 chat ID 时直接提示需要接收码。
- 修复重写中引入、被测试发现的问题：对方加入前 Relay 曾接受发起方的消息。
- AGENT.md 修正「一次性」表述，补充带邀请的 `saved` 示例、目标写法、检查点规则与宿主权限说明。
- 目标放在包头元数据内，受 0.2.x 的 2048 字节上限约束（目标限 1 KiB），因此旧接收端仍能按普通交接保存。
- `go test -race`、`go vet`、`make smoke`（含检查点流程）、`compat_smoke.py` 通过。

## 0.3.2：协作而非聊天

依据 0.3.1 公网协作实测（待办应用，前后端两个 Claude Code 会话，约 7 分钟、6 条消息）暴露的问题修改。

- 各自报告完成：新增 `chat done` 与 `/v1/chats/{room}/done`。一方完成只通知对方、不暂停；双方都完成时 Relay 发出 `checkpoint`（`reason=all_done`）。原 `checkpoint` 保留为「立即暂停、请用户决定」。实测中「双方各自 checkpoint」会被 409 拒绝的问题由此解决。
- 消息类型与编号：消息明文改为加密信封 `{kind, reply_to, text}`，类型为 note/progress/request/reply/delivery，编号如 `h3`、`g2` 由角色与计数算出；`reply` 必须带 `reply_to`。实测中「答复被标成进展」「消息交叉无法对应」由此有了协议层依据。
- 按类型唤醒：`recv --wake KINDS` 在客户端暂缓非唤醒类型的消息（随游标持久化），遇到唤醒类型、非消息事件或超时再按序输出。实测中 3 次仅为进展的唤醒可以避免。
- AGENT.md 新增「协作」一节（干活为主、卡住发请求、共享资源归属、不越界），交接模板新增「Collaboration」一节；接收码放在回复末尾。
- `go test -race`、`go vet`、`make smoke`（新增进展暂缓、reply_to、双方完成暂停）通过。
- 仍未验证：Agent 正在连续调用工具时被后台 `recv` 唤醒的时机。

## 0.4.0：消息交叉、逐字核对与暂停收尾

依据 0.3.2 与 Jared 侧 Agent 的跨机器认知对齐实测（6 分钟、15 条消息，终稿逐字一致，但暴露三个问题）修改。按商定规则，对话协议不兼容即升次版本号。

- 消息交叉（实测 4 次，对方每次都在审阅已被替换的旧版本）：发送 reply、request、delivery 前检查对方未读的 request/reply/delivery/done/proposal，有则拒绝（退出码 5），`--anyway` 可越过；progress 和 note 不拦，以免与 `--wake` 冲突。delivery 可带 `--supersedes`，回复旧版本会被拒绝或被标记为 `stale`。
- 逐字核对失败（两边哈希只差末尾换行）：`chat send --file` 改为按原字节发送；标准输入仍去掉末尾换行。
- 双方 done 后连补充说明都发不出：暂停期间每方可发最多 3 条 reply 或 note，不计预算，带 `after_pause` 标记；request 与 delivery 仍被拒绝。Relay 新增配置 `ChatPauseNotes`。
- 修改中发现并修正的设计冲突：最初把所有未读消息都算作阻塞，会让使用 `--wake` 的一方永远发不出回复。
- `go test -race`、`go vet`、`make smoke`（新增：未读拦截与退出码 5、supersedes、暂停后收尾消息）、`compat_smoke.py` 通过。

## 0.4.1：配置化（四层分工）

按 CONFIG_DESIGN.md 实施，毛仔决定版本留在 0.4.x，因此协作流程做成与 0.4.0 兼容。

- 第 0 层：安全底线不可配置（加密、座位锁、用户同意、不可信标记、Relay 强制的暂停与计数、先读再回）。
- 第 1 层：`jand relay --config`（或 `JAND_RELAY_CONFIG`）读取 JSON 配置，覆盖全部 15 个参数；未知字段、越界值直接拒绝；`--print-config` 显示最终生效值；命令行参数优先。访问令牌只预留字段，非空即报错。
- 第 2 层：`$JAND_HOME/config.json` 提供默认 Relay 地址、输出目录、默认预算、默认 `--wake`、默认流程；优先级为命令行参数 > 环境变量 > 配置文件 > 内置默认；`jand config` 显示生效值与来源。
- 第 3 层：`send --chat --workflow`。流程规定 done_rule（all/any）、pause_notes、budget、允许的消息类型、角色和说明，封入加密章程随建房上传；Relay 执行完成规则、收尾消息条数和预算；接收方领取时核对章程与 Relay 实际执行的规则一致，`saved` 带上核对后的流程和实际预算，核对失败则提示不要加入，join 时再核对一次。任一方完成即暂停（`any_done`）的对话拒绝旧客户端加入。
- 兼容：不带流程时与 0.4.0 行为一致；0.4.0 Relay 上默认流程可用，自定义流程明确报错并撤回邀请（测试用只缺 charter 接口的代理模拟）。
- 测试：Relay 配置解析与回读；流程规则执行（任一方完成即暂停、旧客户端被拒、每个对话的收尾消息条数、超过 Relay 上限被拒、章程接口鉴权）；端到端自定义流程；Relay 篡改参数被接收方识破；旧 Relay 行为；本地配置的优先级与字段拼错；仓库示例流程合法。`make smoke` 新增用仓库的 review 流程走完整对话。
- 实现中发现并修正：接收端在旧 Relay 上取不到章程时，曾误报「核对失败、不要加入」，改为按默认流程处理。
- 文档整体更新时补测：用真实的 0.4.0 程序（`9d0e9ca` 构建）与 0.4.1 程序在 0.4.1 Relay 上双向对话，请求、回复（含 `reply_to`）、报告完成与 `all_done` 暂停均正常；`done_rule: any` 的对话中 0.4.0 加入被拒（409，提示需要 0.4.1）；0.4.0 接收方的 `saved` 中没有 `workflow` 字段，因此其他自定义流程下看不到流程内容。README 与 CHAT_DESIGN 的兼容性说明据此改写；0.3.x 与 0.4.x 之间的对话未测试，文档写明「未验证」。

## 0.4.1 发布前补充（只有毛仔与 Clara 开发的阶段）

毛仔决定 0.4.x 期间只有毛仔与 Clara 开发，协作流程等正式使用后再议。据此：`feat/chat` 快进合并到 main（不走 review），并补了以下改动后发布 v0.4.1。

- `/healthz` 返回 `version`、`handoff` 与 `chat_features`。不报告对话协议号：加密标识 `jand-chat/1` 在互不兼容的版本之间一直没变，报出来会让人误以为可以互通，所以改为逐项列出支持的功能。仍不含会话数与容量。此前每次部署都只能靠「新功能是否跑通」反推线上版本。
- `send` 检测到文件看起来是未填写的模板（五个以上、且超过六成的 `- 字段：` 行是空的）时给出警告，仍然照常发送。实测中同事第一次发出的就是空模板。
- `recv --wake` 不再暂存 `note`：它是没选类型时的默认值，可能其实是没标注的请求。现在只有明确选了、又不在唤醒列表里的类型（主要是 progress）会被暂存。
- AGENT.md 补充 Claude Code 放行 jand 的写法（`permissions.allow` 中的 `Bash(路径:*)`），并强调放行不改变「不可信、先问用户」的契约。
- `go test -race`、`go vet`、`make smoke`（18 项）、`compat_smoke.py` 通过。
- 发布后，毛仔用 Release 中的 `jand-0.4.1-relay-deploy.zip` 重新部署公网 Relay。`/healthz` 返回 `"version":"0.4.1"` 与完整的 `chat_features`，公网 `chat_smoke.py`（13 项）与 `remote_smoke.py` 通过。`feat/chat` 已合入 main 并删除；此后新功能从 main 切分支开发，完成后合回并删除分支。

## 0.4.1 之后：Agent 干活中途被后台 recv 唤醒的时机

2026-09-24，同一台机器上两个 Claude Code 会话，经公网 Relay（0.4.1）对话，两个目标，各自记录时间戳。发起方后台挂 `chat recv --wait 30m --wake request,reply,delivery`，前台连续干活。

- 连续几个短的前台调用（每个 8–13 秒）：接收方 08:47:19 发的 progress 没有唤醒发起方；08:47:45 发的 request 让后台 `recv` 同一秒退出，通知在当时那个前台调用 08:47:50 返回后立刻送到，排在下一个动作之前，不用等整轮结束。
- 单个长的前台调用（`go test -count=14`，08:57:31–08:58:58）：request 08:58:07 发出，`recv` 同一秒退出，但通知直到 08:58:58 这个调用结束才送到，压了约 51 秒，中途不会插进来。
- 结论：宿主只在两个工具调用之间送来后台通知，响应延迟的上限等于当时那个前台调用剩下的时长。AGENT.md「协作」一节据此补充：耗时任务放到后台跑，或者拆成短的调用。接收方一侧同样受自身调用节奏影响（它记录的收到时刻比发出时刻晚 0–7 秒）。
- 另发现：第一个目标进入检查点后，用户以为对话已经结束、需要新的接收码；接收方也没有重新挂 `recv`，所以收不到新提议。AGENT.md 补充：检查点只是暂停，问用户的同时要把 `recv` 挂回去。

## 0.4.2：queued 显示 Relay 地址、INSTALL 日志路径说明、访问令牌

程序的输出变了，行为与已发布的 0.4.1 不同，所以升为 0.4.2（兼容改动升补丁号）。

- `queued` 事件新增 `relay` 字段；文本输出多一行 `Relay:`，写明接收方必须用同一个 Relay。此前发送方看不到自己实际用的是哪个 Relay（命令行、`JAND_RELAY`、配置文件都可能设置），对方用错 Relay 时只会看到「码无效」。只是加了一个字段，线协议和对话协议都没变。
- INSTALL 写明 `/var/log/jand-relay.log` 只是示例配置里的值，给出查实际路径的命令；升级一节改用 `supervisorctl tail jand-relay`，不再依赖日志路径。
- `go vet`、`go test -race`、`make smoke`（18 项）通过；本机 Relay 手动发送，文本和 `--json` 两种输出都带上了 Relay 地址。
- 兼容：`compat_smoke.py` 分别以真实 0.4.1（main `7a4d0b1` 构建）与 v0.2.1-dev 为旧客户端，四种组合的交接都收到 `delivered`。另用脚本在 0.4.2 Relay 上让 0.4.2 与 0.4.1 互为发起方和接收方，走完邀请、加入、request 与 reply、双方 done、`all_done` 检查点，两个方向都正常。
- `compat_smoke.py` 修正：最后一项原本假定旧接收端认不出对话邀请，这只对 0.3 之前的客户端成立；拿 0.4.1 当旧版时会误报失败。现在按旧版本号区分，0.3 之前仍要求按普通交接保存，之后的要求识别出邀请。
- 访问令牌（可选，防止 Relay 被当作匿名中转）：Relay 配置 `access.tokens_sha256` 后，新建交接或对话必须带 `X-Jand-Access`；领取、加入、对话消息不检查。客户端从 `JAND_ACCESS_TOKEN` 或 `access_token_file` 读取令牌，只在发送时带上。`/healthz` 新增 `access`，启动日志新增 `access=`。设计取舍见 CONFIG_DESIGN「实现记录（0.4.2）」。
- 访问令牌测试：配置解析与 `--print-config` 回读；不带令牌或令牌错误时交接与建房都返回 401，带对令牌可以发送，接收方不带令牌照常领取；客户端令牌来源的优先级与空文件报错；`/healthz` 默认 `access:false`。把 Relay 的检查改成永远放行后端到端测试失败，确认测试确实覆盖了检查。真实程序实测：本机 Relay 开启令牌，不带令牌发送得到 `HTTP 401: this relay requires an access token; set JAND_ACCESS_TOKEN`，环境变量与令牌文件两种方式都能发交接和对话，接收方不带令牌领取成功；Relay 日志只有一行 WARN，不含令牌。
- 检查点提示写进程序输出：唤醒时机实测中，用户和接收方 Agent 都把检查点当成了对话结束。只改 AGENT.md 不够，Agent 用 `--json` 看不到文本输出。现在每个 `checkpoint` 事件（自己发起、对方发起、Relay 触发）都带 `message`：对话只是暂停，结束用 `chat close`，继续用 `chat propose`，不需要新码，其间保持 `recv` 才能收到对方的提议；文本输出打印这段话，再附上带对话 ID 的两条命令。单元测试覆盖对方发起与 Relay 触发两种，`chat_smoke` 检查 `all_done` 检查点的 `message`。

## 0.4.3（开发中）：看得见对话经过

毛仔反馈：对话全程由 Agent 在命令行里进行，人只能靠 Agent 事后的总结知道发生了什么。决定不做 GUI，而是从本机已有的对话记录生成可读的视图；数据只用本地文件，不引入数据库，Relay 继续不保存任何历史。

- 记录补全：发起方建房后写 `started`，接收方加入后写 `joined`，都带目标、预算和流程；发起方收到对方领取、加入时也记一行；`resumed` 带上新预算。建房后交接包没发出去（撤回）时，连同记录一起删除。
- `jand chat list`：扫描 `$JAND_HOME/chats/`，列出每个对话的状态（waiting / active / paused / ended，旧对话没有记录的为 unknown，用状态文件时间代替）、消息数、最后活动时间和当前目标。
- `jand chat log [--follow] <chat>`：终端时间线，己方标 `*`，reply 标出回复的是哪条；`--follow` 每秒检查新增记录，对话结束（closed、expired、decline）时退出。
- `jand chat view <chat>`：用 `html/template` 生成独立 HTML（CSP `default-src 'none'`，无脚本、无外部资源，文件权限 0600），默认写在对话目录并用系统浏览器打开；`--out` 指定位置，`--no-open` 不打开。己方消息靠右、对方靠左，Relay 事件居中；消息类型着色，「answers g2」可点回原消息；浅色与深色两套配色。
- 测试：对话从 waiting 到 active、paused、ended 的状态变化；接收方记录以带目标的 `joined` 开头；列表的 JSON 中不含对话令牌和密钥；页面把对方写的 `<script>`、`<img onerror>` 转义输出；`log --follow` 在记录出现 closed 后退出。用真实的 0.4.1 公网对话 `cb7fc191` 生成页面，以无头 Chrome 截图检查浅色与深色显示。
- 发现并修正：`compat_smoke.py` 没有为测试单独设置 `JAND_HOME`，会把测试对话的状态写进使用者真实的对话目录。现在改用临时目录并在结束时删除；修正后跑一遍，真实目录的文件数不变。
- `jand help agent`：AGENT.md 嵌入程序（`docs` 包），运行时替换为本机程序的实际路径（PATH 上找到的正是自己时写 `jand`）和配置的 Relay，去掉只适用于发行包的段落。起初做成独立命令 `jand guide`，毛仔指出应走常规的 `help`；按 `git help <主题>` 的惯例改为 `help` 的主题，`jand help` 仍是简短用法。
- 等消息的两种方式：AGENT.md 中 `mode:background` 与 `mode:poll` 两段，由本机配置 `chat.wake_mode` 选择；发行包（`release.py`）两段都保留。毛仔指出对方不一定用 Claude Code，而且有的 Agent 没有后台唤醒能力；默认推荐仍是 Claude Code。
- `jand chat watch`：Relay 会删除 `after` 及之前的事件，所以提醒器若跑到 Agent 前面，会让 Agent 丢消息。实现上每次都从 Agent 的游标文件读取位置、用 `wait=0` 查询（不占每个对话 4 个长轮询名额），本地去重，不写记录、不改游标。测试：提醒器看到对方的 request 后，Agent 的 `recv` 仍然收到同一条；多次轮询不重复提醒；对话关闭后退出。
- `chat view` 按毛仔的意见改版：中文；「我方 Agent / 对方 Agent / Relay」用颜色与标签区分；每条写明动作；请求追踪表、回复摘录、已回应/未回应、交付取代关系；按目标分段并写明结果。仍用无头 Chrome 截图检查。
- `jand setup`：给人用的首次配置（中文提示，可用参数免交互，可重复运行）。依次：Relay 地址并查 `/healthz`（版本不同时提示对话可能不兼容）；Relay 要求令牌时取令牌（`--token-file`、`JAND_ACCESS_TOKEN`、已配置的文件或当场输入，输入的令牌存成 `$JAND_HOME/access-token`，权限 600，配置只记路径）；选择 Agent（检测 `~/.claude`、`~/.codex`，默认选 Claude Code）：Claude Code 安装 `~/.claude/skills/jand/SKILL.md`（只指向 `jand help agent`），询问后在 `~/.claude/settings.json` 加 `Bash(<jand>:*)`（先备份，保留其他设置，已有则不改）；Codex 在 `~/.codex/AGENTS.md` 末尾加一段带标记的说明（重复运行替换，不重复追加）；其他 Agent 打印要贴的那句话；选择等消息的方式；写配置；自检（经配置的 Relay 给自己发一个文件、领取、比对内容、等回执）。
- setup 测试（临时 HOME 与 JAND_HOME、要求令牌的测试 Relay）：免交互跑两遍，配置、skill、Claude 放行（原有设置保留、只备份一次）、Codex 说明（原内容不动、只有一段）都正确，自检通过；交互输入令牌时令牌文件为 600、`config.json` 中没有令牌本身，回答「否」的 Agent 不被改动；Relay 连不上时失败且不写配置。另用编译好的程序在临时 HOME 中接本机开启令牌的 Relay 真实跑一遍交互流程，全部步骤通过。
- 安装脚本 `install.sh`（macOS/Linux）与 `install.ps1`（Windows）：从 GitHub Release 下载本机对应的包和 `SHA256SUMS.txt`，校验后安装，再运行 `jand setup`（`curl | sh` 时从 `/dev/tty` 读回答）。所有发行版都是预发布版，而 GitHub 的 `/releases/latest` 会跳过预发布版，所以改用版本列表取最新的一个。`install.sh` 实测（用 GitHub 上真实的 v0.4.1）：指定版本与自动取最新版都安装成功；模拟校验文件缺条目、哈希不一致两种情况都中止且不创建安装目录；以管道方式运行（同 `curl | sh`）正常；shellcheck 无告警。实测中发现变量后紧跟中文全角标点（如 `$version（`）时 bash 会把全角字符当作变量名的一部分而报错，已统一改为 `${变量}` 写法。**`install.ps1` 未实际运行过**（本机没有 PowerShell），需要在 Windows 上验证，尤其是旧版控制台对中文输出的显示。
- 发行包的 QUICKSTART 按「傻瓜化」重写：先 `jand setup`，然后直接跟 Agent 说要交出去、收进来还是协作，再告诉人怎么用 `view` / `watch` 看过程；命令细节交给 `jand help agent`。改写时发现并去掉一句错话：原稿说「这个包默认用某 Relay，直接回车即可」，但包里的 Relay 地址并不会成为 `setup` 的默认值。`release.py` 实际打包 6 个归档成功，Linux 包内 QUICKSTART 与 AGENT.md 渲染正确、无残留标记。INSTALL 标题去掉过时的「0.2」。

### 冷启动实测（Ubuntu 22.04 x86_64 上的 Claude Code，2026-09-24）

毛仔在内网 Ubuntu 服务器上照 QUICKSTART 解压、运行 `setup`（Relay 用公网 0.4.1），然后在全新的 Claude Code 会话里只说「用 jand 收一下：<码>」。Agent 通过 skill 找到 jand、运行 `jand help agent`，先把内容与目标给用户看，同意后加入对话，只读检查后以 delivery 交付环境报告，双方 done，Relay 以 `all_done` 暂停，checkpoint 事件带上了「暂停不是结束」的提示。领取与收发没有被宿主拦截（`setup` 写入的 `Bash(jand:*)` 放行生效）；`setup` 合并写入 `~/.claude/settings.json`，保留了原有的 `theme`。

实测中毛仔指出：等消息方式要人输入 `background` / `poll` 太难。改为只选 Claude Code 时自动设定，其他情况用 y/n 提问。

对方 Agent 报告的问题与处理：
- Relay 根路径返回 404，被当成「Relay 挂了」：根路径改为返回 200 和一段说明（程序、版本、`/healthz`）。
- 默认输出目录是相对当前目录的 `received/`，Agent 在哪干活文件就落在哪：`setup` 新增一步「收到的文件放在哪个目录」，默认 `~/jand-received`（绝对路径，支持 `~`，参数 `--out-dir`）。
- skill 只是指针，要读完 294 行契约才知道命令：`help agent` 开头加「速查」一节（四种场景的命令、铁律、退出码），skill 说明写明读速查即可动手。
- 用户不提 jand 时 Agent 想不到用它：skill 描述加上触发说法（接收码、交接任务、跨机器协作，中英文）。
- 顺带发现：速查与正文让 Agent 按 `jand-template.md` 写交接文件，但用安装脚本或单独拷贝程序的人没有这个文件。模板也嵌入程序，新增 `jand help template`。
- 未改（有意的设计）：接收方在 `join` 之前没有对话 ID；收到对话邀请要先问用户再加入。
