# 数据面设计笔记：一次请求的旅程

> 对应源码：`app/proxyman/inbound/worker.go`、`app/dispatcher/default.go`、
> `app/proxyman/outbound/handler.go`、`proxy/socks/server.go`、`proxy/freedom/freedom.go`。
>
> 本文接着 [core-instance-design.md](./core-instance-design.md) 往下写。上一篇解决的是
> **"对象怎么被装配起来"**（装配期），本篇解决 **"流量怎么被搬走"**（运行期）。
> 两者是两条独立的时间线，混淆它们是最常见的阅读障碍。

---

## 一、先分清两条时间线

| | 装配期 | 运行期 |
| --- | --- | --- |
| 触发 | `core.New(config)` | `instance.Start()` |
| 做什么 | 建对象 → `AddFeature` → 解析依赖 | `running=true`，级联 `Start()` 所有 feature |
| 并发 | 单线程串行（约定） | 每连接一个 goroutine |
| 关键文件 | `v2ray.go` | `worker.go` / `default.go` / `handler.go` |

装配期结束时，所有对象已经互相持有**接口引用**（`RequireFeatures` 注入的）；
运行期只是这些已经握上手的对象开始搬数据。**运行期不再查注册表、不再解析依赖**——
这一点想清楚，`Start()` 那两行循环才不会显得过于简陋。

---

## 二、`transport.Link` 与 pipe：全库最关键的抽象

### 2.1 `getLink` 到底造了什么

`DefaultDispatcher.getLink`（default.go:135）每次调用造两个 pipe，共四个端点：

```go
uplinkReader, uplinkWriter     := pipe.New(opt...)   // 请求方向
downlinkReader, downlinkWriter := pipe.New(opt...)   // 响应方向

inboundLink  := &transport.Link{Reader: downlinkReader, Writer: uplinkWriter}
outboundLink := &transport.Link{Reader: uplinkReader,  Writer: downlinkWriter}
```

于是：

- **入站侧**拿到 `inboundLink`：往 `uplinkWriter` 写请求，从 `downlinkReader` 读响应；
- **出站侧**拿到 `outboundLink`：从 `uplinkReader` 读请求，往 `downlinkWriter` 写响应。

**为什么是两条而不是一条**：TCP 是全双工的，请求与响应必须能同时流动。
一个 `pipe` 是单向的，两侧 goroutine 各自读写不同端点，才不会出现"自己写自己读"。
这两条 pipe 就是入站与出站之间**唯一的耦合面**。

由此得出一个必须记住的结论：

> 入站 `proxy.Process` 与出站 `proxy.Process` **不在同一条调用栈上**。
> 它们是两个并发 goroutine，通过这两条内存管道通信。
> 打断点时你会发现栈里根本看不到对侧——这是正常的，不是你读错了。

### 2.2 `Dispatch` 是一次"异步握手"

```go
func (d *DefaultDispatcher) Dispatch(ctx, destination) (*transport.Link, error) {
    ob := &session.Outbound{Target: destination}
    ctx = session.ContextWithOutbound(ctx, ob)
    inbound, outbound := d.getLink(ctx)
    ...
    go d.routedDispatch(ctx, outbound, destination)   // default.go:220 / :222
    return inbound, nil                                // 立刻返回
}
```

`Dispatch` **立即返回**给入站一个可用的 `Link`，真正的路由决策在 goroutine 里做。
这带来两个后果：

1. **错误不走返回值**。路由失败、拿不到 handler 时（default.go:341-350），
   处理方式是 `common.Close(link.Writer)` + `common.Interrupt(link.Reader)`——
   用**打断管道**来通知对侧，调用方只能从读写错误里感知。
2. **入站可以先写数据**。管道有缓冲，入站不等路由完成就能往里写请求。

### 2.3 流量嗅探（sniffing）会改写目标地址

开了 sniffing 时，`outbound.Reader` 被替换成 `cachedReader`（default.go:223-226）：
先偷偷读一段数据识别协议，**再把数据塞回缓存**给后续读取，最多尝试 2 次、
总预算 200ms（default.go:259-287）。识别出域名且命中 `OverrideDestinationForProtocol`
时，会**直接改写 `destination.Address`**（default.go:231-237）。

即：路由决策看到的 dest，未必是客户端原始请求里的 dest。

---

## 三、四跳逐跳拆解

### 跳 1：listener → `tcpWorker.callback`（worker.go:59）

