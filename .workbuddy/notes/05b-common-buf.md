# 05b - 进阶（下）：common/buf 零拷贝缓冲体系

> 走读时间：2026-10-22
> 涉及文件：`common/bytespool/pool.go`、`common/buf/buffer.go` / `multi_buffer.go` / `copy.go` / `reader.go`
> 这是数据面的地基——02 课所有 `buf.Copy` 背后的机制

## 一、bytespool：四级内存池（pool.go）

```go
numPools=4, sizeMulti=4 → 2K / 8K / 32K / 128K 四级 sync.Pool
GetPool(size)  按需取池；Alloc/Free 配对使用
```

- 常规 `buf.Size = 2048`：一次 syscall 读满 2K，吞吐与内存占用的平衡点。
- 超过 128K 的请求直接 `make`，不入池。

## 二、Buffer：游标式复用（buffer.go:28）

```go
type Buffer struct {
    v         []byte    // 底层数组（池里借的）
    start     int32     // 读游标
    end       int32     // 写游标
    ownership ownership // managed / unmanaged / bytespools
}
```

三个关键设计：
1. **读取不搬数据**：`Read` 只把 `start` 前移，底层数组原地不动——零拷贝消费。
2. **所有权标记**：`FromBytes` 包装外来 slice 时标 `unmanaged`，`Release()` 直接跳过归还——避免把不属于自己的内存塞回池（会污染池）。
3. **`StackNew()` 返回值而非指针**（buffer.go:61）：注释写明"released in the same function"——生命周期不逃逸出当前函数的 buffer 用它，配合 Go 逃逸分析留在栈上，连 pool 往返都省掉。trojan 的 `writeHeader` 就在用。

Release 是**全项目的内存纪律**：从 `ReadMultiBuffer` 拿到的 mb，调用方用完必须 `ReleaseMulti`；忘了就是泄漏，double-release 会把同一块内存放两次回池（数据竞争隐患）。

## 三、MultiBuffer：指针数组即链表（multi_buffer.go:27）

```go
type MultiBuffer []*Buffer    // 有序 Buffer 指针数组
```

- **一次携带 2K×N 数据**：`Reader/Writer` 接口（reader.go/writer.go）以 `ReadMultiBuffer/WriteMultiBuffer` 为单位，减少 syscall 次数；
- `MergeMulti / SplitBytes / SplitFirst`：拼接与切分**全部是指针操作**，不 memcpy payload；
- Go 惯用法的变通：函数返回"MultiBuffer 新地址"（`mb = SplitFirst(mb, b)`），模拟链表删除——nil 槽位压缩在尾部。

## 四、buf.Copy：双向对拷的引擎（copy.go:79）

```go
func copyInternal(reader, writer, handler) error {
    for {
        buffer, err := reader.ReadMultiBuffer()
        if !buffer.IsEmpty() {
            for _, h := range handler.onData { h(buffer) }   // 钩子
            if werr := writer.WriteMultiBuffer(buffer); werr != nil {
                return writeError{werr}                       // 错误分类包装
            }
        }
        if err != nil { return readError{err} }
    }
}
```

- **onData 钩子**：02 课到处见的 `buf.UpdateActivity(timer)`（每次拷贝喂看门狗，空闲超时的机制基础）和 `CountSize`（流量统计）都从这里注入；
- **readError/writeError 分类**：02 课 `task.Run(requestDone, responseDone)` 的双向 goroutine 各自拿到的错误能区分方向——读端 EOF → 正常关闭写端；写端失败 → 中断读端。全靠这层包装；
- `CopyOnceTimeout`（copy.go:113）：限时读一个 MultiBuffer——05 课 trojan/vmess "首包合并抗时序探测"就是靠它。

进阶彩蛋：`readv_reader.go` + `readv_unix.go`——用系统 `readv()` 一次调用填多个 Buffer，进一步压 syscall。

## 五、串回主线：一条数据的一生

```
socks 客户端发来 1KB
→ BufferedReader.ReadMultiBuffer（2K Buffer 从池借出）
→ socks requestDone: buf.Copy(reader, link.Writer)
→ uplink pipe（MultiBuffer 进队列，零 memcpy）
→ freedom requestDone: buf.Copy(input, conn writer)
→ conn（TCP+TLS）写出 → writer 侧 ReleaseMulti 归还池
→ 同一块 2K 内存回到池中，等下一条连接借走
```

一条 2K 缓冲在池里反复流转，全程只有网卡 ↔ 内核一次真实拷贝。

## 速记 + 自测

> **速记**：`2K 池 + start/end 游标零拷贝读 + MultiBuffer 指针数组批量读写 + onData 钩子喂狗计数 + read/write 错误分方向 + Release 全民纪律`。

1. 为什么 `FromBytes` 要标 `unmanaged`？
   （答：防止把调用方拥有的内存 Release 回池——那块内存可能还被外部引用，回池复用会造成数据竞争/串包。）
2. `StackNew()` 与 `New()` 的区别只在返回类型，为什么值得存在？
   （答：值类型 + 不逃逸 → Go 分配在栈上，省掉堆分配和 sync.Pool 往返；适用条件是"同函数内释放"，注释明确写了。）
3. Copy 的错误为什么必须区分读/写方向？
   （答：双向对拷时，一侧失败要精确中断另一侧（Interrupt 对端 link），且读 EOF 是正常关闭、写失败才是异常——语义完全不同。）

---

## 学习路径收官

00 总览 → 01 启动 → 01b 配置装配 → 02 数据流 → 03 路由 → 04 传输层 → 05 协议 → 05b buf。
从 `v2ray run` 到一块 2K 内存的完整旅程已全部走通。接下来可选方向：vmess AEAD 细节（`proxy/vmess/aead/`）、DNS 模块（`app/dns/`）、gRPC 管理 API（`app/commander/`）、或动手改代码（比如给某个协议加个 feature）。
