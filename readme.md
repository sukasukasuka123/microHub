# microHub

基于 gRPC 双向流的轻量级微服务调度框架。Hub 作为调度中心，通过长连接流池向多个 Tool 并发派发任务，Tool 以流式帧返回结果。

---

## 架构概览

```
外部调用者
    │  hub.Dispatch(ctx, req)
    ▼
┌─────────────────────────────────┐
│  BaseHub                        │
│  ├── HubHandler（业务方实现）    │  ← 路由策略、结果回调
│  ├── poolManager                │  ← addr → StreamPool 映射
│  │     └── StreamPool × N       │  ← 每个 Tool 一个流池
│  │           └── SingleStream×M │  ← min/max 条双向流
│  └── timerLoop（可选）           │  ← 定时广播
└─────────────┬───────────────────┘
              │  gRPC DispatchStream（双向流）
    ┌─────────┴──────────┐
    ▼                    ▼
┌─────────┐          ┌─────────┐
│ BaseTool│          │ BaseTool│
│ hello   │          │ world   │
│ :50052  │          │ :50053  │
└─────────┘          └─────────┘
```

**星形拓扑**：Hub 是唯一的调度中心，Tool 只和 Hub 通信，Tool 之间不直接连接。每个 Tool 地址对应一个 `StreamPool`，池内维护 min～max 条长连接双向流，请求发完立即归还流，响应通过 `pendingTask` channel 异步接收。

---

## 数据流

以 `Dispatch(ctx, req{Method:"Hello", count:3})` 为例：

```
BaseHub.Dispatch
  └─ HubHandler.Execute(req)          // 业务方路由，返回 DispatchTarget 列表
       └─ dispatchAll(targets)
            └─ go callStream(addr, req)  // 每个 target 一个 goroutine
                 ├─ proto.Clone(req)     // 深拷贝，并发安全
                 ├─ req.TaskId = newTaskID()
                 ├─ StreamPool.Get()     // 从池中借一条流
                 ├─ SingleStream.Send(req)
                 │    ├─ pending.Store(taskId, pendingTask{resChan, done})
                 │    └─ stream.Send(req)   // → gRPC → Tool 进程
                 ├─ StreamPool.Put()     // 立即归还！流继续复用
                 └─ 等待：select { resChan / done / ctx }

Tool 进程（BaseTool）：
  recvLoop → taskCh → executeLoop → go executeOne
    └─ ToolHandler.Execute(req)     // 业务方实现，返回 <-chan *ToolResponse
         └─ goroutine 生产帧：
              ch <- PartialResp(loop_idx=1)   // status="partial"
              ch <- PartialResp(loop_idx=2)
              ch <- OKResp(loop_idx=3)        // status="ok"，最后一帧
              close(ch)
    └─ respCh → sendLoop → stream.Send(resp) × 3

Hub 侧 SingleStream.recvLoop：
  stream.Recv() × 3
    └─ dispatch(resp)
         ├─ partial 帧 → task.resChan <- resp
         ├─ partial 帧 → task.resChan <- resp
         └─ ok 帧 → pending.Delete → task.resChan <- resp → task.finish()

callStream 收集：
  [partial(1), partial(2), ok(3)] → return []DispatchResult
```

**帧协议约定**：`status="partial"` 为中间帧，框架继续等待；`status="ok"` 或 `status="error"` 为结束帧，框架触发 `finish()` 结束收集。**所有多帧响应必须遵守此约定**，否则只会收到第一帧。

### 更加详细的解读：

#### Hub 架构解读

##### 整体结构

