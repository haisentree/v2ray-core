# 01b - 结合配置文件看启动流程：到底注册了哪些 Feature

> 走读时间：2026-10-22
> 核心证据：`infra/conf/v4/v2ray.go:373 Config.Build()` —— JSON 翻译成 protobuf 的现场
> 配套文件：`config-demo.json`（本目录，可直接 `v2ray run -c` 跑）

## 一、示例配置（config-demo.json）

```
log        日志设置
dns        内置 DNS 客户端
routing    路由规则（引用出站 tag）
inbounds   [ socks-in @ 127.0.0.1:10808 ]
outbounds  [ direct(freedom), block(blackhole) ]
```

## 二、JSON → protobuf：Build() 的翻译现场

`infra/conf/v4/v2ray.go:373` 的 `(c *Config) Build()` 做三件事：

### 1. 无条件塞进 App 列表的 3 个（:378-384）

```go
config := &core.Config{
    App: []*anypb.Any{
        serial.ToTypedMessage(&dispatcher.Config{}),        // → app/dispatcher
        serial.ToTypedMessage(&proxyman.InboundConfig{}),   // → app/proxyman/inbound
        serial.ToTypedMessage(&proxyman.OutboundConfig{}),  // → app/proxyman/outbound
    },
}
```

**不管你配置写什么，这三个必装**——没有它们数据面就不成立（v2ray.go 注释也要求：Dispatcher + InboundManager + OutboundManager 是 Instance 的三大件）。

### 2. log 特殊处理（:402-410）

```go
// let logger module be the first App to start,
// so that other modules could print log during initiating
config.App = append([]*anypb.Any{logConfMsg}, config.App...)
```

- 配了 `log` 用你的，没配用 `DefaultLogConfig()`——**日志永远有**。
- 被**插队到 App[0]**：App 列表顺序 = feature 创建顺序 = 启动顺序，日志必须第一个起，后面模块初始化才有日志可写。

### 3. 其余段：配了才有（:386-495）

| JSON 段 | 产出的 App message | 实现 | 注册位置 |
|---------|-------------------|------|---------|
| `api` | `commander.Config` | app/commander（gRPC 管理面） | commander.go:122 |
| `stats` | `stats.Config` | app/stats | stats.go:164 |
| `routing` | `router.Config` | app/router | router.go:170 |
| `dns` | `dns.Config` | app/dns | dns.go:401 |
| `policy` | `policy.Config` | app/policy | policy/manager.go:65 |
| `reverse` | `reverse.Config` | app/reverse | reverse.go:29 |
| `browserForwarder` | 同名 | app/browserforwarder | forwarder.go:126 |
| `observatory` 等 | 同名 | app/observatory(+burst/multi) | observer.go:271 等 |
| `services` | 任意 v5 异构配置 | 按服务名动态加载 | v5cfg |

### 本示例的 App 列表最终长这样

```
App[0] log.LogConfig          ← 插队
App[1] dispatcher.Config      ← 无条件
App[2] proxyman.InboundConfig ← 无条件
App[3] proxyman.OutboundConfig← 无条件
App[4] dns.Config             ← 因配了 "dns"
App[5] router.Config          ← 因配了 "routing"
```

（stats/policy 没配 → 不会有对应 feature，但 v2ray.go:239 的兜底会补默认实现，见下。）

## 三、inbounds/outbounds：不是 App，是 Handler

关键区分：**feature（App）是常驻服务，handler（Inbound/Outbound）是被 manager 管理的实体**。

```
JSON inbounds[i]                        v4 InboundDetourConfig.Build() :118
   │  protocol:"socks" → 查 inboundConfigLoader(:30) 的协议表
   │     settings → SocksServerConfig → Build() → socks.ServerConfig(TypedMessage)
   ▼
core.InboundHandlerConfig{              ← 注意：这是 core 级 proto（config.proto:41）
   Tag:             "socks-in",
   ReceiverSettings: proxyman.ReceiverConfig  (监听地址/端口/sniffing)
   ProxySettings:    proxy.socks.ServerConfig  (协议配置)
}
```

