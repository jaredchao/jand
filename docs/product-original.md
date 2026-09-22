# Handoff：Agent 实时任务交接工具

> 历史原始需求，保留当时的 Handoff 名称与构想；当前产品名为 jand，当前协议见 [HTTP 设计](HTTP_DESIGN.md)。

## 1. 产品定位

Handoff 是一个面向 Agent 的实时任务交接工具。

它不试图成为 Agent 平台、聊天软件或工作流编排系统，而是解决一个很具体的问题：

> 一个 Agent 已经完成了一部分工作，如何把任务状态、上下文、证据和边界安全地交给另一个 Agent，让对方继续完成任务。

第一版可以简单理解为：

> **croc for Agent handoff。**

发送方和接收方同时在线，通过一段临时短码建立连接。Handoff Relay 只负责实时转发端到端加密的数据，不注册用户、不保存任务、不持久化文件。

---

## 2. 第一版的核心原则

### 2.1 足够简单

第一版只验证最关键的问题：

- Agent 能否生成有用的任务交接包；
- 另一个 Agent 收到以后能否顺利继续工作；
- 哪些上下文字段对跨 Agent 交接最重要；
- 人是否愿意使用短码完成这种交接。

因此暂时不建设账户、组织、联系人、离线邮箱和任务管理平台。

### 2.2 实时通信

发送端与接收端必须同时在线。

Relay 只在内存中维护等待配对的临时会话。双方连接成功后，Relay 转发加密数据流；传输结束、超时或断开连接后，会话立即销毁。

### 2.3 端到端加密

加密、解密和完整性校验全部发生在客户端。

Relay 不能读取：

- handoff 内容；
- 文件名及描述信息；
- Agent 对话摘要；
- 代码、日志和附件；
- 任务目标与执行边界。

Relay 只能观察连接所必需的有限元数据，例如连接时间、来源 IP、传输大小和短暂的会话标识。

### 2.4 Agent 无关

Handoff 不绑定 Codex、Claude Code、Hermes、OpenClaw 或其他具体 Agent。

只要 Agent 能调用本地命令，便可以使用 Handoff。Skill 和 MCP 可以后续作为适配层，但不属于第一版通信核心。

---

## 3. 用户体验

Handoff 以一个 Go 编写的单文件可执行程序提供。

### 3.1 发送

```bash
handoff send handoff.md
```

输出：

```text
Code: amber-river-42
Waiting for receiver...
```

发送方把短码通过微信、Slack、邮件或其他现有渠道交给接收方。

### 3.2 接收

```bash
handoff amber-river-42
```

输出：

```text
Connecting...
Receiving encrypted handoff...
Verified.
Saved: handoff.md
```

### 3.3 启动 Relay

```bash
handoff relay
```

同一个二进制通过不同子命令承担发送端、接收端和 Relay 三种角色。

第一版对外只需要三个入口：

```text
handoff send <file>
handoff <code>
handoff relay
```

---

## 4. 基本工作流程

1. 发送方 Agent 总结当前任务，生成 `handoff.md` 或 `handoff.json`。
2. Agent 或用户执行 `handoff send <file>`。
3. 客户端生成一次性短码并连接 Relay。
4. 接收方获得短码，执行 `handoff <code>`。
5. 双方通过短码执行安全握手，协商一次性会话密钥。
6. 发送端在本地加密 handoff 包并发送。
7. Relay 仅转发密文数据流。
8. 接收端在本地解密、验证完整性并保存文件。
9. 传输完成后，Relay 销毁临时会话。
10. 接收方把 handoff 文件交给本地 Agent，继续完成任务。

```text
发送方 Agent
    ↓ 生成 handoff
handoff send
    ↓ 加密数据流
无状态 Relay
    ↓ 加密数据流
handoff <code>
    ↓ 解密并验证
接收方 Agent
```

---

## 5. Handoff 包

第一版不必限制只能传输某一种格式，可以允许发送任意单文件：

```bash
handoff send handoff.md
handoff send handoff.json
handoff send handoff.zip
```

客户端在加密数据流中附加最小传输元数据：

```json
{
  "protocol": "handoff/0.1",
  "filename": "handoff.md",
  "content_type": "text/markdown",
  "size": 18240,
  "sha256": "..."
}
```