```
外部调用者
    │  Dispatch(ctx, req)
    ▼
 BaseHub
    ├── handler (HubHandler)     ← 业务方实现，决定路由
    ├── pm (poolManager)         ← addr → StreamPool 映射
    │       └── StreamPool × N  ← 每个 Tool 地址一个
    │               ├── grpc.ClientConn   ← 底层 TCP 连接（唯一）
    │               ├── HubServiceClient  ← 缓存，不重复 alloc
    │               └── Pool[*SingleStream] ← 流池，min/max 条流
    │                       └── SingleStream × M
    │                               ├── recvLoop goroutine
    │                               └── pending sync.Map
    └── ctx/cancel               ← 生命周期控制
```

星形拓扑：Hub 是中心，每个 Tool 对应一个 `StreamPool`，池内有多条 `SingleStream`，每条流是一个独立的 gRPC 双向流。

---

##### 数据流：一次完整的 `Dispatch`

以 `Dispatch(ctx, req{"Method":"Hello"})` 为例，count=3（3帧响应）：

```
─────────────────────────────────────────────────────────────
阶段 1：路由
─────────────────────────────────────────────────────────────

BaseHub.Dispatch(ctx, req)
  │
  ├─ handler.Execute(req)
  │     └─ MainHubHandler.routeByMethod(req)
  │           └─ registry.SelectToolByMethod("Hello")
  │                 └─ 返回 {Addr:"localhost:50052", Method:"Hello"}
  │
  └─ 返回 []DispatchTarget{
         {Addr:"localhost:50052", Request:req, Stream:true}
     }

─────────────────────────────────────────────────────────────
阶段 2：并发派发 dispatchAll
─────────────────────────────────────────────────────────────

BaseHub.dispatchAll(ctx, targets)
  │
  ├─ proto.Clone(target.Request)       ← 深拷贝，并发安全
  ├─ req.HubName = "main_hub"          ← 补全
  ├─ req.TaskId  = "main_hub-17730...-1" ← 全局唯一
  │
  └─ go func → BaseHub.callStream(ctx, "localhost:50052", req)

─────────────────────────────────────────────────────────────
阶段 3：发送请求 callStream
─────────────────────────────────────────────────────────────

BaseHub.callStream
  │
  ├─ pm.get("localhost:50052") → StreamPool
  ├─ StreamPool.Get(ctx)       → pool.Resource[*SingleStream]
  │       Pool 内部：从空闲队列取一条流，无则新建（调 streamResource.Create）
  │
  ├─ SingleStream.Send(req)
  │       ├─ newPendingTask(bufSize=8)
  │       │     └─ pendingTask{
  │       │           resChan: make(chan *ToolResponse, 8),
  │       │           done:    make(chan struct{}),
  │       │        }
  │       ├─ pending.Store("main_hub-...-1", task)  ← 注册到 sync.Map
  │       └─ stream.Send(req)   ← 写入 gRPC 底层发送缓冲
  │                                  （HTTP/2 DATA frame → loopback TCP）
  │
  ├─ StreamPool.Put(res)   ← 立即归还！流还在飞，但归还给池复用
  │
  └─ 进入等待循环：
       for {
         select {
           case resp := <-task.resChan  ← 收到一帧
           case <-task.done             ← 任务结束
           case <-ctx.Done()            ← 超时
         }
       }

─────────────────────────────────────────────────────────────
阶段 4：Tool 侧处理（独立进程）
─────────────────────────────────────────────────────────────

BaseTool.recvLoop（streamSession）
  │  stream.Recv() ← 收到 req
  │
  └─ taskCh <- task{req}   ← 投入任务队列（buffer=128）

BaseTool.executeLoop
  │  <-taskCh
  │
  └─ go executeOne(req)
         │
         ├─ HelloHandler.Execute(req)
         │     └─ 返回 ch chan *ToolResponse（buffer=3）
         │           goroutine 写入：
         │             ch <- PartialResp(loop_idx=1)
         │             ch <- PartialResp(loop_idx=2)
         │             ch <- OKResp(loop_idx=3)
         │             close(ch)
         │
         └─ for resp := range ch {
               respCh <- resp   ← 写入 session 的 respCh（buffer=256）
            }

BaseTool.sendLoop
  │  <-respCh
  │
  └─ stream.Send(resp) × 3   ← 3帧写回 gRPC 流

─────────────────────────────────────────────────────────────
阶段 5：Hub 收帧 recvLoop → dispatch
─────────────────────────────────────────────────────────────

SingleStream.recvLoop（独立 goroutine，建流时启动）
  │
  ├─ stream.Recv() → resp{TaskId="main_hub-...-1", Status="partial", loop_idx=1}
  │     └─ dispatch(resp)
  │           ├─ pending.Load("main_hub-...-1") → task
  │           ├─ isLast = false（partial）
  │           └─ task.resChan <- resp   ← 写入 channel（不阻塞，buf=8）
  │
  ├─ stream.Recv() → resp{Status="partial", loop_idx=2}
  │     └─ dispatch(resp)
  │           └─ task.resChan <- resp
  │
  └─ stream.Recv() → resp{Status="ok", loop_idx=3}
        └─ dispatch(resp)
              ├─ isLast = true（非 partial）
              ├─ pending.Delete("main_hub-...-1")  ← 从 map 摘除
              ├─ task.resChan <- resp              ← 先写数据
              └─ task.finish()                    ← 再关闭 done

─────────────────────────────────────────────────────────────
阶段 6：callStream 收集结果，返回
─────────────────────────────────────────────────────────────

callStream 的等待循环：

  第1次 select：
    case resp := <-task.resChan → partial(1)，append 到 responses

  第2次 select：
    task.done 和 task.resChan 可能同时就绪（Go 随机选）
    case resp := <-task.resChan → partial(2)，append

  第3次 select：
    case resp := <-task.resChan → ok(3)，append
    （done 也就绪了）

  第4次 select：
    case <-task.done → 进入排空循环：
      inner select default → chan 空，return responses

  返回 [partial(1), partial(2), ok(3)]

─────────────────────────────────────────────────────────────
阶段 7：收尾
─────────────────────────────────────────────────────────────

dispatchAll.wg.Done()
  └─ wg.Wait() 解除阻塞

BaseHub.Dispatch
  ├─ handler.OnResults(results)   ← 业务方日志/监控
  └─ return []DispatchResult{
         {Addr:"localhost:50052", Responses:[3帧], Err:nil}
     }
```