`outbounds` 同理产出 `core.OutboundHandlerConfig`（tag/direct + freedom.Config 等）。
另外：**App 列表第一个出站 = 路由的默认出口**（config.proto:19 注释）。

## 四、运行时装配：App → Feature → Handler 的三级跳

`initInstanceWithConfig`（v2ray.go:203）拿到 protobuf 后：

```
① 遍历 config.App（:221-237）
   每个 anyproto → CreateObject → 查 typeCreatorRegistry → feature 实例 → AddFeature
   （注册表条目就是上表右列的 init() 注册，被 all.go 空白导入触发）

② 兜底检查（:239-255）
   没配的四个必需 feature 补默认实现：
     dns → localdns / policy → policy.DefaultManager
     router → routing.DefaultRouter / stats → stats.NoopManager
   本示例配了 dns/router，所以只补 localdns？不——
   config.App 里已有 dns/router 的配置类型 → 创建的是真实现 → 兜底跳过；
   stats/policy 未配 → 补 NoopManager / DefaultManager

③ addInboundHandlers / addOutboundHandlers（:267-273）
   对每个 InboundHandlerConfig：
     CreateObject(*core.InboundHandlerConfig)         ← 注册表 app/proxyman/inbound/inbound.go:177
       → 创建 handler 时内部再 CreateObject 两层：
          ReceiverSettings → listener（真正监听 10808 端口）
          ProxySettings    → proxy/socks 的 Server（socks.go 里的 init 注册）
     → inboundManager.AddHandler(handler)             ← manager 正是 App[2] 那个 feature
```

**所以本示例跑起来后的完整清单**：

| 类别 | 对象 | 来源 |
|------|------|------|
| Feature | app/log | JSON log 段（或默认） |
| Feature | app/dispatcher | 无条件 |
| Feature | app/proxyman/inbound（Manager） | 无条件 |
| Feature | app/proxyman/outbound（Manager） | 无条件 |
| Feature | app/dns | JSON dns 段 |
| Feature | app/router | JSON routing 段 |
| Feature | app/policy（DefaultManager 兜底） | v2ray.go:244 |
| Feature | app/stats（NoopManager 兜底） | v2ray.go:246 |
| Handler | socks-in → proxy/socks | inbounds[0] |
| Handler | direct → proxy/freedom | outbounds[0] |
| Handler | block → proxy/blackhole | outbounds[1] |

## 五、三层注册表全景（本课核心洞察）

同一个 `typeCreatorRegistry`，装着三类东西：

```
① Manager/Feature 类型:  *proxyman.InboundConfig  → inbound.Manager      (:174)
② Handler 骨架类型:      *core.InboundHandlerConfig → inbound.Handler   (:177)
③ 协议配置类型:          *socks.ServerConfig       → socks Server       (proxy/socks/socks.go)
                         *freedom.Config           → freedom Handler    (freedom.go:25)
```

一个 socks 入站从 JSON 到活对象，要查 ③②① 各一次（顺序是 ②→内部分发→③，①在装配期已就位）。

## 六、速记 + 自测

> **速记**：`dispatcher/proxyman×2 永远在，log 排最前；其余 App 看 JSON 段，缺的用兜底；inbounds/outbounds 不进 App，由两个 Manager 领养。**

1. 为什么删掉 `"dns": {...}` 段实例也能正常做域名解析？
   （答：v2ray.go:243 兜底 localdns.New()——功能降级但可用。）
2. `api` 段配了但 `stats` 没配会怎样？
   （答：commander feature 创建成功，但其 gRPC 服务若依赖 stats.Counter 会拿到 NoopManager——能跑，数值无意义。）
3. 想让某个 app feature **永远不装**，配置层做得到吗？
   （答：做不到——注册靠 all.go 编译期导入；配置只能决定"装不装配置对象"，二进制里有什么才决定能有什么。这也叫 feature flag 反模式？不，这是编译期 feature selection。）

## 下一课预告

**02 - 数据流主干**：本课是"静态装配"，下一课看"动态流动"——socks handler 收到连接后，`Dispatcher.Dispatch` 如何嗅探协议、把目标交给 Router 匹配 `routing.rules`、最终调 `direct`（freedom）出站。
