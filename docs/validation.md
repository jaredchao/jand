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

本轮修改 Relay 与客户端的可观测性和失败提示，线协议不变，已发布的 v0.2.0-dev 客户端可继续连接新 Relay。

- 过期日志：此前会话由 TTL 定时器删除时不写日志，`session expired` 只在后续请求顺带清理时才可能出现。探针以 50ms TTL 上传后不再发请求，日志只有 `stored`；修复后新增测试确认该行出现。
- 上传超时：此前 Go 服务只限制请求头读取时间，慢速上传可长期占用 8 个上传名额。现在每个上传体限时 2 分钟，超时返回 408 并关闭连接。首版实现在超时后清除了读截止时间，服务器随后试图读完剩余请求体而一直阻塞，408 发不出去；测试暴露后改为只在完整读取后清除。新增测试以单名额 Relay 和一个只发半截请求体的连接，确认名额先被占住、超时后收到 408、名额随即释放。
- 失败提示：发送端被拒时附带 Relay 返回的原因（如 `HTTP 503: relay full`）；领取失败提示码为一次性，下载中途断开时说明码已作废。
- `jand send --help` 与 `-h` 输出本程序用法并以 0 退出；未知参数输出用法并以 2 退出。
- `go.mod` 由 `go 1.26.7` 降为 `go 1.24`（`slog.DiscardHandler` 所需的最低版本）。以 Go 1.24.0 工具链实际运行 `go vet` 与 `go test -race ./...` 通过。

本机以 Go 1.26.7 运行 `go vet ./...`、`go test -count=1 -race ./...` 和 `make smoke` 均通过。编译后的二进制接本机 Relay（`--max-sessions 1`）实测：`send --help` 显示用法；第二次发送报 `relay rejected transfer: HTTP 503: relay full`；无效码领取报一次性提示；`/healthz` 返回 `uptime_seconds`；Relay 日志依次出现 `stored`、`relay full`（附水位）、`claim denied`。服务器上尚未部署本轮版本。