---

##### 关键设计决策

**`StreamPool.Put` 在 `Send` 之后立即归还**，而不是等响应回来。这是长连接流的核心优势——流是复用的，发完请求就还给池，下一个请求可以立即复用这条流发另一个任务，两个任务的请求和响应在同一条 HTTP/2 连接上交织传输，互不阻塞。等待响应的工作由 `pendingTask` 的 channel 接管。

**`pending sync.Map` 是多路复用的关键**，它把"哪个响应属于哪个请求"这个问题解耦到了 task_id 层面，使得一条流上可以同时有多个 in-flight 请求，每个请求有自己的 `resChan` 和 `done`，互不干扰。

**`recvLoop` 是单 goroutine 串行处理响应**，不会有并发写同一个 `pendingTask` 的问题，所以 `pendingTask` 内部不需要锁。`pending.Store` 和 `pending.Load` 之间的并发安全由 `sync.Map` 保证。

**`dispatch` 里的顺序**（`pending.Delete` → `resChan <-` → `finish()`）保证了 `callStream` 在 `done` 关闭后排空 `resChan` 时数据已经就位，不会漏帧。

---

## 快速开始

### 目录结构

```
my-project/
├── config/
│   └── registry.yaml
├── cmd/
│   ├── hub/
│   │   ├── main.go
│   │   └── hub_test.go
│   └── tools/
│       ├── hello/main.go
│       └── world/main.go
├── pb_api/
│   └── builder.go          // 框架提供，构造 ToolRequest/ToolResponse
├── proto/gen/proto/
│   ├── service.pb.go
│   └── service_grpc.pb.go
└── root_class/
    ├── hub/baseHub.go
    └── tool/baseTool.go
```

