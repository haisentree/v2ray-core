# core.Instance 设计笔记

> 对应源码：[v2ray.go](../v2ray.go)（`type Instance struct`，v2ray.go:108）。
> 本文以阅读 v2fly v5.54.2 源码为目的，梳理这个结构的定位、各字段职责、背后的设计取舍，
> 以及 Feature 实例全景、实例间的依赖接线与一次请求的完整数据流。

## 一、定位：一个进程 = 一组 Feature 的集合

`Instance` 是 v2ray 的**运行时管理者**，它自己不实现任何具体功能。真正的功能——
dispatcher、路由、DNS、出入站管理器、统计……——全部是挂在 `features` 数组里的
一个个 `features.Feature`。

```
Instance（壳/管理者）
 ├── features[0] = inbound.Manager        // 功能组件
 ├── features[1] = dispatcher
 ├── features[2] = router
 ├── features[3] = dns.Client
 ├── ...
 ├── env      = RootEnvironment           // 底层能力注入（与 features 正交）
 └── ctx      = context.Background()      // 实例根 context
```

`Feature` 的契约极小（[features/feature.go](../features/feature.go):18），只有三个方法：

| 方法                  | 来源            | 作用                                       |
| --------------------- | --------------- | ------------------------------------------ |
| `Type() interface{}`  | `common.HasType` | 返回"接口类型的 nil 指针"作注册/查找键     |
| `Start() error`       | `common.Runnable` | `Instance.Start` 级联调用                 |
| `Close() error`       | `common.Runnable` | `Instance.Close` 级联调用                 |

满足这三个方法就能挂进 `Instance`。接口定义在 `features/`，默认实现在 `app/`，
第三方可以拿自己的实现替换——这是 v5 "一切皆 Feature、一切可替换" 的基础。

## 二、字段逐个解析

```go
type Instance struct {
    access             sync.Mutex
    features           []features.Feature
    featureResolutions []resolution
    running            bool
    env                environment.RootEnvironment

    ctx context.Context
}
```

### 1. `features []features.Feature` —— 运行时的本体

- **数组而非 map**：Feature 总量只有十几个，`getFeature`（v2ray.go:42）线性遍历、
  用 `reflect.Type` 逐一比对，O(n) 足够；换来的是注册顺序确定、无哈希开销。
- **先注册者胜出**：`AddFeature` 只是 `append`，不做同类型替换；`GetFeature`
  返回第一个匹配项。初始化时 essential features 采用
  "`GetFeature(type) == nil` 才补默认实现"（v2ray.go:265），所以配置里显式注册的
  实现天然优先于兜底的默认实现——替换机制就藏在这个顺序约定里。
- **查找键的惯用法**：`Type()` 返回形如 `(*Dispatcher)(nil)` 的"接口类型 nil 指针"。
  Go 里接口本身无法直接当类型标识，用 typed nil pointer 是标准替代：调用方写下
  `GetFeature(inbound.ManagerType())` 再断言回接口，得到静态类型安全的查找。

### 2. `featureResolutions []resolution` —— 依赖挂起队列

v2ray 没有构造函数注入，组件间的依赖是**运行时解析**的：

```go
type resolution struct {
    deps     []reflect.Type   // 需要哪些 Feature（按 Type 匹配）
    callback interface{}      // 凑齐后要调用的函数
}
```

- `RequireFeatures(callback)`（v2ray.go:320）把 callback 的每个参数类型
  `reflect.PtrTo(In(i))` 转成依赖类型列表，构成一个 `resolution`。
- 解析时若 `features` 里凑不齐依赖，整个 resolution 挂进 `featureResolutions`；
- 每次成功 `AddFeature` 后**重试所有挂起的 resolution**（v2ray.go:357-371），
  凑齐一个触发一个，全部清空后置 `featureResolutions = nil`。
- 初始化收尾时做一次强校验：`featureResolutions != nil` 直接报
  `"not all dependency are resolved."`（v2ray.go:273）——保证带病配置不会溜进运行期。

本质上是把"依赖还没就绪"变成可延迟、可重试的一等公民，注册顺序因此变得无关紧要。

### 3. `running bool` —— 生命周期状态位 + 热启动开关

- `Start()`（v2ray.go:385）先置 `running = true`，再级联调用所有 feature 的
  `Start()`；`Close()`（v2ray.go:299）反过来先置 false 再级联 `Close()`。
