# 集成 Jand 内核

插件可以通过 Jand CLI 发起交接、接收协作消息和读取本地记录。调用方应固定内核版本，并将收到的消息和文件视为外部输入。用户授权由宿主负责。

| 接口 | 用途 |
|---|---|
| [Agent 操作契约](../AGENT.md) | 命令、JSON 事件、退出码和收件流程 |
| [聊天记录接口](inspect-contract.md) | Jand 0.5.0 起使用 `chat inspect --json` 读取本地记录。该命令不向 Relay 请求数据 |
| [本地任务控制器](TASK_CONTROLLER_2026-11-02.md) | 任务版本、派发容量、证据和取消状态 |
| [令牌管理](onboarding-contract.md) | 自建 Relay 的可选令牌配置 |

## 固定内核版本

从明确的 GitHub Release 获取目标平台归档、`SHA256SUMS.txt` 和发行元数据。把发布标签解析为完整提交 ID，然后生成清单：

```sh
gh release view "$tag" --repo jaredchao/jand \
  --json tagName,targetCommitish,isPrerelease,assets > "$release_json"
git ls-remote origin "refs/tags/$tag" "refs/tags/$tag^{}"
python3 scripts/runtime_manifest.py \
  --assets-dir "$assets_dir" --release-json "$release_json" \
  --source-revision "$release_commit" --out "$manifest_path"
```

带注释的标签使用 `^{}` 对应的提交。生成器检查 GitHub 资产摘要、发行校验和以及归档中的平台、版本和二进制哈希。清单应随插件版本一起审查和分发。运行时先校验归档，再安全解包到插件私有目录，最后校验可执行文件哈希。平台支持取决于该版本的实际资产。

## 本地验证

```sh
make check
make build
python3 scripts/plugin_smoke.py
```

真实宿主的收件、展示和任务生命周期应使用插件自身的集成测试验证。
