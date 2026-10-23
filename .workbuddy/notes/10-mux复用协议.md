# 10 - 专题：common/mux——v1.mux.cool 单连接复用协议

> 走读时间：2026-10-22
> 涉及文件：`frame.go`(145) / `session.go`(160) / `server.go`(252) / `client.go`(414) —— 全包约 1555 行
> 定位：焊上 07 课（入站侧的 mux.Server）与 09 课（出站侧的 ClientManager）

## 一、帧协议（frame.go:43 注释就是规范）

```
[2B length][2B sessionID][1B status][1B option] + payload
（status == New 时追加：[1B network][2B port][address]）
```

- **四种 status**：`New`(开新会话，唯一带目标地址) / `Keep`(会话数据) / `End`(关会话，Option=0x02 表示异常) / `KeepAlive`
- **option**：0x01=帧内有数据（决定要不要继续读 payload），0x02=错误
- 只有 New 帧带地址——后续数据帧 6 字节头 + 纯 payload，开销极小

**v1.mux.cool 魔法地址**（client.go:188）：`v1.mux.cool:9527`——复用连接的"伪装目标"，全部帧都在这条到虚构域名的连接里传。

## 二、服务端（server.go）：从帧到真 Dispatch

`Server` 本身实现 `routing.Dispatcher`（所以 07 课 worker 的 dispatcher 可以是它）：

```go
func (s *Server) Dispatch(ctx, dest) {
    if dest.Address != muxCoolAddress {
        return s.dispatcher.Dispatch(ctx, dest)   // 非 mux 流量原样透传
    }
    // 是 mux 连接：造双管道，起 ServerWorker 解帧
}
```

`ServerWorker.run` 循环 `handleFrame`，按 status 四路分发：

| 帧 | 处理（server.go:111-202） |
|----|--------------------------|
| New | `dispatcher.Dispatch(meta.Target)` 建真会话 → 存 SessionManager → `go handle()` 负责回程；若帧带首包直接写入 |
| Keep | 按 sessionID 查会话 → 帧内数据写进会话 output（去往目标）|
| End | 关会话（OptionError 则中断双向）|
| KeepAlive | 数据直接 Discard |

回程（handle :85）：`buf.Copy(s.input, ResponseWriter)` —— 目标响应编码成帧写回 mux 连接。

## 三、客户端（client.go）：从 Dispatch 到帧

**三层结构**：

```
ClientManager.Dispatch（09 课 handler.go:166 的入口）
  └─ IncrementalWorkerPicker：挑空闲 ClientWorker
       （无空闲 → DialingWorkerFactory 现场拨一条新 mux 连接；30s 清理已关闭的）
       └─ ClientWorker：一条复用连接
            └─ SessionManager：map[uint16]*Session（sessionID 递增分配）
```

**关键流程**：
- `DialingWorkerFactory.Create`（:147）：造双 pipe（64KB 上限）→ goroutine 用真实出站协议 `Process` 到 `v1.mux.cool:9527`——**复用连接本身就是一次普通出站**，因此天然继承 04 课的全部能力（WS+TLS 承载、sockopt……）
- `ClientWorker.Dispatch(ctx, link)`（:301，09 课被调处）：`Allocate()` 分配 sessionID，`s.input=link.Reader, s.output=link.Writer`，`go fetchInput`
- `fetchInput`（:254）：`writeFirstPayload`（100ms CopyOnceTimeout，超时发空 New 帧——**05 课首包合并模式再现**）→ `buf.Copy` 把用户数据编码成 New/Keep 帧写入复用连接
- `fetchOutput`（:377）：读复用连接回程帧，Keep 帧数据按 sessionID 写回对应 `s.output`
- `monitor`（:221）：16s ticker，**无活跃会话即关闭整条复用连接**；`IsFull` 按 MaxConcurrency（活跃数）/MaxConnection（历史总数）限流

**对称性**：client 的 `fetchOutput` 与 server 的 `run` 是镜像（同样的 handleStatusXxx 四路分发）；client 的 `fetchInput` 与 server 的回程 `handle` 亦互为镜像。**一份帧协议，两端各写一遍编码器和解码器**。

## 四、调度集成：07/09 课焊点全景（见上图）

```
入站侧：worker.dispatcher = mux.Server
        dest ≠ v1.mux.cool → 原样透传（07 课"隐形中转层"的真相）
        dest = v1.mux.cool → ServerWorker 解帧 → 按会话真 Dispatch
出站侧：handler.mux = ClientManager
        mux 启用 → Dispatch 走复用通道
        复用连接本身 = 一次普通出站（可能套 WS+TLS）
```

## 五、收益与代价

| 收益 | 代价 |
|------|------|
| N 个请求共用 1 次 TLS 握手 | 一条连接挂掉，N 个会话全灭 |
| 多目标混流 → 抗流量分析 | 队头阻塞：慢目标拖累同连接的其他会话 |
| 帧头仅 6 字节 | 需要 New/Keep/End 生命周期管理 |

## 速记 + 自测

> **速记**：`6 字节帧头四状态，New 帧才带地址；两端镜像编解码；magic 地址 v1.mux.cool 过 Server 透传闸门；复用连接自己也是一次普通出站。`

1. 为什么 Server.Dispatch 要判断 dest 是否为 muxCoolAddress？
   （答：worker 把**所有**连接都交给 mux.Server（07 课），绝大多数是普通目标必须原样透传；只有客户端显式把复用连接导向 v1.mux.cool 才进入解帧模式——一个地址充当协议开关。）
2. `writeFirstPayload` 超时后为什么还要发空 New 帧？
   （答：让服务端立即创建会话并开始回程（如先返回 TLS 握手所需数据）；若等首包，请求-响应式协议会死锁。与 05 课 trojan 首包合并是同一模式的正反两面。）
3. mux 为什么默认不推荐给"单用户低并发"场景？
   （答：收益来自摊薄握手与混流；低并发时复用连接长期只跑 1 个会话，白白承担队头阻塞和单点故障风险。）

## 下一专题候选

- `app/dns`（DNS 分发器、fakedns 引擎本体——08 课嗅探的另一半）
- `app/commander`（gRPC 管理 API：运行时增删 handler）