- **热启动**：`running == true` 期间 `AddFeature` 进来的新 feature 会立即被
  `Start()`（v2ray.go:346-351），即支持运行期动态挂载组件（API 层的
  inbound/outbound 增删就依赖这条路径）。
- 两个细节：
  - `Start` 失败后实例状态是 *unknown*（注释原话），且实例**只能启动一次**，
    Close 之后不保证能再 Start；
  - 热启动分支直接 `return`，**不会**再触发依赖解析——运行期
    `featureResolutions` 恒为 nil（初始化强校验保证），所以两段逻辑不会打架。

### 4. `access sync.Mutex` —— 锁的边界划得很窄

锁**只保护 `Start`/`Close` 之间 `running` 的翻转互斥**，`AddFeature` /
`RequireFeatures` / `GetFeature` 都不加锁。这是有意的边界假设：

- 装配阶段（`New` → `initInstanceWithConfig`）约定单线程串行，无需并发保护；
- 运行期通过 API 动态 `AddFeature` 时则依赖调用方自身的串行化。
  （换句话说：把 `AddFeature` 当作并发安全随便调是错误用法，这点阅读时容易被忽略。）

### 5. `env environment.RootEnvironment` —— 与 features 正交的能力注入层

v5 引入的 [environment](../common/environment/rootcap.go) 体系解决的是另一类问题：
**底层能力**（系统网络 Dialer/Listener、文件系统、临时/持久存储）的注入与作用域隔离。

- `RootEnvironment` 可以按 tag 派生 `AppEnvironment`（带持久存储）和
  `ProxyEnvironment`（仅临时存储），每个入站/出站 handler 拿到独立 scope，
  存储互不可见，`RemoveOutboundHandler` 时可整体 `DropProxyEnvironment` 回收；
- features 管"功能组件"，env 管"底层能力"，两者的汇合点是
  `CreateObjectWithEnvironment`：config 里的每个 app/proxy 对象创建时同时拿到
  Instance（用于依赖解析）和对应 scope 的 env（用于能力访问）。

### 6. `ctx context.Context` —— 自举的根 context

- `New` 默认给 `context.Background()`，`NewWithContext` 可自定义；
- 这个 ctx 会通过 `context.WithValue(ctx, v2rayKey, v)`（[context.go](../context.go):39）
  携带 `*Instance` 本身，向所有子组件、所有 goroutine 扩散；
- 任意深处都可用包级函数 `RequireFeatures(ctx, callback)` /
  `MustFromContext(ctx)` 取回实例——**组件不需要持有 Instance 指针，
  context 就是它的服务定位入口**。这是"回调注册式依赖解析"能以包级函数形式
  暴露的前提。

## 三、一次完整的装配（New 的时序）

```
New(config)
 └─ initInstanceWithConfig
     ├─ 1. 建 RootEnvironment（系统网络/文件系统/deferred 持久存储）
     ├─ 2. 遍历 config.App：CreateObjectWithEnvironment → AddFeature
     │      （app 组件在创建期就可通过 RequireFeatures 声明依赖，先挂起）
     ├─ 3. 补齐 4 个 essential features（仅当缺失时）：
     │      localdns / policy.DefaultManager / routing.DefaultRouter / stats.NoopManager
     ├─ 4. 校验 featureResolutions 已清空，否则报错退出
     ├─ 5. 给 deferred 持久存储 ProvideInner（此时 storage service 已就绪）
     └─ 6. 依 config 建 inbound / outbound handlers（走 ProxyEnvironment scope）

（调用方随后）
instance.Start()   // running=true，级联 Start 所有 feature
instance.Close()   // running=false，级联 Close 并聚合错误
```

第 2、3 步的顺序体现核心思想：**配置驱动的组件先注册，注册过程触发依赖解析；
标准库兜底组件只在缺位时补上**。所以用户配置可以整体替换任何一个内置实现。

## 四、Feature 实例清单（app/ 下的默认实现）

### 功能枢纽类（流量的必经之路）

| 实例结构            | 所在包               | `Type()` 键                   | 职责                                       |
| ------------------- | -------------------- | ----------------------------- | ------------------------------------------ |
| `DefaultDispatcher` | app/dispatcher       | `routing.DispatcherType()`    | 核心枢纽：入站连接进来后由它分发给路由/出站 |
| `inbound.Manager`   | app/proxyman/inbound | `inbound.ManagerType()`       | 持有所有入站 handler，按 tag 索引           |
| `outbound.Manager`  | app/proxyman/outbound | `outbound.ManagerType()`     | 持有所有出站 handler，按 tag 分发           |
| `Router`            | app/router           | `routing.RouterType()`        | 按规则决定流量走哪个出站 tag                |

