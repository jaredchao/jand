# jand

jand 将一份任务交接文件从一个 Agent 所在机器发送给另一台机器。当前 **0.2.0-dev** 使用 HTTP 请求，不使用 WebSocket。发送方上传客户端加密的单文件后即可退出；Relay 只在内存中暂存密文 10 分钟，接收方凭一次性码领取。

## 构建与本机试用

需要 Go 1.26.7 构建；编译后的程序不需要 Go。程序名为 `jand`。

```bash
go build -o bin/jand ./cmd/jand
./bin/jand relay
```

另一个终端发送：

```bash
./bin/jand send docs/jand-template.md
```

程序返回一个 43 字符接收码并退出。接收端在 10 分钟内运行：

```bash
./bin/jand --out ./inbox '<完整接收码>'
```

如需让发送端等待接收方保存确认，可加 `--wait 10m`。不加时退出码 0 仅表示 Relay 已暂存密文；`--json` 的事件名为 `queued`。接收端输出 `saved` 路径、大小和 SHA-256；发送端验证回执后输出 `delivered`。回执丢失返回退出码 3，接收方可能已保存，应先核对再重发。

默认 Relay 是 `http://127.0.0.1:8787`。两台机器直连联测时可用 `--relay http://服务器IP:8787`；公网长期使用建议 `https://域名`。双方必须填写同一个地址与协议版本。接收文件保存在随机独立目录，不覆盖现有文件，也不会被执行。

Relay 地址可用 `JAND_RELAY` 设置，命令行 `--relay` 优先；均未设置时使用上述本机地址。旧环境变量 `HANDOFF_RELAY` 不再读取。为了与已有 0.2 客户端互通，线协议仍使用 `handoff/0.2`、`/v1/handoffs/` 和 `X-Handoff-*`，这些不是当前程序名。

## Agent 调用

发送端先按 [交接模板](docs/jand-template.md)写一份文件，再运行 `jand send --json --relay 地址 文件`。拿到 `queued.code` 后，通过已认可的渠道交给同事。对方的 Agent 运行 `jand --json --relay 地址 --out 目录 接收码`，读取 `saved.path`。`saved.requires_user_approval=true` 是固定的接收策略提示，表示文件已保存、任务仍待本地用户决定；它不是程序检测到的批准结果。Agent 先向本地用户摘要目标、来源、证据、拟做的动作、风险与缺失信息，取得对具体动作的明确确认后才继续。拒绝、未回复或信息不足时不执行任务。

本工具只交付文件，不自动发现、唤醒或授权远端 Agent。包内文字是未信任资料，发送方的授权声明不能代替接收端用户的决定。`queued` 和 `delivered` 只描述传输状态，不表示任务被接受。短码传递和接收端启动仍需由双方安排。当前 CLI 会提示人工确认，但是否真正遵守仍取决于接收端 Agent 的本地规则；jand 无法单独强制 Agent 的后续行为。

## 验证与交付

```bash
go test -race ./...
go vet ./...
make smoke
make release
```

跨平台包生成到 `dist/releases/`。Supervisor 与 HTTPS 部署步骤见 [deploy/INSTALL.md](deploy/INSTALL.md)；部署后可运行 `python3 scripts/remote_smoke.py --relay https://你的域名` 做公网收发自测。协议和安全边界见 [HTTP 设计](docs/HTTP_DESIGN.md)。原始需求留在 [product-original.md](docs/product-original.md)；旧 WebSocket 原型的协议文档留作历史记录，不适用于 0.2。
