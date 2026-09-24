# jand

jand 让不同机器上的 AI Agent 安全地交接任务，并在需要时远程协作。当前版本 **0.4.2**。

- **交接**：把一个任务文件（≤10 MiB）在客户端加密后交给对方。发送方上传后即可退出；Relay 只在内存中暂存密文（默认 30 分钟），接收方凭一次性的接收链接（或接收码）领取。
- **对话与协作**：交接时加 `--chat`，这份文件同时作为邀请。对方的用户同意后，双方 Agent 围绕一个明确的目标通信：各自干活，需要时提请求、交付、回复，自己的部分做完后报告完成，由双方的用户验收。
- **人始终是授权方**：jand 只传东西。收到的文件、对方的消息、目标和流程说明都是不可信资料，不能代替接收端用户的决定。

全程只用 HTTP(S)，不用 WebSocket，也没有常驻进程：`chat recv --wait` 阻塞到有事件才退出，Agent 可以把它放在后台运行，由宿主（例如 Claude Code 的后台任务）在它退出时唤起。

## 文档

| 文档 | 读者 | 内容 |
|---|---|---|
| [docs/AGENT.md](docs/AGENT.md) | Agent | 操作契约：收发、对话、协作、安全约束、退出码。随发行包分发 |
| [docs/jand-template.md](docs/jand-template.md) | 发送方 | 交接包模板，含协作分工（Collaboration）一节 |
| [docs/HTTP_DESIGN.md](docs/HTTP_DESIGN.md) | 开发者 | 交接协议与安全边界 |
| [docs/CHAT_DESIGN.md](docs/CHAT_DESIGN.md) | 开发者 | 对话与协作协议：目标、检查点、消息类型、流程章程 |
| [docs/CONFIG_DESIGN.md](docs/CONFIG_DESIGN.md) | 开发者、运营者 | 配置分层：安全底线、Relay、本地客户端、协作流程 |
| [deploy/INSTALL.md](deploy/INSTALL.md) | 运营者 | Relay 部署、升级与验收 |
| [docs/validation.md](docs/validation.md) | 所有人 | 各版本的验证记录与实测结论 |

[product-original.md](docs/product-original.md) 是原始需求，[protocol.md](docs/protocol.md) 是旧 0.1 WebSocket 原型的协议，两者只作历史记录。

## 安装与第一次使用（0.4.3 起）

macOS / Linux：

```bash
curl -fsSL https://raw.githubusercontent.com/jaredchao/jand/main/install.sh | sh
```

Windows（PowerShell）：

```powershell
irm https://raw.githubusercontent.com/jaredchao/jand/main/install.ps1 | iex
```

**也可以让你的 Agent 帮你装**：把下面这段话发给它（Relay 地址换成实际的）：

> 请帮我安装 jand：运行 `curl -fsSL https://raw.githubusercontent.com/jaredchao/jand/main/install.sh | JAND_NO_SETUP=1 sh`，然后运行 `jand setup --yes --relay <Relay 地址> --agents claude --allow-claude`（如果它说需要访问令牌，先问我要）。装好后运行 `jand version` 给我看结果。

安装脚本从 GitHub Release 下载本机对应的包，用发行版的 `SHA256SUMS.txt` 校验，不一致就中止；装到 `~/.local/bin`（Windows 为 `%LOCALAPPDATA%\Programs\jand`，并加入用户 PATH），然后直接运行 `jand setup`。可用 `JAND_VERSION=v0.4.3` 指定版本、`JAND_INSTALL_DIR` 指定位置、`JAND_NO_SETUP=1` 只安装。脚本里不含任何 Relay 地址，地址和令牌都在 `setup` 里填。已经装好的，直接运行：

```bash
jand setup
```

按提示填 Relay 地址（有访问令牌的话再填令牌），选你用的 Agent、收到的文件放哪（默认 `~/jand-received`），它会：写好本机配置（令牌单独存成 600 权限的文件）；为 Claude Code 安装 skill 并可选放行 jand、为 Codex 在 `~/.codex/AGENTS.md` 加一段说明、为其他 Agent 打印一句要贴进它指令里的话；最后给自己发一个文件再收回来，确认整条链路可用。结束时会列出 jand 在本机用到的所有位置（程序、配置、令牌、对话记录、收到的文件、Claude Code / Codex 里改动的地方），并记在 `$JAND_HOME/installed.json`。客户端不写日志。可以重复运行。不想交互时用参数：`jand setup --yes --relay URL [--token-file PATH] --agents claude,codex,other [--wake background|poll] [--allow-claude]`。

