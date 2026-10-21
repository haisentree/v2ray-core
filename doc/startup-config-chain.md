# 启动、配置与整条链路

> 对应源码：`main/main.go`、`main/commands/run.go`、`config.go`、`v2ray.go`。
> 本文所有命令与日志均在本机（WSL2，V2Ray 5.54.2，go1.26.3）**实测通过**。

---

## 一、项目怎么启动

### 1.1 入口极简

`main/main.go` 全文只有十几行：

```go
import (
	"github.com/v2fly/v2ray-core/v5/main/commands"
	"github.com/v2fly/v2ray-core/v5/main/commands/base"
	_ "github.com/v2fly/v2ray-core/v5/main/distro/all"   // ← 关键：空导入
)

func main() {
	base.RegisterCommand(commands.CmdRun)      // run
	base.RegisterCommand(commands.CmdVersion)  // version
	base.RegisterCommand(commands.CmdTest)     // test
	base.SortLessFunc = runIsTheFirst
	base.SortCommands()
	base.Execute()
}
```

**那行空导入 `_ "main/distro/all"` 是整个启动机制的核心。**

`main/distro/all/all.go` 里没有任何函数调用，只有几十行 `_ "..."` 空导入——
每一个被导入的包在自己的 `init()` 里调用 `common.RegisterConfig` 完成注册：

- 三个**必需** Feature：`app/dispatcher`、`app/proxyman/inbound`、`app/proxyman/outbound`
- 可选 Feature：`dns`、`router`、`policy`、`stats`、`log`、`reverse`…
- 所有**代理协议**：`proxy/socks`、`freedom`、`vmess`、`vless`、`trojan`、`shadowsocks`…
- 所有**传输层**：`transport/internet/{tcp,udp,tls,websocket,quic,grpc,kcp}`
- **配置格式**：`main/formats`（JSON/TOML/YAML）、`infra/conf/v5cfg`
- **子命令**：`main/commands/all`

> 这解释了一个常见问题：**为什么自己写 main 时协议会"不存在"？**
> 因为没有导入对应的包，`init()` 没跑，注册表里就没有它。
> 精简构建时按需替换 `distro/all` 即可裁剪二进制体积。

### 1.2 启动三阶段

| 阶段 | 做什么 | 关键位置 |
| --- | --- | --- |
| ① 进程初始化 | 空导入触发全部 `init()` 注册；`base.Execute()` 分发子命令 | `main.go:6`、`run.go:83` |
| ② 装配 | 定位配置 → `LoadConfig` → `core.New`（**未启动**） | `run.go:104/247`、`v2ray.go:197` |
| ③ 运行 | `Start()` 级联启动 → 阻塞等信号 → `Close()` | `run.go:124/136`、`v2ray.go:385` |

**② 和 ③ 的分界必须分清**：`core.New` 返回时对象已装配完毕，但
**一个端口都没监听，流量完全不可用**。真正的监听发生在 ③ 的 `Start()` 里。

### 1.3 `executeRun` 的四个阶段（run.go:83）

```
1a. setConfigFlags(cmd)            注册 -c/-d/-format/-r
1b. plugins.Plugins                插件预登记（如 -pprof，必须在 Flag.Parse 之前）
2a. cmd.Flag.Parse(args)           解析参数
2b. getConfigFilePath()            定位配置文件（5 级回退）
2c. startV2Ray()                   LoadConfig → core.New
3a. go f()                         插件长驻函数异步拉起
3b. server.Start()                 级联启动，打出 "V2Ray x started"
    defer server.Close()
    runtime.GC()                   清掉配置解析期的垃圾
4.  <-osSignals                    阻塞等 SIGINT / SIGTERM
```

注意两点：

- 阶段 4 **没有"二次信号强制退出"保护**——若 `Close()` 卡住，进程无法退出。
- `runtime.GC()` 是刻意的：protobuf 反射解析会产生大量垃圾，进入稳态前先清一次。

### 1.4 实测命令

