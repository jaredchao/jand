# jand 0.2 Relay 部署与联测（Supervisor + HTTPS）

以下命令由服务器管理员执行。先确认服务器架构、现有 Supervisor 配置包含目录、域名 DNS、证书和私钥路径，以及 8787 端口是否已有旧 Relay。切换时等待旧会话结束；Relay 的未领取密文只在内存中，重启会丢失。旧 0.1 WebSocket 客户端不能连接 0.2 HTTP Relay。

## 1. 选择服务器二进制

将 `jand-<版本>-relay-deploy.zip` 上传并解压，在解压目录检查两个 Linux 归档：

```bash
sha256sum -c SHA256SUMS.txt
uname -m
```

`x86_64` 选 `jand-<版本>-linux-amd64.tar.gz`，`aarch64` 选 `jand-<版本>-linux-arm64.tar.gz`。例如 x86_64：

```bash
tar -xzf jand-<版本>-linux-amd64.tar.gz
cd jand-<版本>-linux-amd64
./jand --version
sudo install -m 0755 ./jand /usr/local/bin/jand
```

同一个 `jand` 程序既支持客户端命令，也支持 `relay`。服务器只需对应 CPU 架构的一份。

## 2. Supervisor 管理 Relay

示例配置是 [jand-relay.supervisor.conf](jand-relay.supervisor.conf)。它以专用 `jand` 用户运行，监听 `127.0.0.1:8787`，由本机反向代理提供公开 HTTPS。先用 `id jand` 确认用户存在；若不存在，由管理员按发行版创建。再核对配置中的程序路径、用户和日志路径。以下以 Supervisor 的包含目录为 `/etc/supervisor/conf.d/` 举例；实际路径以服务器主配置的 `[include]` 为准。

```bash
sudo install -m 0644 ../jand-relay.supervisor.conf /etc/supervisor/conf.d/jand-relay.conf
sudo supervisorctl reread
sudo supervisorctl update jand-relay
sudo supervisorctl start jand-relay
sudo supervisorctl status jand-relay
curl -fsS http://127.0.0.1:8787/healthz
```

Relay 的运行日志写入 Supervisor 配置中的 `stdout_logfile`（示例为 `/var/log/jand-relay.log`）：启动一行，其后每次上传、领取、回执与过期各一行，被拒绝的请求记 WARN 并注明原因（`busy`、`malformed tokens`、`upload timed out`、`invalid or oversized body`、`code collision`、`relay full`；`relay full` 一行附带当前与上限的会话数和字节数）。启动行会列出版本与 `max_sessions`、`max_stored`、`ttl`、`upload_timeout`，可据此确认替换后的程序已生效。日志只含会话短标识与字节数，不含接收码、令牌或文件内容。没有流量时不产生日志，此时用 `curl -s http://127.0.0.1:8787/healthz` 确认进程存活，它返回 `status`、`uptime_seconds`、`version` 等字段（0.4.1 起带版本号）。

**可选：配置文件（0.4.1 起）。** 需要调整超时、容量、对话预算或暂停后收尾消息上限时，把 [relay.json.example](relay.json.example) 复制为 `/etc/jand/relay.json`，按需删改（没写的字段保持默认值；拼错的字段名会让 Relay 拒绝启动，而不是被悄悄忽略），然后在 Supervisor 的 `command` 末尾加 `--config /etc/jand/relay.json`。命令行上的 `--listen`、`--max-sessions` 仍然优先于配置文件。上线前先运行 `jand relay --config /etc/jand/relay.json --print-config` 查看最终生效的值；启动日志也会记录所用的配置文件路径。不加 `--config` 时，行为与此前版本完全一致。

示例 `autostart=false`，因此 `update` 后仍由管理员明确 `start`。若 8787 已被旧 Relay 占用，先查明旧进程和未完成会话；不要同时启动两个 Relay。`supervisorctl status` 和本机健康检查分别证明 Supervisor 进程状态与本机 HTTP 响应，不能证明公网入口或文件交付。

## 升级已部署的 Relay

替换前确认没有同事正在收发：重启会丢失所有未领取的密文。按第 1 节选好架构并校验后：

```bash
sudo install -m 0755 ./jand /usr/local/bin/jand
sudo supervisorctl restart jand-relay
tail -n 5 /var/log/jand-relay.log
curl -fsS http://127.0.0.1:8787/healthz
```

启动行的 `version=` 应为新版本，`uptime_seconds` 应从 0 附近重新计数。

客户端要不要跟着升级，分两种情况：

- **交接**：线协议自 0.2 起未变，已分发的客户端无需升级。
- **对话**：协议仍在开发，Relay 升级时查看 README 的「兼容性」一节。0.4.1 的 Relay 兼容 0.4.0 客户端；0.3.x 与 0.4.x 之间未验证。升级对话协议时，提前通知使用对话的同事一起升级客户端。

0.4.1 起，`/healthz` 返回 `version` 和 `chat_features`，直接用 `curl -fsS https://你的域名/healthz` 就能确认公网上跑的是哪个版本、支持哪些对话功能。更早的版本不返回版本号，只能看启动日志。

## 3. 域名与证书

域名 DNS 指向目标服务器。选现有 Nginx 或 Caddy 的一条代理链，不要同时为同一域名启用两套配置。Nginx 示例在 [nginx.conf.example](nginx.conf.example)：将 `jand.example.com`、证书和私钥路径改为实际值，并将该 server 块合并到现有配置；不要覆盖其他站点。Caddy 示例在 [Caddyfile.example](Caddyfile.example)。若由 Nginx 使用已有证书，先确认服务进程能读取证书文件，再由管理员执行配置测试和重载。

Relay 保持只监听 `127.0.0.1:8787`；公网只开放 HTTPS 入口，不需要开放 8787。Nginx 示例关闭请求体和响应缓冲，并禁用该站点访问日志；仍需检查上游代理和全局日志规则，避免记录房间路径或将请求体缓存到磁盘。

完成域名与代理配置后，在服务器外用真实域名检查：

```bash
curl -fsS https://你的域名/healthz
```

这证明 HTTPS 路径可达，并能从 `version` 看出是哪个版本（0.4.1 起）；但仍然不能代替收发测试。

## 4. 收发验收

在本地源码目录先运行 `make build`，再用一个临时测试文件执行：

```bash
python3 scripts/remote_smoke.py --relay https://你的域名
```

脚本会验证 HTTPS 健康检查、上传后 `queued`、接收文件字节与 SHA-256、经回执确认的 `delivered`，以及旧码不能再次领取；不会打印接收码。它只证明当前客户端经公网入口往返一次，不证明两台设备或两端 Agent 的真实交接。

对话功能用同一个源码目录验证：

```bash
python3 scripts/chat_smoke.py https://你的域名
```

它在本机模拟双方，经公网 Relay 走完一遍：邀请、加入、后台 `recv` 被唤醒、消息类型与回复、先读再回、取代关系、双方完成后暂停、暂停后收尾消息、按示例流程 `review` 的对话、提出与接受新目标、关闭。最后由双方在各自设备用对应平台的 `jand` 包做一次真实交接或对话。

**关于 Relay 地址**：公开的包和仓库只使用示例地址 `203.0.113.10`，它不是已部署的服务。使用者把真实地址写进本机配置（`jand config` 显示配置文件路径，写入 `{"relay": "https://你的域名"}`），或设置 `JAND_RELAY`。如果需要把真实地址写死在包里，用 `python3 scripts/release.py --relay https://你的域名` 另行构建，只私下分发，不上传公开 Release，并重新核对 `SHA256SUMS.txt`。
