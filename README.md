# 网络唤醒 · RocWOL

一个自用的 Android 远程唤醒工具。手机把请求发给服务器，服务器通过私有隧道找到路由器，由路由器向固定电脑发送 Wake-on-LAN 魔术包。

App 用 Kotlin 和 Jetpack Compose 编写，界面采用 Material Design 3，支持浅色、深色和跟随系统。服务器网关与路由器执行端用 Go 编写。

这不是通用的路由器管理平台。域名、SSH 主机指纹、目标电脑和部署路径按现有环境配置；换环境前要先改源码和部署脚本，不能直接照搬。

## 怎么用

手机需要 Android 11 或更新版本，设置安全锁屏，并能完成强生物识别或锁屏凭据认证。首次使用需要服务器签发的一次性配对码。

- **唤醒**：查看服务器、路由器和电脑状态，发送唤醒请求，查询最近一次请求的结果。
- **管理**：选择授权时长和访问范围，完成系统身份验证后管理路由器；查看当前授权状态，打开浏览器或手动取消授权。
- **设置**：查看固定目标、切换线路和主题。

“已发送”只表示路由器成功发出魔术包，不表示电脑已经启动。电脑是否支持从睡眠或关机唤醒，仍取决于 BIOS、网卡、电源设置和有线网络。

唤醒请求使用固定请求 ID 去重。网络中断、超时或切换线路时，不会自动补发；结果不确定时先查询原请求。

## 动效与提示

1.2.1 为页面切换加入淡入淡出与轻微横向位移，各页分别保留滚动位置。授权状态、主按钮颜色、信息展开和确认弹窗都有短过渡；主题颜色平滑切换，不重建页面。倒计时不逐秒播放动画，短操作也不会闪一下加载条。动效遵循系统动画时长设置，关闭系统动画后直接显示最终状态。

配对成功、切换线路、授权成功、取消授权和主动取消系统认证使用短 Toast，不再插入顶部通知卡片。网络错误、身份失效和授权状态不确定等重要信息显示为底部悬浮提示，可通过“详情”阅读完整内容；打开详情时不重复叠加提示，放大字体或重建 Activity 不会丢失已打开的说明。动画不会延迟服务器授权、撤销或触发额外请求。

## 管理授权

手机有两把 Android Keystore 密钥：日常唤醒使用一把，路由器管理使用另一把。管理密钥每次签名都需要系统认证。服务器验证管理签名成功时立即开始计算授权时长，不等浏览器打开或兑换票据。普通唤醒凭据不能建立管理授权，只能在本机已有授权的范围内申请浏览器票据。

“管理”页提供以下设置：

- **授权时长**：可选 5、10、15、30 分钟，默认 10 分钟。
- **授权后自动打开浏览器**：默认开启。关闭后，授权仍立即生效，可稍后手动打开浏览器。浏览器打不开不会取消已经生效的授权。
- **允许所有设备访问**：默认关闭。开启前会要求明确确认，系统身份验证时也会提示访问范围。开启后，授权有效期内，任何能访问当前授权域名的设备都能到达 LuCI 登录页，不需要持有 App 的浏览器票据。这会扩大公网入口的暴露范围，不应仅为方便而长期开启。

时长和访问范围设置会保存，只对下一次授权生效，不改变或延长当前授权；自动打开浏览器开关控制下一次操作。每次新授权绑定一个完整来源（协议、域名和端口），并替换这台手机之前的管理授权，使旧票据、Cookie 和连接失效，不影响其他已配对设备的授权。

默认模式下，浏览器必须兑换一次性票据才能访问管理入口。票据最多有效一分钟，且不会晚于授权到期；兑换后 Cookie 继承原授权的到期时间。查询状态、打开浏览器、兑换票据、刷新页面均不续期。两种访问模式都保留 LuCI 独立登录，App 不保存路由器 root 密码；所有设备模式也不放开唤醒 API、执行端保留路径、来源检查或私有 mTLS 通道。

管理页区分有效授权、未授权和尚未确认的状态，并显示有效授权的到期时间与实际访问范围。返回 App 只刷新服务器状态，不会取消授权。授权成功后，原来的“授权管理”主按钮变为实心红色“取消授权”；点击后请求服务器取消，成功取消或到期后恢复为“授权管理”。状态尚未确认但可能仍有授权时，仍可手动请求取消。

断网或 App 被强制停止时，不能保证立即取消，最终由服务器到期失效。本机倒计时到期不表示已成功请求服务器取消。手动取消会关闭这台手机的授权、票据、Cookie 和连接，不影响唤醒，也不取消其他设备建立的授权；它不能擦除浏览器已经加载的页面。

1.2.0 使用新的管理授权签名协议，App 与网关需要一起升级；旧版 App 不能发起新版管理授权。现有配对身份和唤醒协议保持不变。

## 连接方式

```text
Android App / 外部浏览器
          │ HTTPS
          ▼
OpenResty → roc-wol 网关（仅监听本机）
                     │ mTLS
                     ▼
             frpc STCP visitor
                     │ 私有 STCP 隧道
                     ▼
       路由器 roc-wol-agent（仅监听本机）
             ├─ etherwake → 固定电脑
             └─ 管理代理 → LuCI
```

默认线路是 `https://wol.hubu.wiki`，App 还保留 `https://roc.hubu.wiki` 备用选项。线路只能手动切换。App 不启动 VPN，也不会绕过 Clash；实际走哪条网络仍由系统和 Clash 规则决定。

