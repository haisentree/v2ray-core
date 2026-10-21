# app/ 目录阅读指南

> 面向 `/root/Project/Git-temp/v2ray-core/app/`。
> 前置阅读：[core-instance-design.md](./core-instance-design.md)（装配期）、
> [data-plane-design.md](./data-plane-design.md)（运行期）。
>
> `app/` 是 Feature 的**默认实现**所在。要读懂它，先记住一句：
> **`features/` 定义接口，`app/` 给出实现，两者对着看。**

---

## 一、先看清真实代码量

`app/` 表面有 259 个 go 文件、约 3.9 万行，看着吓人。但把三类东西剔掉后：

| 类别 | 说明 |
| --- | --- |
| `*.pb.go` | protobuf 生成代码（含 `*_grpc.pb.go`） |
| `*_test.go` | 测试 |
| `各 */command/` | gRPC 管理服务，与数据面无关 |

**净代码只有约 1.7 万行**，其中压在主链上的七个目录合计 **7843 行**：

| 目录 | 净行数 | 净文件数 | 性质 |
| --- | --- | --- | --- |
| `dns` | 3137 | 16 | 主链 · 最难 |
| `proxyman` | 1661 | 9 | 主链 · 数据面 |
| `router` | 1605 | 11 | 主链 · 决策 |
| `dispatcher` | 664 | 6 | 主链 · 枢纽 |
| `stats` | 371 | 4 | 主链 · 支撑 |
| `log` | 230 | 3 | 主链 · 支撑 |
| `policy` | 175 | 4 | 主链 · 支撑 |
| `webrtc` | 3596 | 9 | 旁路 |
| `subscription` | 1900 | 49 | 旁路 |
| `tun` | 1478 | 24 | 旁路 |
| `observatory` | 946 | 11 | 旁路 |
| `reverse` | 582 | 5 | 旁路 |
| `commander` | 456 | 6 | 旁路（gRPC 总入口） |
| `persistentstorage` | 189 | 3 | 旁路 |
| `restfulapi` | 184 | 4 | 旁路 |
| `browserforwarder` | 138 | 2 | 旁路 |
| `instman` | 94 | 2 | 旁路 |

注意两个反直觉的点：

- **`webrtc` 净量 3596 行，是全目录最大的**，但它完全在旁路，一点都不碰主链。
  不要因为"它最大"就先读它。
- **`subscription` 有 49 个文件却只有 1900 行**，平均一个文件 39 行——很碎，
  适合要用时再翻，不适合通读。

---

## 二、通用读法：四步拆一个 Feature

`app/` 下 24 个 Feature 实现全是同一个套路。按固定顺序看四处，十分钟能吃完一个：

### ① `RegisterConfig` —— 创建入口

通常在文件末尾或开头的 `init()` 里。看它就知道这个对象**从哪个配置类型被造出来**。

### ② `Type()` —— 它注册成谁

返回的是"接口类型的 nil 指针"，如 `return routing.DispatcherType()`。
**一般就一行**，但这一行决定了它在 `Instance` 里的身份。

### ③ `Init` / `RequireFeatures` 的参数 —— 参数列表即依赖

这是**信息密度最高的一处**。回调函数的每个参数就是一个依赖，读完就得到了这个
Feature 的入边，画依赖图靠的就是这里。

### ④ `Start()` / `Close()` —— 生命周期

看它管了什么资源（listener、后台 goroutine、定时器）。

### 实例：`app/dispatcher/default.go`

```go
func init() {
    common.Must(common.RegisterConfig((*Config)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {   // ①
        d := new(DefaultDispatcher)
        if err := core.RequireFeatures(ctx, func(om outbound.Manager, router routing.Router,
            pm policy.Manager, sm stats.Manager) error {                                                                     // ③ 四个依赖
            return d.Init(config.(*Config), om, router, pm, sm)
        }); err != nil {
            return nil, err
        }
        return d, nil
    }))
}

func (*DefaultDispatcher) Type() interface{} { return routing.DispatcherType() }   // ②
func (*DefaultDispatcher) Start() error      { return nil }                        // ④ 空实现
func (*DefaultDispatcher) Close() error      { return nil }                        // ④ 空实现
```

④ 是空实现——因为 dispatcher 纯做接线，没有资源可管。**这类 Feature 读完 ①②③ 就够了**，
别在 ④ 上找东西。

---

## 三、建议阅读顺序

按"依赖少、能独立理解"到"依赖多、需要前序知识"排：

### 第 1 批：热身（约 600 行）

