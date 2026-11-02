# macOS 发布凭据复用

当前发布签名身份：`Developer ID Application: zhou wei (A5L4GGX82X)`。
此前成功使用的公证配置：`claracore-notary`。签名证书和公证凭据是不同的项目，
找到签名证书不证明公证配置存在。

复用既有配置时，先运行只读预检，再构建：

```sh
xcrun notarytool history --keychain-profile claracore-notary \
  --keychain "$HOME/Library/Keychains/login.keychain-db" --output-format json
python3 scripts/release.py --sign 'Developer ID Application: zhou wei (A5L4GGX82X)' \
  --notary-profile claracore-notary \
  --notary-keychain "$HOME/Library/Keychains/login.keychain-db"
```

也可通过 `JAND_SIGN_IDENTITY`、`JAND_NOTARY_PROFILE`、`JAND_NOTARY_KEYCHAIN`
指定非敏感定位信息。脚本在编译前检查公证配置；失败不创建新凭据，不打印密码，
不自动降为未公证。先核对上次实际配置和存储位置，再判断是否需要补充信息。

本机非敏感定位配置保存在 `~/.config/jand/release.json`，字段为 sign_identity、
notary_profile、notary_keychain。脚本默认复用该文件，命令参数和环境变量优先。
不要在文件中存密码、私钥或访问令牌。后续发布应读取此配置和预检结果，不再要求
用户重复提供已有配置名称；配置项找不到也不能推断用户需要新申请凭据。

2026-11-02 核查：此前同名配置有成功记录；当前默认查找及显式 login 钥匙串
均找不到该项。未确认其不可用的原因，不推断已删除、失效或密码错误。
本轮 0.5.0 macOS 归档仍有 Developer ID 签名，但不能标为已公证。