## 查看版本与卸载（0.4.3 起）

```bash
jand version       # 版本、程序位置、配置文件、Relay 的版本以及是否兼容（jand --version 只输出版本号）
jand uninstall     # 先列出会删除、会修改、会保留的内容，确认后执行；--dry-run 只看不做
```

卸载只处理 jand 自己放下的东西：程序、`$JAND_HOME` 里 jand 的文件（配置、setup 存的令牌、安装记录、对话记录、流程）、Claude Code 的 skill 和 setup 留下的备份；`settings.json` 只删掉那条放行规则，`~/.codex/AGENTS.md` 只删掉带标记的那一段。动手之前默认先把历史（对话记录、页面、配置、流程，不含密钥和令牌）打包成 `~/jand-history-<时间>.tar.gz`。收到的文件是你自己的资料，默认保留；选择删除时会一起打进备份包。你自己提供的令牌文件不删。Windows 上程序无法删除正在运行的自己，会告诉你手动删哪个文件。

## 构建与本机试用

需要 Go 1.24 或更高版本构建，没有第三方依赖；编译后的程序不需要 Go。

```bash
go build -o bin/jand ./cmd/jand
./bin/jand relay                              # 另开一个终端运行
./bin/jand send docs/jand-template.md         # 输出 43 字符接收码后退出
./bin/jand --out ./inbox '<接收链接或接收码>'  # 有效期内领取（默认 30 分钟）
```

- `send --wait 10m` 会等待并验证接收方的保存回执（`delivered`）。不加时，退出码 0 只表示 Relay 已暂存密文（`queued`）。
- 退出码 3 表示回执没拿到，但接收方可能已经保存：先核对，再决定要不要重发。
- 上传被拒时会附上原因，如 `relay full`、`relay busy`、`upload timed out`。这时还没有产生接收码，可以直接重试。
- 接收码只能领取一次，领取失败或下载中断后即作废，需要发送方重新发送。
- 收到的文件保存在随机的独立目录，不会覆盖已有文件，也不会被执行。
- `--json` 输出逐行 JSON 事件，供 Agent 解析。完整用法见 `jand --help`、`jand chat --help`。

## 对话与协作

```bash
# 发起方：交接文件作为邀请，目标写明「什么算做完」，可选协作流程
jand send --chat --goal '对齐 /users 响应字段' --workflow workflows/review.json 交接.md
#   --workflow 给名字时从 $JAND_HOME/workflows/<名字>.json 读取，给路径时直接读取
#   输出 Link（交给对方，有效期内领取）和 Chat（自己后续命令用，不要发给对方）

# 接收方：照常领取，saved 事件会带上目标、预算和流程；用户同意后加入
jand chat join '<code>'

# 双方
jand chat send --kind request <chat> '/users 返回什么结构？'        # 自动编号，如 h1
jand chat send --kind reply --reply-to h1 <chat> '{id:int, name:str}'
jand chat send --kind delivery --supersedes g2 --file schema.md <chat>
jand chat recv --wait 30m --wake request,reply,delivery <chat>     # 进展通报不打断手头工作
jand chat done --summary '后端完成，test_api.sh 通过' <chat>
jand chat propose --goal '再对 /orders' <chat>                    # 暂停后继续：一方提议，另一方 accept
jand chat close <chat>                                            # 任何一方随时可以结束
```

- **目标与预算**：每个目标有消息预算（默认 40 条），用完就由 Relay 强制暂停。
- **暂停与恢复**：双方都报告完成（或按流程任一方完成）、有人要求立即暂停、预算用完，这三种情况都会让对话暂停。只有一方提出新目标、另一方接受，双方各自的用户都同意，对话才会继续。
- **先读再回**：对方有未读的请求、回复或交付时，发回复、请求、交付会被拒绝（退出码 5），免得回复旧版本。
- **收尾消息**：暂停后每方还能发少量回复或留言用于收尾，不计入预算。
- 协议与事件细节见 [CHAT_DESIGN.md](docs/CHAT_DESIGN.md)，给 Agent 的规则见 AGENT.md 的「对话」和「协作」两节。