```go
func (w *tcpWorker) callback(conn internet.Connection) {
    ctx, cancel := context.WithCancel(w.ctx)
    ctx = session.ContextWithID(ctx, session.NewID())       // 每条连接一个 ID
    ctx = session.ContextWithInbound(ctx, &session.Inbound{  // 源地址 / 网关 / tag
        Source:  net.DestinationFromAddr(conn.RemoteAddr()),
        Gateway: net.TCPDestination(w.address, w.port),
        Tag:     w.tag,
    })
    ctx = session.ContextWithContent(ctx, content)           // sniffing 开关
    if err := w.proxy.Process(ctx, net.Network_TCP, conn, w.dispatcher); err != nil { ... }
}
```

三个要点：

- **`w.dispatcher` 其实是 `h.mux`**（always.go:120，`dispatcher: h.mux`）。
  类型是 `routing.Dispatcher`，值是 `*mux.Server`——`mux.Server` 同样实现了 `Dispatch`，
  开了 mux 的连接先在这里解复用。你的笔记里提过这点，值得再强调一次。
- **ctx 是唯一的贯穿物**：session ID、Inbound、Outbound、Content（含 sniffing 配置）
  全部挂在 ctx 上，一路传到最后。要理解任何一处，先看清那个 ctx 里有什么。
- **统计计数器在这里插桩**：有 uplink/downlink counter 时，conn 被包成
  `internet.StatCouterConnection`（worker.go:95-101）。

UDP 走的是完全不同的路径：`udpWorker` + `activeConn map[connID]*udpConn`，
靠 `task.Periodic` 每 16 秒清理、超 8 秒无活动即回收（worker.go:363、:394）。

### 跳 2：入站协议 `Process`（以 socks 为例，server.go:65）

`proxy.Inbound` 的契约只有两个方法（proxy/proxy.go:23）：

```go
type Inbound interface {
    Network() []net.Network
    Process(context.Context, net.Network, internet.Connection, routing.Dispatcher) error
}
```

socks 的 `processTCP`（server.go:82）做三件事：

1. `svrSession.Handshake(reader, conn)` —— SOCKS5 握手，解出 `dest`；
2. 用 `handshakeFinalizingDispatcher`（server.go:133）包一层 dispatcher，
   **保证 SOCKS 的最后一字节 reply 只在 `Dispatch` 成功返回后才发给客户端**；
3. `dispatcher.Dispatch(ctx, dest)` 拿 Link（server.go:188），
   然后 `task.Run(ctx, requestDonePost, responseDone)` 双向 `buf.Copy`。

### 跳 3：`DefaultDispatcher`（default.go:203 → :300）

`routedDispatch` 是真正的分发逻辑，优先级从高到低：

```
1. ctx 里有 forcedOutboundTag  →  直接用这个 tag（平台接口指定）
2. router.PickRoute(ctx)       →  规则匹配得出 tag
3. 都没有                      →  ohm.GetDefaultHandler()
```

拿到 handler 后 `handler.Dispatch(ctx, link)`（default.go:362）——
注意传的是 **outboundLink**，不是 inboundLink。

`DefaultDispatcher` 自己几乎不干活：四个依赖（outbound.Manager / Router /
policy.Manager / stats.Manager）全是装配期注入的（default.go:104），
`Start`/`Close` 都是空实现（default.go:128-133）。它是纯粹的**接线端子**。

### 跳 4：出站 `Handler.Dispatch`（handler.go:153）

```go
func (h *Handler) Dispatch(ctx context.Context, link *transport.Link) {
    if 配了 DomainStrategy && 目标是域名 {
        outbound.Target.Address = h.resolveIP(...)     // handler.go:160-164
    }
    if h.mux != nil && (开启 mux) {
        h.mux.Dispatch(ctx, link)
    } else {
        h.proxy.Process(ctx, link, h)                  // ← 注意第三个参数是 h 自己
    }
}
```

**这里有个很关键的"环"**：`h.proxy.Process(ctx, link, h)` 把 `Handler` 自己
作为 `internet.Dialer` 传给了协议实现。所以 freedom 里写的
`dialer.Dial(ctx, destination)`（freedom.go:130）实际调回来的就是
`Handler.Dial`（handler.go:196）。

这个环是 v2ray 的**代理链**能力所在——`Handler.Dial` 里如果
`senderSettings.ProxySettings` 配了 tag，它会再取另一个 handler 并
`go handler.Dispatch(...)`（handler.go:198-212），形成"出站套出站"。

### 终点：`freedom.Process`（freedom.go:95）

