# Handoff 0.1 WebSocket 协议与设计决策（历史）

> 历史 0.1 方案，保留当时的 Handoff 名称；当前产品名为 jand，当前协议见 [HTTP 设计](HTTP_DESIGN.md)。

**此文件仅记录旧 0.1 原型，不适用于当前 0.2 构建。当前协议见 [HTTP_DESIGN.md](HTTP_DESIGN.md)。**

状态：开发原型；双方须运行同一协议版本。原始产品说明保存在 `product-original.md`，本文件记录评审后实际实施的约定。

## 1. 实施路线

采用独立 Go CLI + WebSocket Relay，复用外部 PAKE 实现，不要求用户另装 croc。这个选择满足单个程序、自建 Relay 和统一机器输出的原始要求；代价是需要维护并进一步评审自己的应用协议。没有宣称复用整个 croc 协议或与 croc 互通。

包划分：`code` 生成邀请，`wire` 连接与公共消息，`secure` 认证加密，`relay` 中转，`transfer` 文件交付，`cmd/handoff` 命令行。

## 2. 配对

自 0.1.1-dev，客户端接受 `http://服务器IP:8787`，内部转换为 `ws://服务器IP:8787/v1/session` 进行 HTTP Upgrade。`https://` 对应 `wss://`，原有 WS(S) 地址也兼容。公网 HTTP/WS 不再受 loopback 限制。直接部署时 Relay 监听 `0.0.0.0:8787`，放行 TCP 8787 即可；本版不要求域名、证书或反向代理。文件仍在第 3、4 节定义的端到端加密通道内传送。HTTP 外层没有 TLS 保护，公开房间编号及连接元数据可被观察或干扰；秘密不会放入 URL、join 消息或公开编号。

连接 `GET /v1/session`，WebSocket 禁用压缩。双方首个 text message：

```json
{"version":"handoff/0.1","role":"sender","room":"12ab34cd"}
```

`role` 为 `sender` 或 `receiver`。房间编号 32 位，秘密独立生成。发送端登记成功后收到 `{"status":"waiting"}`，此时才输出完整接收码。接收方只能加入已存在且尚未被认领的房间；Relay 原子认领后向双方发送 `{"status":"paired"}`。

错误状态包括 `invalid_join`、`invalid_role`、`unavailable`、`busy_or_collision`，随后断开。房间碰撞时当前发送失败，用户重试生成新码；不会复用已有房间。无自动重试。

**完整码和秘密永远不发送给 Relay，也不将其哈希作为公开标识。** 对于诚实 Relay，房间会在结束后删除；旧码无法重新加入已结束传输。它不是全局永不重复的码注册系统。恶意 Relay 可以破坏可用性，但得不到客户端自动重复的秘密猜测机会。

## 3. 认证

1. 发送端 role 0，接收端 role 1。
2. 使用 `schollz/pake/v3@v3.2.0` 的 `InitCurveWithIdentities`，固定 `p256`。
3. 有序身份为 `handoff/0.1/<room>/sender` 和 `handoff/0.1/<room>/receiver`，绑定协议版本、角色和房间。
4. 发送方传 PAKE 公共数据，接收方更新后返回其公共数据；每条最多 4096 字节。
5. 从 PAKE session key 经 HKDF-SHA256 派生两个 32 字节密钥，info 分别为 `handoff/0.1/<room>/sender-to-receiver`、`handoff/0.1/<room>/receiver-to-sender`，salt 为空。
6. 发送端发送加密的 `Confirm: sender`，接收端验证后回复 `Confirm: receiver`；双方确认完成后才传文件元数据。

客户端每次只接受一轮握手；异常、错误密码或畸形 PAKE 数据均终止，不使用同一码自动重试。PAKE 库对部分畸形输入可能 panic，边界将其转换为认证失败。36 位秘密仍有在线猜测风险；PAKE 不替代秘密分发渠道。临时密钥仅在客户端内存中使用，Go 运行时下不承诺所有历史内存副本被可靠擦除。

## 4. 加密记录

每个后续 WebSocket binary message 是一条 AES-256-GCM 记录。

```text
plaintext = kind(1 byte) || payload
nonce = 4 zero bytes || uint64 big-endian sequence
AAD = "handoff/0.1"
```

每个方向序号从 0 开始独立递增，方向密钥不同。序号隐式，不允许跳过、重排、重放或重连后重置。AEAD 错误立即终止。每个 payload 最多 65536 字节。