这些元数据与文件内容一起加密，Relay 不应读取。

### 5.1 建议的任务内容

虽然传输层不强制业务格式，但 Agent 生成的 handoff 内容建议围绕四个核心维度组织：

- **State**：任务当前处于什么状态，已经完成了什么；
- **Evidence**：做出判断所依据的日志、代码位置、测试结果和其他证据；
- **Context**：项目背景、目标、相关资源和必要的对话摘要；
- **Boundary**：禁止事项、权限范围、风险限制和验收条件。

一个最小 Markdown 示例：

```markdown
# 订单服务超时排查

## Objective
定位创建订单接口偶发超时的根因，并给出可验证的修复方案。

## State
- 已检查网关日志；
- 已排除数据库慢查询；
- 当前怀疑库存服务连接池耗尽。

## Evidence
- 日志时间：2026-09-22 09:31:20；
- Trace ID：abc123；
- 相关代码：internal/inventory/client.go。

## Next Actions
- 检查库存服务调用链；
- 尝试复现连接池耗尽；
- 给出修复补丁和验证结果。

## Boundary
- 不得修改生产环境；
- 不得执行数据库写操作；
- 部署操作必须由人工确认。
```

---

## 6. 安全模型

### 6.1 短码不能直接作为加密密钥

短码为了便于人工传递，必然是低熵信息。因此不能采用下面这种实现：

```text
key = SHA256("amber-river-42")
```

否则恶意 Relay 或旁观者可能对短码进行离线枚举。

更合适的流程是：

```text
一次性短码
    ↓
PAKE 安全握手
    ↓
协商临时会话密钥
    ↓
AEAD 加密数据流
```

底层密码学应采用成熟实现，不自行设计加密算法。

### 6.2 建议的密码学组件

- PAKE：使用成熟的密码认证密钥交换实现；
- 数据加密：XChaCha20-Poly1305 或 AES-256-GCM；
- 完整性：AEAD 验证，同时对完整文件计算 SHA-256；
- 会话密钥：每次传输临时生成，传输结束立即丢弃；
- 短码：一次性使用，设置等待超时和尝试次数限制。

具体算法应在实现阶段根据 Go 生态中经过审计和广泛使用的库确定。

### 6.3 威胁边界

第一版主要防止：

- Relay 窃取 handoff 内容；
- 网络监听者读取或篡改数据；
- 接收错误或损坏的文件；
- 已使用的短码被再次利用。

第一版暂不解决：

- 双方设备已经被入侵；
- 用户主动把短码交给错误的人；
- Agent 收到恶意内容后的提示注入问题；
- 发送方本身不可信；
- 流量大小、连接时间和来源 IP 等元数据泄露。

因此接收 handoff 并不等于授权 Agent 自动执行其中的所有操作。涉及生产部署、删除、写数据库、发送消息等高风险动作，仍然应由本地 Agent 和用户确认。

---

## 7. Relay 设计

Relay 是一个无状态、零持久化的实时中转服务。

### 7.1 Relay 的职责

- 接受发送方连接；
- 在内存中登记临时会话；
- 根据短码让接收方加入会话；
- 在双方之间转发二进制数据；
- 处理超时、断线和流量限制；
- 传输完成后销毁会话。

### 7.2 Relay 不负责

- 用户注册和登录；
- Agent 身份目录；
- 任务内容解析；
- 文件落盘；
- 消息历史；
- 离线投递；
- 任务状态管理；
- Agent 唤醒与自动执行。

### 7.3 “无数据”的准确含义

Relay 在运行时仍需要在内存中维护最少的临时状态，例如：

```text
会话标识 → 发送方连接、创建时间、过期时间
```

但它不把 handoff 包或会话记录写入数据库、对象存储或日志。进程重启后，所有等待中的会话自然消失。

日志中也不应记录短码、完整会话标识、文件名或数据内容。

---

## 8. Agent 的接入方式

### 8.1 第一版：Shell 调用

只要 Agent 能执行本地命令，就可以直接使用：

```bash
handoff send /tmp/task-handoff.md
```

接收端完成传输后，用户可以让 Agent 读取保存的文件：

