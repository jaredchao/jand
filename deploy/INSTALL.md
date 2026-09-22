# jand 0.2 Relay 部署与联测（Supervisor + HTTPS）

以下命令由服务器管理员执行。先确认服务器架构、现有 Supervisor 配置包含目录、域名 DNS、证书和私钥路径，以及 8787 端口是否已有旧 Relay。切换时等待旧会话结束；Relay 的未领取密文只在内存中，重启会丢失。旧 0.1 WebSocket 客户端不能连接 0.2 HTTP Relay。

## 1. 选择服务器二进制

将 `jand-0.2.0-dev-relay-deploy.zip` 上传并解压，在解压目录检查两个 Linux 归档：

```bash
sha256sum -c SHA256SUMS.txt
uname -m
```

`x86_64` 选 `jand-0.2.0-dev-linux-amd64.tar.gz`，`aarch64` 选 `jand-0.2.0-dev-linux-arm64.tar.gz`。例如 x86_64：

```bash
tar -xzf jand-0.2.0-dev-linux-amd64.tar.gz
cd jand-0.2.0-dev-linux-amd64
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

Relay 的运行日志写入 Supervisor 配置中的 `stdout_logfile`（示例为 `/var/log/jand-relay.log`）：启动一行，其后每次上传、领取、回执与过期各一行，被拒绝的请求记 WARN 并注明原因。日志只含会话短标识与字节数，不含接收码、令牌或文件内容。没有流量时不产生日志，此时用 `curl -s http://127.0.0.1:8787/healthz` 确认进程存活，它返回 `{"status":"ok","uptime_seconds":N}`。

示例 `autostart=false`，因此 `update` 后仍由管理员明确 `start`。若 8787 已被旧 Relay 占用，先查明旧进程和未完成会话；不要同时启动两个 Relay。`supervisorctl status` 和本机健康检查分别证明 Supervisor 进程状态与本机 HTTP 响应，不能证明公网入口或文件交付。

## 3. 域名与证书

域名 DNS 指向目标服务器。选现有 Nginx 或 Caddy 的一条代理链，不要同时为同一域名启用两套配置。Nginx 示例在 [nginx.conf.example](nginx.conf.example)：将 `jand.example.com`、证书和私钥路径改为实际值，并将该 server 块合并到现有配置；不要覆盖其他站点。Caddy 示例在 [Caddyfile.example](Caddyfile.example)。若由 Nginx 使用已有证书，先确认服务进程能读取证书文件，再由管理员执行配置测试和重载。

Relay 保持只监听 `127.0.0.1:8787`；公网只开放 HTTPS 入口，不需要开放 8787。Nginx 示例关闭请求体和响应缓冲，并禁用该站点访问日志；仍需检查上游代理和全局日志规则，避免记录房间路径或将请求体缓存到磁盘。

完成域名与代理配置后，在服务器外用真实域名检查：

```bash
curl -fsS https://你的域名/healthz
```

这只能证明 HTTPS 路径可达，不能判定具体 Relay 版本，也不能代替收发测试。

## 4. 收发验收

在本地源码目录先运行 `make build`，再用一个临时测试文件执行：

```bash
python3 scripts/remote_smoke.py --relay https://你的域名
```

脚本会验证 HTTPS 健康检查、上传后 `queued`、接收文件字节与 SHA-256、经回执确认的 `delivered`，以及旧码不能再次领取；不会打印接收码。它只证明当前客户端经公网入口往返一次，不证明两台设备或两端 Agent 的真实交接。最后由双方在各自设备用对应平台的 `jand` 包做一次任务文件交接。

正式分发前，用已确认的真实地址重新生成客户端包：

```bash
python3 scripts/release.py --relay https://你的域名
```

重新核对 `dist/releases/SHA256SUMS.txt`。当前默认包内的 `203.0.113.10` 是示例地址，不是已部署服务。