### 支撑服务类（被枢纽调用）

| 实例结构              | 所在包                   | `Type()` 键                          | 职责                     |
| --------------------- | ------------------------ | ------------------------------------ | ------------------------ |
| dns `Client`          | app/dns                  | `dns.ClientType()`                   | 域名解析，内部管理多个 nameserver |
| `FakeDNSEngine`       | app/dns/fakedns          | `dns.FakeDNSEngineType()`            | fake DNS 池              |
| `policy.Manager`      | app/policy               | `policy.ManagerType()`               | 每连接的超时/限速策略    |
| `stats.Manager`       | app/stats                | `stats.ManagerType()`                | 计数器注册中心           |
| `log.Instance`        | app/log                  | 自身具体类型（特例，见下）           | 日志                     |
| filesystemstorage     | app/persistentstorage    | `ScopedPersistentStorageServiceType` | 持久存储                 |

### 运维/扩展类（旁路，不碰流量主链）

`Commander`（gRPC API）及其各 command 子模块（proxyman/router/stats/observatory/subscription/instman command）、
`Observatory`（含 burst、multi）、`restfulapi`、`instman`、`reverse`、`tun`、`webrtc`、
`browserforwarder`、`subscription`。

**注意**：顶层 `proxy/` 目录里的代理实现（freedom、vmess、vless、trojan…）**不是 Feature**，
它们被 inbound/outbound handler 包装后由管理器持有；但它们同样通过 `RequireFeatures`
拿依赖（freedom ← policy+dns，dns 出站代理 ← dns.Client+policy，loopback ← dispatcher，
wireguard ← dns.Client，vless inbound ← dns.Client）。

## 五、实例之间怎么联系：接口 + RequireFeatures 注入

主机制是**接口注入**：每个 app 在 `init()` 里注册 creator（`common.RegisterConfig`），
creator 内部调 `core.RequireFeatures(ctx, func(依赖...) error { obj.Init(依赖) })`，
依赖凑齐后以**接口**形式存进自己的私有字段。全库的接线点汇总成依赖图：

```
DefaultDispatcher ──→ outbound.Manager, routing.Router, policy.Manager, stats.Manager
Router ────────────→ dns.Client, outbound.Manager, routing.Dispatcher
出站 handler ───────→ dns.Client（域名解析）
dns 各 nameserver ──→ routing.Dispatcher（DoH/TCP DNS 查询走本实例出站）、FakeDNSEngine
Reverse ───────────→ routing.Dispatcher, outbound.Manager
Commander ─────────→ outbound.Manager（注册自己的出站代理入口）
Observatory/burst ─→ outbound.Manager
路由策略 random/leastping/leastload ──→ Observatory
TUN ───────────────→ routing.Dispatcher, policy.Manager
WebRTC ────────────→ dispatcher, outbound.Manager, dns.Client
restfulapi ────────→ stats.Manager
各 command 子模块 ──→ 各自对应的 Manager（如 proxyman/command ← in/out Manager）
```

**循环依赖是这个机制存在的最大理由**：dispatcher 需要 router 做路由决策，router 又需要
dispatcher 把 DNS 查询发出去——互相依赖。构造函数注入无解，而 `RequireFeatures` 的
挂起-重试天然解开：两个对象都先创建、各自挂起 resolution，谁后注册谁触发解析，
双方向对方要到的只是"当时已就绪"的接口引用。

补充三个细节：

1. 少量地方用**运行期 `GetFeature`** 而非装配期注入：proxyman/inbound 创建 handler 时
   现场拿 policy/stats Manager（app/proxyman/inbound/always.go:23）、`AddInboundHandler`
   拿 Manager（v2ray.go:122）、`core.Dial` 拿 dispatcher（functions.go:56）。
2. **log 不走 `GetFeature`**：`app/log.Instance` 的 `Type()` 返回自己的具体类型
   （app/log/log.go:74），生效靠包级 `log.RegisterHandler` 全局注册——任何组件直接
   `log.Record` 就能用，不需要从 Instance 查。
3. 组件之间**从不 import 彼此的具体 struct**，只 import `features/` 下的接口包；
   `GetFeature(dns.ClientType())` 返回的可能是用户配置的任意实现——这就是任何一环
   都能被第三方替换的原因。