| kind | 含义 | 方向 |
| --- | --- | --- |
| 1 | Confirm，载荷 `sender` 或 `receiver` | 双向，首条 |
| 2 | Metadata，JSON | 发送→接收 |
| 3 | Data，非空原始字节块 | 发送→接收 |
| 4 | End，空载荷 | 发送→接收 |
| 5 | Receipt，JSON | 接收→发送 |
| 6 | Failure，固定错误标识 | 接收→发送 |

文件顺序固定为 Metadata、零至多条 Data、End。传输中断或未收到 End 不得提交文件。空文件直接 Metadata、End。

Metadata：`protocol`、`filename`、`size`、`sha256`，最多 4096 字节。原文中的 `content_type` 暂未实现，因为本版不解析或执行内容。

Receipt：`{"status":"saved","sha256":"...","size":123}`。Failure 只包含 `receive_failed`，不泄露接收方本地路径或系统错误。

## 5. 文件与交付语义

发送端对不超过 10 MiB 的普通文件做内存快照，元数据和发送字节来自同一份数据。接收端独立验证声明长度、实际长度、完整 SHA-256 与记录顺序。

接收目录内创建 0700 随机子目录，临时文件 0600，拒绝跨平台路径分隔符、控制字符、Windows 保留设备名及不安全结尾。临时内容验证通过后执行文件 Sync、Close，再用 hard link 原子建立最终文件；目标已存在（包括 symlink）时失败。删除临时链接后发送 Receipt。ZIP 不自动解压。

正常失败删除临时文件与目录。进程强制终止或断电可能留下 `.partial`，下次不会自动当成已收文件使用；由用户检查清理。不保证面对已控制本地同一账户的攻击者。

发送端只有验证 Receipt 内容匹配才显示 delivered。End 一旦开始发送，其写入失败、回执超时、回执格式不符、回执丢失均返回退出码 3（结果未知）。明确收到经过认证的 Failure 返回退出码 1。接收端文件已保存但回执发送失败时保留文件并报告保存路径、退出码 3；不会再声称文件未保存。

文件保存确认与 Agent 开始执行、任务完成相互独立。本版没有后两种远端状态。

## 6. Relay 资源与生命周期

Relay 维护内存 map 和连接句柄，读取/转发单条有限大小的消息，不写应用文件、数据库、短码或会话日志。

| 约束 | 默认 |
| --- | --- |
| 连接升级及首次 join | HTTP 头 5 秒；join 10 秒 |
| 会话上限 | 128（可配置，最大 4096） |
| WebSocket 连接上限 | 会话上限 × 2 |
| 初始 join 消息上限 | 4096 字节 |
| 转发消息上限 | 65664 字节 |
| 每会话累计密文字节 | 11 MiB，含两个方向 |
| 每会话转发消息数 | 1024；拒绝空消息 |
| 等待配对 | 10 分钟 |
| 配对后空闲 / 总时长 | 30 秒 / 5 分钟 |

缓冲以单条消息为单位，不缓存整个交接包。每会话只允许一个接收方；任意一侧断开、超时或服务器退出均关闭双方并删除会话。全局上限只能限制资源占用，不保证公开匿名服务在恶意流量下仍然可用；本版未实现分布式限流或多实例房间路由。

## 7. Agent 接续

生成码后必须维持发送进程，`--json` stdout 立即输出事件，适配支持持续进程输出的命令工具。接收后用户让 Agent 读取 `saved.path`；Agent 自行结合本地资源执行，不自动导入系统提示、权限或发送方的授权。

模板区分已确认事实、推测、资源版本、缺失依赖、下一步和执行边界。产品验收应另外进行真实两设备、两个 Agent 的接续任务试用；本机通信测试不能替代这一项。

## 参考与依赖依据

- [croc](https://github.com/schollz/croc)：短码实时传输的参考。
- [schollz/pake](https://github.com/schollz/pake)：使用 v3.2.0 的身份绑定 API，要求应用层双向密钥确认。
- [Magic Wormhole 安全说明](https://magic-wormhole.readthedocs.io/en/latest/attacks.html)：公开会话编号与秘密必须分离；短码猜测与可用性边界。
- [coder/websocket](https://github.com/coder/websocket)：使用 v1.8.15，支持上下文、读大小限制和禁用压缩。

尚无独立密码学审计，没有核实或宣称所选 PAKE 库经过第三方审计。测试中的恶意中继代理检验了一组主动攻击路径，不能证明不存在其他协议缺陷。
