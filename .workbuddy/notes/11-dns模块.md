# 11 - 专题：app/dns——DNS 分发器与 FakeDNS 引擎

> 走读时间：2026-10-22
> 涉及文件：`dns.go`(511) / `nameserver.go`(224) / `fakedns.go`(88) / `fakedns/fake.go`(300)
> 定位：03 课 IpOnDemand 调用的 `dns.Client` 本体；08 课 fakedns 嗅探的引擎

## 一、四层结构

```
DNS（dns.Client feature 本体）
 ├─ hosts: StaticHosts            静态映射（可域名替换）
 ├─ clients: []*Client            每个上游 DNS 一个包装
 │    ├─ server: Server           协议实现（UDP/TCP/DoH/DoQ/Local/FakeDNS）
 │    ├─ domains / expectIPs      域名规则 / GeoIP 校验
 │    └─ queryStrategy / cacheStrategy / fallbackStrategy
 ├─ domainMatcher + matcherInfos  域名 → client 路由（03 课同款 strmatcher）
 └─ fakeDNSEngine                 FakeDNSEngine feature
```

装配期兜底惯例再现：**没有任何 nameserver 配置 → NewLocalDNSClient**（dns.go:81）。

## 二、Server 按 URL 分发（nameserver.go:46）

| 地址写法 | 实现 | 备注 |
|---------|------|------|
| `localhost` | LocalNameServer | 系统 resolv |
| `fakedns` | FakeDNSServer | 假 IP 池 |
| `https://…` | DoH **远程** | RequireFeatures(dispatcher)——**查询本身是一次 Dispatch**，可被路由/走代理/过链式代理 |
| `https+local://…` | DoH 本地 | 直连不走 dispatcher |
| `tcp://` / `tcp+local://` / `quic+local://` | TCP/QUIC 变体 | |
| 其他（IP 形式） | ClassicNameServer | UDP 经典 |

**远程 vs 本地的本质**：远程模式的 DNS 报文作为普通流量进数据面（自由路由+加密），本地模式为避免回环直连。

## 三、查询决策流（lookupIPInternal，见上图左列）

```
① StaticHosts：命中 IP 直接回；命中"域名替换"则换目标继续
② sortClients：domainMatcher 命中 → 规则 client 优先（附 matcherInfos 反查归属）
              其余 client 按 fallback 策略补位（Disabled / DisabledIfAnyMatch）
③ 逐个 QueryIP（4s 超时）：
   错误分类——ErrEmptyResponse 算空不算错，换下一个；
   context.Canceled/DeadlineExceeded 直接终止
④ expectIPs：返回 IP 不在预期 GeoIP 网段 → errExpectedIPNonMatch → 换下一个（防污染）
⑤ 全灭 → 聚合错误
```

**防污染三板斧**：expectIPs 网段校验、远程模式（查询走代理+TLS）、fakedns（根本不真查）。

## 四、FakeDNS 引擎（fakedns/fake.go + dns/fakedns.go）

**Holder 数据结构**（fake.go:19）：
```go
domainToIP cache.Lru      // 域名 ↔ 假 IP 双向 LRU（默认 65535 条）
nextIP     *big.Int       // 递增分配游标
ipRange    *gonet.IPNet   // 默认 198.18.0.0/15（IANA 基准测试保留段，天然不会撞真 IP）
```

- 分配（:113）：LRU 命中返回同 IP（同一域名稳定）；未命中 nextIP++，出池回绕，撞已用 IP 则继续走
- 反查（:138）：IP 在池内 → `GetKeyFromValue` 取域名——**这就是 08 课嗅探零成本还原的引擎内部**
- HolderMulti：v4 池（198.18.0.0/15）+ v6 池（fc00::/18）并行

**规则联动**（dns/fakedns.go:70 GetFakeIPForDomain3）：分配假 IP 前先过 `sortClients`——**fakedns 同样遵守 geosite 域名路由**，可以让国内域名走真解析、国外域名走假 IP。

**与嗅探的闭环**：dns.go:215 `RequireFeatures(dns.Client)` → `v.AddFeature(client.AsFakeDNSEngine())`——FakeDNSEngine 作为独立 feature 注册，08 课嗅探器就是从 Instance 里取的它。

## 五、防死循环的两道闸

1. `IsOwnLink`（dns.go:243）：入站 tag 属于 DNS 自己的 client → 这条链接是自己的出站查询，proxy/dns outbound 直接接管，不再进 DNS 模块
2. 03 课的 `SkipDNSResolve`：DNS 模块发出的查询在路由匹配时跳过 DNS 解析

两道闸都防同一个问题：**"解析 DoH 服务器域名"需要再查 DNS** → 无限递归。

## 速记 + 自测

> **速记**：`hosts → 域名规则排 client → 逐个查询换下一个 → expectIPs 防污染；Server 按 URL 分发，远程模式 DNS 查询也是一次 Dispatch；fakedns = LRU + 198.18/15 递增游标，分配反查 O(1)。`

1. 为什么 DoH 远程模式需要 dispatcher，本地模式不需要？
   （答：远程=查询走完整数据面（可路由、可代理、可复用 mux），必须借 routing.Dispatcher；本地直连 dispatcher 反而可能死循环（DoH 域名自身解析）。）
2. fakedns 的 LRU 满了会发生什么？
   （答：旧映射被淘汰 → 对应假 IP 反查失败 → 嗅探回退普通模式（08 课 fakedns+others）；同时 nextIP 回绕可能撞上仍在用的 IP，代码用 GetKeyFromValue 跳过。）
3. `expectIPs: ["geoip:cn"]` 配在某 DNS 服务器上，返回了 8.8.8.8 会怎样？
   （答：GeoIPMatcher 不命中 → errExpectedIPNonMatch → sortClients 序列里的下一个 client 继续查——这是"用国内 DNS 查国内站，失败自动切换"的经典配置用法。）

## 下一专题候选

- `app/commander`（gRPC 管理 API：运行时增删 handler/inbound）
- `proxy/dns`（DNS 出站：把 DNS 模块暴露成入站可用的出站协议）