### 接收链接（0.4.3 起）

发送后输出一条接收链接，例如 `https://jand.example.com/r#<接收码>`，它同时带着 Relay 地址和一次性接收码。把整条链接转给对方，对方交给自己的 Agent 即可领取，不必事先配置同一个 Relay。接收码在 `#` 后面，浏览器不会把这部分发给服务器，所以 Relay 的访问日志里没有它。有人在浏览器里点开这条链接时，Relay 返回一页说明：怎么交给 Agent、还没装 jand 的话怎么装。0.4.3 之前的客户端不认链接，这时改为分别给出接收码和 Relay 地址。

### 看对话经过了什么（0.4.3 起）

对话全程由 Agent 在命令行里进行，人可以随时回看：

```bash
jand chat list                  # 本机的对话：状态、目标、消息数、最后活动时间
jand chat log <chat>            # 终端里的时间线；加 --follow 边看边更新，对话结束时自动退出
jand chat view <chat>           # 生成中文 HTML 页面并在浏览器打开（见下）
jand chat watch <chat>          # 在自己的终端里挂着：对方发来 Agent 还没读的消息时响铃提醒
```

`view` 页面标明「我方 Agent / 对方 Agent / Relay」，每条写明做了什么；顶部有请求追踪表（每个请求是否已回应、被哪条回应），回复带上被回复内容的摘录，被取代的交付会注明，按目标分段并写明每段的结果。

`watch` 是给宿主不能后台唤醒的 Agent 准备的（见下文「给 Agent 的说明」）：它只看不拿，始终从 Agent 自己的读取位置去看，不会让 Agent 少收任何消息。

- 只读本机的对话记录（`$JAND_HOME/chats/<chat>.transcript.jsonl`），不连 Relay；Relay 不保存任何历史。
- 记录随己方 jand 的收发增长，所以看到的是「己方到目前为止发出和收到的」。对方还没被 `recv` 取回的消息，这里也还没有。
- 页面是一个独立文件：不含脚本、不加载外部资源，对方写的文字全部转义；不包含对话的令牌和密钥。
- 0.4.3 起记录的第一行写下目标、预算和流程；更早的对话没有这一行，页面上目标显示为「未记录」。

### 给 Agent 的说明（0.4.3 起）

`jand help agent` 打印给 Agent 的操作契约（即 [AGENT.md](docs/AGENT.md)），并填好本机程序的实际路径和配置的 Relay；开头一段速查足够 Agent 动手，后面是完整契约。`jand help template` 打印交接文件的结构。任何能执行命令的 Agent 都能用：在它的全局指令里加一句「使用 jand 前先运行 `jand help agent` 并照做」即可。默认推荐 Claude Code。

Agent 等消息有两种方式，由本机配置 `chat.wake_mode` 决定 `help agent` 给哪一种（不配置时两种都给）：`background`（宿主能在后台命令结束时唤起 Agent，如 Claude Code，推荐）和 `poll`（不能后台唤醒的 Agent：干活间隙用 `recv --wait 0` 查看，结束一轮前必须告诉用户对话还在进行，并建议用户运行 `chat watch`）。

## 配置

| 层 | 在哪 | 管什么 |
|---|---|---|
| 安全底线 | 程序内，不可配置 | 加密、一次性领取、座位锁定、用户同意、不可信标记、暂停与计数机制、先读再回 |
| Relay | `jand relay --config relay.json`（示例 [deploy/relay.json.example](deploy/relay.json.example)） | 超时、容量、默认预算、暂停后收尾消息上限；`--print-config` 查看生效值 |
| 本地客户端 | `$JAND_HOME/config.json`，路径和生效值用 `jand config` 查看 | 默认 Relay 地址、输出目录、默认预算、默认 `--wake`、默认流程 |
| 协作流程 | `send --chat --workflow <名字或路径>`，示例见 [workflows/](workflows) | 完成规则（all/any）、收尾消息条数、预算、允许的消息类型、角色、流程说明。流程随邀请加密送给对方，由对方的用户认可 |