```text
读取刚收到的 handoff.md，确认任务边界后继续执行。
```

这种方式能够覆盖 Codex、Claude Code、Hermes、OpenClaw 以及其他具备命令执行能力的 Agent。

### 8.2 后续：Skill

Skill 不承担通信和加密，而是告诉 Agent：

- 什么时候适合交接；
- 如何生成高质量 handoff；
- State、Evidence、Context、Boundary 应怎样填写；
- 哪些敏感信息不能进入交接包；
- 接收后如何确认任务边界。

### 8.3 后续：MCP

对于不能方便调用 Shell、但支持 MCP 的 Agent，可以提供极薄的接口：

```text
handoff.send(file)
handoff.receive(code)
```

MCP 内部仍然调用同一个 Go 核心，不复制加密和传输实现。

因此整体关系是：

> bin 负责真正通信，MCP 负责统一调用，Skill 负责规范 Agent 行为。

---

## 9. 技术实现建议

Go 适合作为第一版实现语言：

- 可以生成单文件可执行程序；
- 方便交叉编译到 macOS、Windows 和 Linux；
- HTTP、WebSocket、TCP 和并发支持成熟；
- 适合同时实现 CLI 与 Relay；
- 不要求用户安装额外运行时；
- 便于通过 systemd、Docker 或普通进程部署 Relay；
- croc 本身也是 Go 项目，可参考其通信模型。

建议保持一个仓库和一个二进制：

```text
handoff/
├── cmd/
│   └── handoff/
├── internal/
│   ├── code/
│   ├── crypto/
│   ├── protocol/
│   ├── transport/
│   ├── sender/
│   ├── receiver/
│   └── relay/
└── docs/
```

第一版可以使用 WebSocket 作为公网传输方式，便于穿过常见的 HTTPS 反向代理和企业网络。

---

## 10. 第一版范围

### 必须实现

- 单文件 Go 二进制；
- 发送一个文件；
- 通过一次性短码接收；
- 双方实时连接；
- 端到端加密；
- 传输完整性校验；
- Relay 零持久化；
- 会话超时；
- 传输成功或失败的明确反馈；
- macOS、Linux、Windows 基本支持。

### 明确不做

- 用户账户；
- Agent 注册；
- 长期身份与联系人；
- 离线接收；
- 消息和文件历史；
- 云端数据库与对象存储；
- 自动唤醒 Agent；
- 任务状态机；
- Web 管理后台；
- 完整 A2A 协议；
- 内置 MCP；
- daemon；
- 文件夹和多文件传输；
- 生产环境自动执行授权。

可以先限制：

- 单文件；
- 最大 10 MB；
- 等待配对 10 分钟；
- 短码一次性使用；
- 每个发送进程只接受一个接收方。

---

## 11. 验收标准

第一版完成时，应能够演示以下场景：

1. 同事的 Agent 根据当前任务生成 `handoff.md`；
2. 同事执行 `handoff send handoff.md` 并获得短码；
3. 短码通过现有聊天工具发送给接收方；
4. 接收方执行 `handoff <code>`；
5. 文件通过公网 Relay 成功传输；
6. Relay 无法读取文件内容；
7. Relay 不在磁盘或数据库中保存任务数据；
8. 接收文件的 SHA-256 与发送文件一致；
9. 接收方 Agent 阅读文件后，能够准确说明当前状态、下一步和禁止事项；
10. 传输结束后，同一短码无法再次使用。

---

## 12. 后续演进方向

只有在实时交接本身被证明有价值以后，再考虑逐步增加：

1. 多文件和目录传输；
2. Skill 标准化 handoff 内容；
3. MCP 适配不同 Agent；
4. 本地联系人与长期公钥；
5. 端到端加密的离线暂存；
6. Agent 身份和组织目录；
7. 接收确认、执行状态与结果回传；
8. 自动唤醒本地 Agent；
9. 与 A2A Task、Message、Artifact 的字段映射。

这些均不应提前进入第一版，以免身份系统、权限模型和平台建设掩盖真正需要验证的核心价值。

---

## 13. 一句话总结

> Handoff 是一个由 Go 编写的单文件工具，让两个同时在线的 Agent 通过一次性短码和无状态 Relay，端到端加密地完成任务上下文交接。