### 1. 配置 registry.yaml

```yaml
services:
  tools:
    - name: "hello"
      addr: "localhost:50052"
      method: "Hello"
      
      # ✅ input_schema: 自定义递归格式
      input_schema: |
        {
          "type": "object",
          "data": {
            "count":  { "type": "integer", "default": 1 },
            "prefix": { "type": "string",  "default": "Hello" }
          },
          "required": []
        }
      
      # ✅ output_schema: 同样格式
      output_schema: |
        {
          "type": "object",
          "data": {
            "message":  { "type": "string" },
            "from":     { "type": "string" },
            "loop_idx": { "type": "integer" },
            "total":    { "type": "integer" }
          }
        }

    - name: "world"
      addr: "localhost:50053"
      method: "World"
      input_schema: |
        {
          "type": "object",
          "data": {
            "name":  { "type": "string", "default": "World" },
            "times": { "type": "integer", "default": 1 }
          }
        }
      output_schema: |
        {
          "type": "object",
          "data": {
            "greeting":  { "type": "string" },
            "iteration": { "type": "integer" }
          }
        }

  hubs:
    - name: "main_hub"
      addr: "localhost:50051"
      registered_at: "2024-01-01T00:00:00Z"

pool:
  grpc_conn:
    min_size: 24
    max_size: 130
    idle_buffer_factor: 0.55
    survive_time_sec: 180
    monitor_interval_sec: 6
    max_retries: 3
    retry_interval_ms: 200
    reconnect_on_get: false
```

### 2. 实现 Tool（以 hello 为例）

```go
package main

import (
    "encoding/json"
    "fmt"
    "log"

    pb     "github.com/sukasukasuka123/microHub/proto/gen/proto"
    pb_api "github.com/sukasukasuka123/microHub/pb_api"
    "github.com/sukasukasuka123/microHub/root_class/tool"
)

type HelloParams struct {
    Count  int    `json:"count"`
    Prefix string `json:"prefix"`
}

type HelloResult struct {
    Message string `json:"message"`
    From    string `json:"from"`
    LoopIdx int    `json:"loop_idx"`
    Total   int    `json:"total"`
}

type HelloHandler struct{}

func (h *HelloHandler) ServiceName() string { return "hello" }

// Execute 同步解析参数，异步生产响应帧。
// 返回值是只读 channel，框架负责消费；关闭 channel 表示任务完成。
func (h *HelloHandler) Execute(req *pb.ToolRequest) (<-chan *pb.ToolResponse, error) {
    var params HelloParams
    if len(req.Params) > 0 {
        if err := json.Unmarshal(req.Params, &params); err != nil {
            return nil, fmt.Errorf("parse params: %w", err)
        }
    }
    if params.Count <= 0 { params.Count = 1 }
    if params.Count > 10 { return nil, fmt.Errorf("count %d exceeds max 10", params.Count) }
    if params.Prefix == "" { params.Prefix = "Hello" }

    ch := make(chan *pb.ToolResponse, params.Count)
    go func() {
        defer close(ch)
        for i := 1; i <= params.Count; i++ {
            isLast := i == params.Count
            var resp *pb.ToolResponse
            var err error
            if isLast {
                resp, err = pb_api.OKResp("hello", req.TaskId, HelloResult{
                    Message: params.Prefix, From: "hello", LoopIdx: i, Total: params.Count,
                })
            } else {
                resp, err = pb_api.PartialResp("hello", req.TaskId, HelloResult{
                    Message: params.Prefix, From: "hello", LoopIdx: i, Total: params.Count,
                })
            }
            if err != nil {
                ch <- pb_api.ErrorResp("hello", req.TaskId, "BUILD_RESP", err.Error(), "")
                return
            }
            ch <- resp
        }
    }()
    return ch, nil
}

func main() {
    t := tool.New(&HelloHandler{})
    log.Println("[hello] 启动 :50052")
    if err := t.Serve(":50052"); err != nil {
        log.Fatalf("[hello] %v", err)
    }
}
```