小而完整，目的是把四步法练熟。

| 顺序 | 入口文件 | 看点 |
| --- | --- | --- |
| 1 | `policy/manager.go`（68 行） | 最小的 Feature，超时与限速策略 |
| 2 | `stats/stats.go` → `stats/channel.go` | 计数器注册中心 + 发布订阅 channel |
| 3 | `log/log.go`（172 行） | 唯一不走 `GetFeature` 的特例，靠包级全局注册 |

### 第 2 批：枢纽（约 664 行）

- `dispatcher/default.go`（363）—— 核心枢纽，见上一篇笔记
- `dispatcher/sniffer.go`（144）、`fakednssniffer.go`（120）—— 流量嗅探，可选

`dispatcher/dispatcher.go` 只有 3 行，是空壳，别在它身上浪费时间。

### 第 3 批：数据面（约 1661 行）

```
proxyman/inbound/inbound.go     (180)  Manager 实现，Type/AddHandler
  └─ always.go                  (187)  AlwaysOnInboundHandler，组装 worker + mux
       └─ worker.go             (528)  tcpWorker / udpWorker，真正的收包处
  └─ dynamic.go                 (203)  按需启动的入站，可跳过

proxyman/outbound/outbound.go   (179)  Manager 实现，按 tag 索引
  └─ handler.go                 (327)  出站 Handler，兼 Dialer（代理链的环在这里）
```

建议按 `inbound.go → always.go → worker.go` 的顺序，自顶向下。

### 第 4 批：决策（约 1605 行）

```
router/router.go            规则匹配主循环、PickRoute
  ├─ condition.go           条件：域名 / IP / 端口 / 协议…
  ├─ condition_geoip.go     GeoIP 匹配
  ├─ balancing.go           负载均衡（含 roundrobin）
  ├─ strategy_random.go     最简策略，先读这个
  ├─ strategy_leastping.go  依赖 Observatory
  ├─ strategy_leastload.go  依赖 Observatory
  └─ weight.go              权重
```

策略三选一先读 `strategy_random.go`，另两个要等读完 `observatory` 才顺。

### 第 5 批：最绕的一块（约 3137 行）

```
dns/dns.go              Client 实现、LookupIP、缓存
  ├─ nameserver.go      nameserver 接口
  ├─ nameserver_local.go / _udp.go / _tcp.go / _doh.go / _quic.go / _fakedns.go
  ├─ hosts.go           静态 hosts
  ├─ fakedns.go         FakeDNS 引擎
  └─ dnscommon.go       公共逻辑
```

**难点在于递归**：DNS 查询本身是一条流量，会再进一次 `Dispatcher`。
这正是 dispatcher ↔ router ↔ dns 循环依赖的由来。读之前务必先吃透第 2、4 批。

---

## 四、明确可以先跳过的

| 跳过对象 | 理由 |
| --- | --- |
| 所有 `*.pb.go`、`*_grpc.pb.go` | protobuf 生成物，想看字段定义去读同目录的 `*.proto` |
| 所有 `*_test.go` | 要动手改时再回来看 |
| 所有 `*/command/` 子目录 | gRPC 管理接口（proxyman/router/stats/log/observatory/subscription/instman 各有其一），纯运维旁路 |
| `webrtc`、`subscription`、`tun` | 净量不小但完全旁路，按需翻 |
| `restfulapi`、`instman`、`browserforwarder` | 外围小模块 |

---

## 五、三个实用 grep

```bash
cd /root/Project/Git-temp/v2ray-core

# 1. 列出所有 Feature 实现（24 个，去掉 _test）
grep -rn "func (.*) Type() interface{}" --include="*.go" app/

# 2. 画出依赖图：每个 Feature 的依赖一览
grep -rn "RequireFeatures" --include="*.go" app/

# 3. 找某个 Feature 的所有配置注册点
grep -rn "common.RegisterConfig" --include="*.go" app/
```

第 2 条尤其有用：把 `RequireFeatures` 后的回调参数抄下来，就是你在
`core-instance-design.md` 第五节手画的那张依赖图的**程序化版本**。

---

## 六、一句话总结

> 先 `policy → stats → log` 热身，再 `dispatcher` 看枢纽，
> 然后 `proxyman` 走完数据面，`router` 理解决策，最后啃 `dns`。
> 每个目录都用"① 注册 → ② Type → ③ 依赖参数 → ④ 生命周期"四步拆。
> `*.pb.go`、`*_test.go`、`*/command/` 一律先跳过。
>
> 真正要读的不到 8000 行，不是 3.9 万行。