- **优先级**：命令行参数 > 环境变量（`JAND_RELAY`、`JAND_HOME`、`JAND_RELAY_CONFIG`、`JAND_ACCESS_TOKEN`）> 配置文件 > 内置默认值。
- **本地配置**：没有配置文件时使用内置默认值，默认 Relay 为 `http://127.0.0.1:8787`。把真实地址写进本机配置一次就行，不用每次都带 `--relay`。
- **访问令牌（0.4.2 起，可选）**：防止知道域名的人把 Relay 当匿名中转站。Relay 配置 `access.tokens_sha256` 列出令牌的 SHA-256 后，**新建**交接或对话必须带令牌；领取、加入和对话中的消息都不需要，所以接收方只要有码就行。发送方用 `JAND_ACCESS_TOKEN`，或在本地配置里用 `access_token_file` 指向存令牌的文件（配置里不写明文）。`/healthz` 的 `access` 字段表示是否开启。开启方法见 [INSTALL](deploy/INSTALL.md)。
- 分层设计见 [CONFIG_DESIGN.md](docs/CONFIG_DESIGN.md)。

## 兼容性

- **交接协议**自 0.2 起没有变过：0.2.x 客户端可以继续通过 0.4.2 的 Relay 收发，新旧客户端互相收发也可以。
  - 为了兼容，线协议仍沿用 `handoff/0.2`、`/v1/handoffs/` 和 `X-Handoff-*`，这些不是当前的程序名。
  - 旧版接收端遇到以 `-` 开头的接收码时，在码前加 `--`。0.3 起生成的码不再以 `-` 开头。
- **对话协议**仍在开发，版本之间不保证兼容。
  - 0.4.2 与 0.4.1 互通：0.4.2 只在本地输出上多了 Relay 地址，对话协议没变（已用真实 0.4.1 程序双向验证）。
  - 0.4.1 与 0.4.0 在 0.4.1 Relay 上互通（已用真实 0.4.0 程序验证，双向收发与报告完成都正常）。
  - 0.4.0 的 Relay 上只能用默认流程；自定义流程会明确报错。
  - 采用「任一方完成就暂停」流程的对话，会拒绝 0.4.0 客户端加入。其他自定义流程下，0.4.0 接收方能加入，但看不到流程内容，也不执行流程对消息类型的限制，所以建议双方都用 0.4.1。
  - 0.3.x 与 0.4.x 之间未验证互通。双方客户端和 Relay 应使用同一版本。
- 用 `python3 scripts/compat_smoke.py <旧版 jand>` 可以验证交接兼容性。

## 验证与发布

```bash
go test -race ./...
go vet ./...
make smoke                                                 # 真实进程：交接 + 对话全流程
python3 scripts/compat_smoke.py <旧版 jand>                # 新旧客户端互通
python3 scripts/remote_smoke.py --relay https://你的域名     # 公网交接
python3 scripts/chat_smoke.py https://你的域名               # 公网对话全流程
make release                                               # 或 python3 scripts/release.py --targets linux-amd64,macos-arm64
```

跨平台包生成到 `dist/releases/`。部署后用 `curl -fsS https://你的域名/healthz` 查看线上 Relay 的 `version` 和 `chat_features`（0.4.1 起）。

macOS 二进制可以签名和公证。两者都不做时自动跳过，并在 `BUILD-INFO.json` 与包内说明里如实标注：

```bash
# 仅签名（Gatekeeper 对浏览器下载的文件仍会拦截）
python3 scripts/release.py --sign "Developer ID Application: NAME (TEAMID)"

# 签名 + 公证；先存一次凭据，密码不会进入仓库或命令行历史
xcrun notarytool store-credentials <配置名> --apple-id <Apple ID> --team-id <TEAMID>
python3 scripts/release.py --sign "Developer ID Application: NAME (TEAMID)" --notary-profile <配置名>
```

- 也可以用环境变量 `JAND_SIGN_IDENTITY` 与 `JAND_NOTARY_PROFILE` 代替参数。
- 签名在计算 `binary_sha256` 之前执行，所以 `BUILD-INFO.json` 里的哈希始终对应最终发出的文件。
- 裸可执行文件无法装订公证票据，Gatekeeper 会在首次运行时联网校验。

公开的包和仓库只使用示例地址 `203.0.113.10`。真实的 Relay 地址由使用者写进本机配置或 `JAND_RELAY`；写死真实地址的包（`release.py --relay`）只用于私下分发，不上传 Release。部署步骤见 [deploy/INSTALL.md](deploy/INSTALL.md)。