**关键点**：
- 单帧响应（count=1）直接发 `ok`；多帧响应中间帧发 `partial`，最后帧发 `ok`
- 参数解析在 goroutine 启动前同步完成，解析失败直接 `return nil, err`
- `defer close(ch)` 是框架感知任务结束的唯一信号，不能省略

### 3. 实现 Hub

```go
package main

import (
    "context"
    "log"
    "time"

    pb     "github.com/sukasukasuka123/microHub/proto/gen/proto"
    pb_api "github.com/sukasukasuka123/microHub/pb_api"
    hub    "github.com/sukasukasuka123/microHub/root_class/hub"
    reg    "github.com/sukasukasuka123/microHub/service_registry"
)

type MainHubHandler struct{}

func (h *MainHubHandler) ServiceName() string { return "main_hub" }

// Execute 路由策略：req==nil 表示定时触发（广播），否则按 method 路由。
func (h *MainHubHandler) Execute(req *pb.ToolRequest) ([]hub.DispatchTarget, error) {
    if req == nil {
        return h.broadcast()
    }
    t, ok := reg.SelectToolByMethod(req.Method)
    if !ok {
        return nil, nil
    }
    return []hub.DispatchTarget{
        {Addr: t.Addr, Request: req, Stream: true},
    }, nil
}

func (h *MainHubHandler) broadcast() ([]hub.DispatchTarget, error) {
    tools := reg.GetAllTools()
    targets := make([]hub.DispatchTarget, 0, len(tools))
    for _, t := range tools {
        // 每个 target 必须有独立的 req，不能共享指针（并发写 TaskId 会竞态）
        req, err := pb_api.Request().Method(t.Method).Params(map[string]int{"count": 1}).Build()
        if err != nil {
            return nil, err
        }
        targets = append(targets, hub.DispatchTarget{Addr: t.Addr, Request: req, Stream: true})
    }
    return targets, nil
}

func (h *MainHubHandler) OnResults(results []hub.DispatchResult) {
    for _, r := range results {
        if r.Err != nil {
            log.Printf("[Hub] ✗ addr=%s err=%v", r.Target.Addr, r.Err)
            continue
        }
        for _, resp := range r.Responses {
            switch resp.Status {
            case "ok":
                log.Printf("[Hub] ✓ tool=%s task=%s result=%s", resp.ToolName, resp.TaskId, resp.Result)
            case "partial":
                log.Printf("[Hub] ↻ tool=%s task=%s partial=%s", resp.ToolName, resp.TaskId, resp.Result)
            case "error":
                for _, e := range resp.Errors {
                    log.Printf("[Hub] ✗ tool=%s [%s] %s", resp.ToolName, e.Code, e.Message)
                }
            }
        }
    }
}

func (h *MainHubHandler) Addrs() []string {
    tools := reg.GetAllTools()
    addrs := make([]string, 0, len(tools))
    for _, t := range tools {
        addrs = append(addrs, t.Addr)
    }
    return addrs
}

func main() {
    if err := reg.Init("config/registry.yaml"); err != nil {
        log.Fatalf("registry init: %v", err)
    }

    h := hub.New(&MainHubHandler{})

    // ServeAsync 第二个参数为定时广播间隔，0 = 不启动定时广播
    go func() {
        if err := h.ServeAsync(":50051", 0); err != nil {
            log.Fatalf("ServeAsync: %v", err)
        }
    }()
    time.Sleep(100 * time.Millisecond)

    // 主动派发示例
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    req, _ := pb_api.Request().Method("Hello").Params(map[string]interface{}{
        "count": 3, "prefix": "Hi",
    }).Build()
    results := h.Dispatch(ctx, req)
    for _, r := range results {
        log.Printf("addr=%s ok=%v frames=%d", r.Target.Addr, r.AllOK(), len(r.Responses))
    }

    select {}
}
```