**整理入库时，旧的公网 `15201` 映射和 `roc.hubu.wiki` 直通 LuCI 路由尚未关闭，备用域名尚未完成新网关切换。** 在它们关闭前，仍有绕过 App 管理授权的旧入口。只删除 DNS 记录不能关闭 `IP:15201`，也不能算完成安全切换。

部署脚本保留了双域名切换流程。如果最终只保留直连，需要一起处理旧 FRP 映射、旧反代、证书续期配置和 App 备用选项；此次界面与授权更新没有改动这些旧入口配置。

## 目录

```text
android/          Android App、Gradle Wrapper
cmd/              网关和路由器执行端的命令入口
internal/         认证、网关、执行端、存储和协议实现
deploy/           systemd、procd 与证书续期脚本
tools/            构建、下载校验和部署脚本
```

以下目录只留在本机，不提交 Git：

```text
private/credentials/   SSH 私钥和路由器登录凭据
private/deployment.json 部署身份、端口及隧道密钥
private/backups/       部署前备份
private/gradle-cache/  构建缓存
private/toolchain.json 本机工具链记录
dist/                  APK、Linux 二进制、校验文件和验证记录
```

## 构建

构建脚本面向 Windows PowerShell。先安装 **Go 1.26.3** 和 **Node.js 24 或更新版本**，再在项目根目录运行：

```powershell
npm ci --prefix tools
powershell.exe -NoProfile -ExecutionPolicy Bypass -File tools/build.ps1
```

脚本准备独立的 JDK 17、Android SDK 和 Gradle 8.13，校验下载文件，构建并验证 release 签名。工具链放在 `%LOCALAPPDATA%\RocWol\tools`，不会替换全局 Java 配置。首次构建可能需要接受 Android SDK 许可。

主要产物：

- `dist/roc-wol-release.apk`
- `dist/roc-wol-linux-amd64`
- `dist/roc-wol-agent-linux-arm64`
- `dist/build-manifest.json`
- `dist/roc-wol-release.apk.sha256`

release 签名材料默认保存在 `%LOCALAPPDATA%\RocWol\signing`，不在仓库里。已有 App 要覆盖升级，必须继续使用同一份签名材料；换电脑构建前要单独备份和迁移，不能重新生成后当作原签名使用。

Go 测试可单独运行：

```powershell
go test ./...
```

测试通过不等于已经完成真实唤醒验收。还需要用实际手机和电脑验证系统认证、Clash 网络、睡眠唤醒以及关机唤醒。

## 部署

现有部署针对 Linux amd64 服务器、1Panel/OpenResty、已有 frps 和 arm64 ImmortalWrt 路由器。脚本复用共享 frps，不修改或重启它；新增服务和私有隧道单独管理。

先把凭据放在本机：

- `private/credentials/SSH.pem`：服务器 SSH 私钥。
- `private/credentials/router-password.txt`：路由器密码，也接受 `root` 与密码两行格式，或包含 `username`、`password` 的 JSON。

这两个默认路径可用 `--ssh-key` 和 `--router-password-file` 覆盖。不要把凭据写进命令行参数或提交记录。

```powershell
node tools/deploy.mjs --help
node tools/deploy.mjs inspect
```

第一次连接路由器时，如果没有已保存的主机指纹，脚本会显示首次观察到的指纹并停止。通过本地可信渠道核对后，再传入 `--router-host-key`。服务端也固定校验 SSH 主机指纹，不自动接受变化。

部署分开执行：

```powershell
node tools/deploy.mjs stage --phase private
node tools/deploy.mjs stage --phase https
```

私有阶段部署 STCP 和 mTLS；HTTPS 阶段配置公开证书及直连入口。当前脚本的 HTTP-01 签发需要两个域名都能访问验证路径。直连域名应为 DNS-only；Cloudflare 设置只改相应主机，不改整个区域。

先在真实手机上通过直连完成配对、状态查询、实际唤醒和管理登录，再执行：

```powershell
node tools/deploy.mjs cutover --device-id <已验收手机的设备ID>
```

`cutover` 会切换备用域名并关闭旧 `15201` 映射。设备 ID 检查不能代替人工验收。开始切换后，即使备用线路被 Cloudflare 阻断，也不会恢复旧的公开 LuCI 映射；局域网管理入口保留。

私有 mTLS 叶证书需要轮换时使用 `stage --phase private --rotate-mtls`。公网证书由 `roc-wol-renew.timer` 续期。

## 配对与撤销

以下命令在服务器上执行，管理接口只通过本地 Unix socket 提供：

```sh
/opt/roc-wol/bin/roc-wol pair-code --socket /run/roc-wol/admin.sock --ttl 10m
/opt/roc-wol/bin/roc-wol devices list --socket /run/roc-wol/admin.sock
/opt/roc-wol/bin/roc-wol devices revoke --socket /run/roc-wol/admin.sock --id <设备ID>
```

配对码是一次性的，不要写进 README、日志或截图。手机遗失时，在服务器撤销设备；仅在 App 中清除本机身份不会撤销服务器记录。

## 提交边界

仓库只保存源码、构建和部署脚本。私钥、密码、签名库、真实部署配置、数据库、备份、缓存和安装包不入库。`.gitignore` 只能防止误添加未跟踪文件，提交前仍需检查暂存区；私有仓库也不是密码保险箱。

## 许可证

[MIT](LICENSE)。保留仓库原有的许可证和版权声明。