```bash
cd /root/Project/Git-temp/v2ray-core

./main/main version
# V2Ray 5.54.2 (V2Fly, a community-driven edition of V2Ray.) Custom (go1.26.3 linux/amd64)

./main/main test -c /tmp/v2ray-test-config.json
# Configuration OK.          ← 只装配不启动，用来验证配置

./main/main run -c /tmp/v2ray-test-config.json
# 前台常驻，Ctrl-C 退出
```

`run` 的参数：

| 参数 | 作用 |
| --- | --- |
| `-c` / `-config` | 配置文件，可多次指定 |
| `-d` / `-confdir` | 配置目录，按扩展名扫描 |
| `-r` | 配合 `-d`，递归扫描 |
| `-format` | `auto`（默认）/ `json` / `toml` / `yaml` |

---

## 二、配置文件

### 2.1 配置文件去哪找（`getConfigFilePath`，run.go:201）

优先级从高到低，**全部落空则从 stdin 读**：

1. `-c` / `-config` 显式给出
2. `-d` / `-confdir` 指定目录（`-r` 时递归）
3. 环境变量 `v2ray.location.confdir` 指向的目录
4. 当前工作目录下的 `config.json`
5. 环境变量 `v2ray.location.config` 指向的文件
6. 都没有 → **从 stdin 读**

### 2.2 最小可用配置（实测通过）

socks 入站 `127.0.0.1:10808` → freedom 出站：

```json
{
  "log": {
    "loglevel": "debug"
  },
  "inbounds": [
    {
      "tag": "socks-in",
      "listen": "127.0.0.1",
      "port": 10808,
      "protocol": "socks",
      "settings": {
        "auth": "noauth",
        "udp": false
      },
      "sniffing": {
        "enabled": false
      }
    }
  ],
  "outbounds": [
    {
      "tag": "direct",
      "protocol": "freedom",
      "settings": {}
    }
  ]
}
```

保存为 `/tmp/v2ray-test-config.json`，`test` 子命令返回 `Configuration OK.`。

### 2.3 带路由规则的示例

在 2.2 基础上加 `routing`，把私有地址打到 blackhole：

```json
{
  "inbounds": [
    { "listen": "127.0.0.1", "port": 10808, "protocol": "socks",
      "settings": { "auth": "noauth", "udp": false } }
  ],
  "outbounds": [
    { "protocol": "freedom", "settings": {}, "tag": "direct" },
    { "protocol": "blackhole", "settings": {}, "tag": "blocked" }
  ],
  "routing": {
    "rules": [
      { "type": "field", "ip": ["geoip:private"], "outboundTag": "blocked" }
    ]
  }
}
```

（写法参照仓库自带的 `release/config/vpoint_vmess_freedom.json`。）

**注意**：`outbounds` 第一项是默认出站。没配 `routing` 时，
`DefaultRouter.PickRoute` 匹配不到规则，dispatcher 会打
`default route for ...` 并回落到 `GetDefaultHandler()`——本次实测正是走的这条路径。

### 2.4 配置 → 对象的映射

```
JSON/TOML/YAML
   │  mergers 合并多个文件
   ▼
infra/conf（jsonv4 或 v5cfg 解析器）
   │
   ▼
core.Config（protobuf，config.proto）
   │  core.New
   ├─ config.App[]      → CreateObjectWithEnvironment → AddFeature
   ├─ config.Inbound[]  → AddInboundHandler  → inbound.Manager
   └─ config.Outbound[] → AddOutboundHandler → outbound.Manager
```

配置里 `protocol` 字段的值（如 `"socks"`）会在注册表中按别名查到具体实现，
找不到就报 `unable to find implementation`——**这也是"协议不存在"多半是忘了 import 的原因**。

---

## 三、整条链路（附实测日志）

### 3.1 启动阶段

```
[Debug]   app/log: Logger started
[Debug]   app/proxyman/inbound: creating stream worker on 127.0.0.1:10808
[Info]    transport/internet/tcp: listening TCP on 127.0.0.1:10808
[Warning] V2Ray 5.54.2 started
```