```go
err := retry.ExponentialBackoff(5, 100).On(func() error {   // 5 次、100ms 起
    conn, err = dialer.Dial(ctx, destination)
    ...
})
...
task.Run(ctx,
    requestDone,                              // buf.Copy(input → conn)
    task.OnSuccess(responseDone, task.Close(output)))   // buf.Copy(conn → output)
```

`task.Run` 的语义是**两个函数任一结束就取消另一个**，配合
`signal.CancelAfterInactivity` 做空闲超时。这就是"半关闭"能被正确处理的原因。

---

## 四、用 `test_socks_chain.py` 走一遍完整时序

脚本里的 SOCKS5 握手 → v2ray 内部：

```
[脚本] TCP 连到 127.0.0.1:10808
         │
         ▼
[1] tcpWorker.callback            worker.go:59    造 ctx，包 mux
[2] socks Server.processTCP       server.go:82    握手解出 dest=127.0.0.1:18080
[3] DefaultDispatcher.Dispatch    default.go:203  getLink 造两条 pipe
      └─ goroutine: routedDispatch default.go:300  PickRoute → tag → GetHandler
[4] outbound.Handler.Dispatch     handler.go:153  → freedom.Process(ctx, link, h)
[5] freedom.Process               freedom.go:95   dialer.Dial = Handler.Dial
      └─ internet.Dial            handler.go:276  真实 TCP 连到 18080
[6] 双向 buf.Copy：脚本请求 → uplink pipe → freedom → 目标；响应沿 downlink pipe 回来
[7] 脚本收到 "HTTP/1.1 200 OK" + BODY，断言通过
```

想要亲眼验证，可以临时把日志级别调到 debug，日志里会依次出现：
`creating stream worker on ...` → `TCP Connect request to ...` →
`opening connection to ...` → `taking detour [tag] for [dest]`。

---

## 五、容易踩坑的细节

1. **`w.dispatcher` 不是 `DefaultDispatcher`**，是 `mux.Server`（always.go:120）。
   查调用链时别在这里断掉。
2. **路由错误不通过返回值暴露**，而是靠打断管道传递（default.go:347-348）。
   "连接莫名其妙断了"往往要往 routedDispatch 里找。
3. **sniffing 会改写 dest**（default.go:234），路由看到的地址可能已被替换。
4. **`Handler` 既是出站处理器又是 `Dialer`**，这个自引用是代理链与
   packetaddr、安全引擎等能力统一挂载的入口（handler.go:196-278）。
5. **统计插桩有三处**：入站 worker（按连接包 conn）、
   `getLink`（按 user 包 Writer，default.go:156-176）、
   出站 `Handler.Dial`（包 conn，handler.go:296）。查流量对不上时三处都要看。
6. **log 不走 `GetFeature`**（见上一篇笔记），所以别在日志里找 Feature 注册痕迹。
7. **DNS 是递归的**：出站解析域名 → `dns.Client` → 查询流量本身又进一次
   `Dispatcher`。这就是 dispatcher ↔ router ↔ dns 循环依赖的现实来源。
8. **`Close` 是级联且不保证幂等**：`Instance.Close` 遍历所有 feature 调 `Close`
   并聚合错误（v2ray.go:299-316）；`Start` 失败后实例状态是 *unknown*，
   且规定只能启动一次。

---

## 六、下一步读什么

按"越靠前越该先读"排列：

1. `common/session/` —— 读懂 ctx 里到底装了什么（ID / Inbound / Outbound / Content / 属性），
   这是所有代码的隐含参数。
2. `transport/pipe/` —— 几百行，理解 `Reader`/`Writer` 的阻塞、中断与缓冲语义。
3. `common/buf/` —— `MultiBuffer`、`buf.Copy`、缓冲池。v2ray 性能的一半在这里。
4. `common/task/` + `common/signal/` —— `task.Run` 与 `CancelAfterInactivity`，
   半关闭与空闲超时的实现基础。
5. `app/router/` —— 规则匹配与 `PickRoute`；`app/dns/` —— 递归 DNS 与 fakedns。
6. `transport/internet/` —— 传输层（TCP/UDP/TLS/WS/QUIC…），349 个文件，最后再碰。
7. `infra/conf/` —— JSON → proto 的配置层，建议回头看：
   不看它不影响理解运行时，看了才能理解配置为何那样写。

一句话总结主干：**入站造 ctx 和解 dest → dispatcher 用两条 pipe 缝住两侧 →
router 选 tag → 出站 handler 拨号并双向拷贝**。四跳，其余全是支路。