### 4. 启动

```bash
go run ./cmd/tools/hello   # terminal 1
go run ./cmd/tools/world   # terminal 2
go run ./cmd/hub           # terminal 3
```

---

## API 参考

### ToolHandler 接口

```go
type ToolHandler interface {
    ServiceName() string
    Execute(req *pb.ToolRequest) (<-chan *pb.ToolResponse, error)
}
```

可选接口，实现后框架在每条流建立时注入 `ActiveSender`，允许 Tool 主动向 Hub 推送：

```go
type ActiveHandler interface {
    OnRegister(sender ActiveSender)
}

type ActiveSender interface {
    Send(resp *pb.ToolResponse) error
}
```

**注意**：`ToolHandler` 是单例，`OnRegister` 每条流调用一次。不要把 `sender` 存到 handler 字段，应绑定到每条流独立的 goroutine：

```go
type MyHandler struct{} // 无 sender 字段

func (h *MyHandler) OnRegister(s tool.ActiveSender) {
    go func() {
        // 每条流独立的心跳 goroutine，sender 关闭时自动退出
        for range time.Tick(30 * time.Second) {
            if err := s.Send(heartbeatResp()); err != nil {
                return
            }
        }
    }()
}
```

### HubHandler 接口

```go
type HubHandler interface {
    ServiceName() string
    Execute(req *pb.ToolRequest) ([]DispatchTarget, error)  // req==nil 表示定时触发
    OnResults(results []DispatchResult)
    Addrs() []string
}
```

### pb_api 构造函数

```go
// 构造请求
pb_api.NewRequest("Hello", params)                    // → (*ToolRequest, error)
pb_api.MustRequest("Hello", params)                   // → *ToolRequest，失败 panic
pb_api.Request().Method("Hello").Params(p).Build()    // 链式

// 构造响应（toolName、taskID 必填）
pb_api.OKResp("hello", req.TaskId, data)              // status="ok"
pb_api.PartialResp("hello", req.TaskId, data)         // status="partial"
pb_api.ErrorResp("hello", req.TaskId, code, msg, field) // status="error"

// 链式
pb_api.Resp().ToolName("hello").TaskID(id).StatusOK().Result(data).Build()
```

### BaseHub 方法

```go
// 创建 Hub，同时预热流池
hub := hub.New(handler)

// 启动 gRPC 监听（阻塞），interval=0 不启动定时广播
hub.ServeAsync(addr string, interval time.Duration) error

// 主动派发（核心），阻塞等待所有 Tool 响应
hub.Dispatch(ctx, req) []DispatchResult

// 短连接派发（兼容，低频场景）
hub.DispatchSimpleCall(ctx, req) (*ToolResponse, error)
```

### DispatchResult

```go
type DispatchResult struct {
    Target    DispatchTarget
    Responses []*pb.ToolResponse  // 按帧顺序，含 partial 和 ok
    Err       error
}

func (r DispatchResult) AllOK() bool  // 无错误且所有帧 status="ok"
```

---

## 响应状态约定

| status | 含义 | 框架行为 |
|--------|------|---------|
| `"partial"` | 中间帧，任务未结束 | 写入 resChan，继续等待 |
| `"ok"` | 最终帧，任务成功结束 | 写入 resChan，触发 finish() |
| `"error"` | 最终帧，任务失败结束 | 写入 resChan，触发 finish() |

**多帧响应必须**：中间帧发 `partial`，最后帧发 `ok` 或 `error`。单帧响应直接发 `ok`。

违反此约定的后果：第一个非 `partial` 帧到达时框架即认为任务结束，后续帧被丢弃并打印 `未知 task_id，忽略`。

---

## 测试