## 六、数据流图：一次代理请求的完整旅程

### 主链

```
 客户端
   │ TCP/UDP
   ▼
 [1] inbound.Handler（worker，由 inbound.Manager 按 tag 持有）
   │     协议层 proxy.Process()：认证、解出目标地址 dest
   ▼
 [2] routing.Dispatcher（DefaultDispatcher，核心枢纽）
   │     每条连接：向 policy.Manager 取超时/限速策略，往 stats.Manager 挂计数器
   ▼
 [3] routing.Router.PickRoute(ctx)
   │     域名/IP/端口规则匹配 → 得到出站 tag
   │     （leastping/leastload 策略参考 Observatory 拨测结果选路）
   ▼
 [4] outbound.Manager.GetHandler(tag)
   │
   ▼
 [5] 出站 proxy（freedom / vmess / vless / ...）
   │     目标是域名时：先 dns.Client.LookupIP() 解析
   ▼
 目标服务器
```

各环节说明：

- **[1] 入站 worker**：listener 收到连接后交给 worker（app/proxyman/inbound/worker.go）。
  一个容易迷惑的细节：worker 持有的"dispatcher"实际是一个 `mux.Server` 包装
  （always.go:99 `dispatcher: h.mux`）——开了 mux 的连接先解复用，再进真正的 dispatcher。
- **[2] dispatcher**：`DefaultDispatcher` 持有的 router/policy/stats/outbound 四个接口
  全是装配期注入的（见第五节依赖图），Dispatch 内部完成"取策略 → 记统计 → 问路由"。
- **[3] router**：规则匹配产出 tag；返回的是路由决策而非连接，取连接是下一步的事。
- **[4][5] 出站**：manager 按 tag 取 handler，handler 调具体 proxy 发包。

### DNS 递归旁路（数据流里最绕的一处）

```
 [5] freedom 遇到域名目标
       └─► dns.Client.LookupIP()
             └─► 某个 nameserver（DoH / TCP / UDP classic / fakedns）
                   └─► 查询包自己作为一条新流量进入 [2] Dispatch
                         └─► 同样被路由（走代理或直连）→ 递归走一遍主链
```

DNS 查询本身也是一次"被路由的流量"，这就是 nameserver 要注入 dispatcher、
router 要注入 dns.Client 的原因，也是 dispatcher↔router 循环依赖的现实来源。

### 两条反馈回路

```
 观测回路：Observatory ──定期经 outbound.Manager 拨测各出站──► 拨测结果
                 ▲                                              │
                 └── router 策略（leastping/leastload）选路时读 ◄─┘

 管理回路：Commander（gRPC）/ restfulapi
                 └──AddInboundHandler / RemoveOutboundHandler──► in/outbound.Manager
                        （运行期热改拓扑，走 Instance 的热启动路径）
```

### 总结

主干极短：**worker → dispatcher → router → outbound**，四跳；
一切复杂性都藏在各环节内部（协议解析、规则匹配、代理协议），环节之间只传
接口调用和 ctx（ctx 里带着 `*Instance`、env scope 和 session 元数据，
把这条链上所有组件与 Instance 缝在一起）。

## 七、设计模式小结

| 模式             | 在 Instance 里的体现                                              |
| ---------------- | ----------------------------------------------------------------- |
| Service Locator  | `GetFeature(Type)` 按类型键查找组件；`FromContext(ctx)` 全局取实例 |
| 依赖注入（延迟） | `RequireFeatures` 回调注册 + `AddFeature` 后重试解析              |
| 组合根           | `New` 是唯一的装配点：config → 对象 → feature → 依赖解析          |
| 级联生命周期     | Start/Close 只有两行循环，复杂度全部下放给各 Feature             |
| 能力注入（正交） | env 体系单独管网络/文件系统/存储，并按 app/proxy scope 隔离       |

值得记住的两个"反直觉"点：

1. **Instance 本身几乎是个数据结构**——没有分发逻辑，没有钩子，所有行为都来自
   数组里的 Feature。理解 v2ray 运行时 = 理解这堆 Feature 各自的 Start 做了什么。
2. **并发边界不对称**——Start/Close 有锁，AddFeature/GetFeature 无锁；
   锁的存在是为了让热启动路径（running 翻转）与关闭路径互斥，而不是让运行期
   注册变成线程安全。