**这里有个容易误解的点**：`Logger started` 与 `creating stream worker`
都发生在**装配期**（`core.New`），不是 `Start()`。原因是 `app/log/log.go:42` 的注释写得很明白：

```go
// start logger instantly on inited
// other modules would log during init
if err := g.startInternal(); err != nil { return nil, err }
newError("Logger started").AtDebug().WriteToLog()
```

> **log 是唯一在装配期就自我启动的 Feature**，因为它必须早于其他模块的日志输出。
> 你笔记里那条"log 不走 GetFeature、靠包级全局注册"的特例，在这里又体现了一次。

而 `listening TCP` 属于运行期（`worker.Start()`），在 `V2Ray ... started` 之前——
因为 `Start()` 是先级联所有 feature 再打这句（v2ray.go:389-396）。

### 3.2 处理一条请求

实测脚本 `script/test_socks_chain.py` 发一次 SOCKS5 + HTTP GET：

```
[Info]    [827997507] proxy/socks: TCP Connect request to tcp:127.0.0.1:18080
[Warning] [827997507] app/dispatcher: default route for tcp:127.0.0.1:18080
[Info]    [827997507] proxy/freedom: opening connection to tcp:127.0.0.1:18080
[Info]    [827997507] transport/internet/tcp: dialing TCP to tcp:127.0.0.1:18080
[Info]    [827997507] app/proxyman/inbound: connection ends > ... context canceled
[Info]    [827997507] app/proxyman/outbound: failed to process outbound traffic > ... context canceled
```

访问日志（access log）单独一行，**末尾方括号里是最终选中的出站 tag**：

```
tcp:127.0.0.1:58352 accepted tcp:127.0.0.1:18080 [direct]
```

方括号里的 `[direct]` 是排查路由问题最有用的信息——直接告诉你这条流量走了哪个出站。

### 3.3 逐步对应源码

| 日志 | 源码位置 |
| --- | --- |
| `creating stream worker` | `app/proxyman/inbound/always.go:111`（装配期） |
| `listening TCP` | `app/proxyman/inbound/worker.go:123` `internet.ListenTCP` |
| `V2Ray x started` | `v2ray.go:396` |
| `TCP Connect request to` | `proxy/socks/server.go:123` |
| `default route for` | `app/dispatcher/default.go:337` |
| `taking detour [tag]` | `app/dispatcher/default.go:331`（命中路由规则时） |
| `opening connection to` | `proxy/freedom/freedom.go:123` |
| `dialing TCP to` | `transport/internet/tcp` |
| `accepted ... [direct]` | `app/dispatcher/default.go:352-360`（`log.AccessMessage`） |

### 3.4 三条连接三个 session ID

实测三次请求分别拿到 `827997507`、`836420808`、`343039884`——
印证 `worker.go:61` 的 `session.NewID()`：**每条连接一个 ID，日志靠它串联**。
排查单个连接问题时，先 `grep` 这个 ID。

另外注意到一条被拒绝的连接：

```
tcp:127.0.0.1:58352 rejected  proxy/socks: insufficient header > EOF
```

这是测试脚本 `wait_listening()` 的探测连接（连上就关，没发 SOCKS5 握手）。
说明**端口连通 ≠ 协议握手成功**，探测类监控要小心误判。

---

## 四、排错清单

| 现象 | 先看哪里 |
| --- | --- |
| `unable to find implementation` | 协议名拼错，或对应包没被 import（见 1.1） |
| `not all dependency are resolved.` | 装配期依赖没凑齐，检查 `RequireFeatures` 与配置组合 |
| 端口没监听 | 还在装配期；看有没有 `listening TCP` 日志 |
| 连上就断 | 看 `connection ends` 的原因链；常见是 `context canceled`（对侧关了） |
| 走了错误出站 | 看 access log 末尾 `[tag]`，再看 `taking detour` / `default route` |
| 域名解析不对 | 出站 `DomainStrategy`；DNS 递归链路见 `data-plane-design.md` §五 |

排查时把 `loglevel` 调到 `debug`，然后按 session ID grep，基本能定位到具体一跳。