### 单元测试

```bash
# 单测（框架自动启动 tool 子进程，pool min=2 max=8）
go test ./cmd/hub -run ^Test -v -timeout 30s

# 竞争检测
go test ./cmd/hub -race -run ^Test -count=5
```

### 压测

```bash
# 先手动启动 tool（不依赖测试框架自动拉起，避免端口冲突）
go run ./cmd/tools/hello &
go run ./cmd/tools/world &

# 用生产规格的 pool 压测
GRPC_POOL_MIN=24 GRPC_POOL_MAX=130 \
  go test ./cmd/hub -bench=BenchmarkDispatch_Concurrency -benchtime=5s -v
```

### 参考基准（本机 loopback，pool min=24）

| benchmark | ns/op | 说明 |
|-----------|-------|------|
| `Hello` 串行 | ~1.1ms | 单 goroutine，受 RTT 限制 |
| `Parallel` 并发 | ~350μs | GOMAXPROCS goroutine 并发 |
| `MultiTool` 广播 | ~1.2ms | hello+world 并发派发，接近单次延迟 |
| `Concurrency/500` | ~120μs | 500 goroutine 并发，流池充分利用 |

串行延迟高（1.1ms）是单条 goroutine 等待 loopback RTT 的正常表现，不代表吞吐低。`Concurrency/500` 的 120μs 才是流池并发能力的真实体现。

---

## 常见问题

**Q: 为什么只收到第一帧响应？**

Execute 里多帧中间帧必须用 `pb_api.PartialResp`，只有最后一帧才用 `OKResp`。如果全部帧都用 `OKResp`，第一帧到达时框架就认为任务结束，后续帧被丢弃。

**Q: 并发压测时出现 `未知 task_id` 日志？**

所有并发 goroutine 共享同一个 `*pb.ToolRequest` 指针，框架内部 `dispatchAll` 会并发写 `req.TaskId`，导致 task_id 互相覆写。每个 goroutine 应该用独立的 req 对象，或者让框架处理（`Dispatch` 传入的 req 框架会自动 `proto.Clone`）。

**Q: `activeSender.Send` panic: send on closed channel？**

`ToolHandler` 是单例，不要把 `sender` 存到 handler 字段，因为 `OnRegister` 每条流都会调用一次覆盖旧值。正确做法是把 sender 只传给绑定到该流的独立 goroutine，sender 返回 error 时 goroutine 退出。

**Q: 定时广播怎么用？**

`hub.ServeAsync(addr, 5*time.Second)` 第二个参数传非零值，框架每隔该时间调一次 `handler.Execute(nil)`，`nil` 表示定时触发而非外部请求，业务方在 Execute 里通过 `req == nil` 判断并返回广播目标。生产环境如果不需要定时功能传 `0`。

**Q: 热更新如何触发？**

修改 `registry.yaml` 文件后，`service_registry.Watch()` 自动检测文件变化，重建连接池并通过 `changeCh` 通知 Hub 更新路由表，无需重启进程。

---

## Proto 定义

```protobuf
message ToolRequest {
    string hub_name = 1;   // 框架自动填充
    string task_id  = 2;   // 框架自动填充，全局唯一
    string method   = 3;   // 路由标识，对应 registry.yaml 中的 method 字段
    bytes  params   = 4;   // JSON 序列化的业务参数
}

message ToolResponse {
    string tool_name = 1;
    string task_id   = 2;
    string status    = 3;  // "ok" | "partial" | "error"
    bytes  result    = 4;  // JSON 序列化的业务响应
    repeated ErrorDetail errors = 5;
}

message ErrorDetail {
    string code    = 1;
    string message = 2;
    string field   = 3;
    string help    = 4;
}

service HubService {
    rpc DispatchStream (stream ToolRequest) returns (stream ToolResponse);
    rpc DispatchSimple (ToolRequest)        returns (ToolResponse);
}
```
