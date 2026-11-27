# macOS 签名与公证

Jand 使用 Developer ID Application 证书签名，使用 Apple `notarytool` 提交公证。签名证书与公证凭据各自独立。

## 在发布机上复用凭据

发布机通过 `notarytool` 从默认的数据保护钥匙串读取公证凭据（下文用 `<notary-profile>` 表示）。先验证凭据，再构建：

```sh
xcrun notarytool history --keychain-profile <notary-profile> --output-format json >/dev/null
python3 scripts/release.py
```

将 `sign_identity` 和 `notary_profile` 存在本机的 `~/.config/jand/release.json`。文件只存签名身份和凭据名称，凭据本身由钥匙串保存。只有凭据明确存放在某个文件钥匙串时，才设置 `notary_keychain` 或传 `--notary-keychain`。命令行参数优先于环境变量，环境变量优先于本机配置。

## 排查公证预检

遇到 `No Keychain password item found for profile` 时，先用保存凭据时相同的钥匙串参数运行 `notarytool history`。省略 `--keychain` 会使用数据保护钥匙串。显式传入 `login.keychain-db` 会改为查找该文件钥匙串，即使同名凭据在默认路径可用，也会报找不到。

发布脚本在编译前执行公证预检。只有 `notarytool` 成功完成公证，包内 `BUILD-INFO.json` 才会将 `notarized` 标为 `true`。

[Apple 的 notarytool 指南](https://developer.apple.com/documentation/technotes/tn3147-migrating-to-the-latest-notarization-tool)
