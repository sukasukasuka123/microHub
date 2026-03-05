# microHub

轻量级 gRPC 微服务编排框架。一个 Hub 作为调度中心，多个 Tool 作为执行节点，Hub 负责路由和结果聚合，Tool 只管执行业务逻辑。

---

## 目录结构

```
microHub/
├── config/
│   └── registry.yaml        # 服务注册表 + 连接池配置（唯一数据源）
├── hub/
│   ├── main.go              # Hub 实例：路由策略 + 启动入口
│   └── hub_test.go              # 框架测试代码
├── tools/
│   ├── hello/main.go        # Tool 示例
│   └── world/main.go        # Tool 示例
├── root_class/
│   ├── hub/baseHub.go       # Hub 基类：连接池、并发派发、热更新
│   └── tool/baseTool.go     # Tool 基类：gRPC 服务端、请求校验
├── service_registry/
│   └── s_registry.go        # 注册表：yaml 读写、热更新通知、tool 状态管理
└── proto/
    └── service.proto
```

---

## 核心概念

### Hub
调度中心。实现 `HubHandler` 接口，决定每次派发的目标列表和结果处理逻辑。支持：

- **定时广播**：`Serve` 传入非零 interval，定时调用 `Execute(nil)` 广播所有 tool
- **按需路由**：上游调用时按 `service_name` 或 `method` 精确路由
- **连接池热更新**：监听 `registry.ChangeCh()`，yaml 变更时自动重建连接池，无需重启

### Tool
执行节点。实现 `ToolHandler` 接口，只需关心 `ServiceName()` 和 `Execute()` 两个方法。框架负责 gRPC 服务注册、请求校验、流式/简单两种调用模式的适配。

### Registry
`registry.yaml` 是唯一数据源，同时承担：

- 服务发现（tool/hub 的地址和方法名）
- 连接池参数配置
- 运行时热更新（fsnotify 监听文件变化，变更自动生效）

---

## 快速开始

**1. 启动 Tool**

```bash
go run tools/hello/main.go
go run tools/world/main.go
```

**2. 启动 Hub**

```bash
go run hub/main.go
```

Hub 启动后每 5 秒自动广播一次，也可通过 gRPC 客户端主动调用。

**3. 动态注册新 Tool**

直接修改 `config/registry.yaml`，添加新 tool 条目，Hub 自动感知无需重启：
```yaml
services:
  tools:
    - name: "new_tool"
      addr: "localhost:50054"
      method: "NewMethod"
```

---

## 实现一个 Tool

```go
type MyHandler struct{}

func (h *MyHandler) ServiceName() string { return "my_tool" }

func (h *MyHandler) Execute(req *pb.ToolRequest) ([]*pb.ToolResponse, error) {
    // 业务逻辑
    return []*pb.ToolResponse{{
        ServiceName: h.ServiceName(),
        Status:      "ok",
        Result:      "done",
    }}, nil
}

func main() {
    t := tool.New(&MyHandler{})
    if err := t.Serve(":50055"); err != nil {
        log.Fatalf("%v", err)
    }
}
```

## 实现一个 Hub

```go
type MyHubHandler struct{}

func (h *MyHubHandler) ServiceName() string { return "my_hub" }

func (h *MyHubHandler) Execute(req *pb.ToolRequest) ([]hubbase.DispatchTarget, error) {
    // req == nil 为定时触发，自行决定派发策略
    if req == nil {
        return h.broadcast(), nil
    }
    return h.routeByName(req)
}

func (h *MyHubHandler) OnResults(results []hubbase.DispatchResult) {
    for _, r := range results {
        if r.AllOK() {
            log.Printf("✓ %v", r.Responses)
        }
    }
}

func (h *MyHubHandler) Addrs() []string {
    // 返回本 Hub 关心的所有下游地址，用于连接池管理
    tools := registry.GetAllTools()
    addrs := make([]string, len(tools))
    for i, t := range tools {
        addrs[i] = t.Addr
    }
    return addrs
}
```

---

## Hub 命名约定：test / prod 分离

框架不区分环境，通过 `name` 字段命名约定实现 test/prod 分离：

```yaml
services:
  hubs:
    - name: "test_hub"
      addr: "localhost:50051"
    - name: "main_hub"
      addr: "localhost:50061"
```
**需要改动关于hub的描述**
`test_hub` 的 `Execute` 只路由到待验证的 tool，通过后调用 `registry.MarkPassed()`，`main_hub` 的 `Execute` 只派发 `registry.GetPassedTools()` 中的 tool。两个 Hub 平行运行，互不干扰。

---

## Tool 生命周期管理（AI Agent 场景）

框架内置了 tool 测试状态的管理 API，适合 AI 自动生成、测试、上线 tool 的闭环：

```go
registry.MarkPassed("my_tool")    // 标记通过测试，进入 main_hub 派发范围
registry.MarkFailed("my_tool")    // 标记失败，从派发范围移除
registry.IsPassed("my_tool")      // 查询状态
registry.GetPassedTools()         // 获取所有通过测试的 tool 列表
```

典型的 AI Agent 自建 tool 流程：
```
AI 生成 tools/xxx/main.go
    → go run tools/xxx/main.go（启动进程）
    → 修改 registry.yaml 注册到 test_hub 地址范围
    → test_hub 自动发现并发送验证请求
    → 验证通过 → MarkPassed() → 修改 yaml 注册到 main_hub 地址范围
    → main_hub 热更新，新 tool 上线
```

## 相关单元测试和基准测试的结果：

![unit_test.log](test_log/unit_test.log)

![Benchmark.log](test_log/Benchmark.log)


---

## 连接池参数说明

```yaml
pool:
  grpc_conn:
    min_size: 24              # 谷值连接数 = 低峰QPS × 平均持有时间(s) × 预热系数
    max_size: 130             # 峰值连接数 = 峰值QPS × 平均持有时间(s) × 突发系数
    idle_buffer_factor: 0.55  # 空闲缓冲比 = 1 - 平均活跃数/max_size，建议 0.4~0.7
    survive_time_sec: 180     # 空闲连接存活时间
    monitor_interval_sec: 6   # 连接池巡检间隔，建议 survive_time / 30
    max_retries: 3            # 获取连接失败最大重试次数
    retry_interval_ms: 200    # 重试间隔，建议 >= 连接建立耗时 × 2
    reconnect_on_get: false   # gRPC 自带重连机制，池层无需重复处理
```

建模公式：
```
ActiveConns  = QPS × hold_ms / 1000
max_size     = ceil(ActiveConns × burst_factor)
min_size     = max(ceil(valley_ActiveConns × warmup_factor), max_size × 0.05)
idle_buffer  = 1 - avg_active / max_size
```

---

## 适用场景

- AI Agent 工具编排（自动生成、测试、上线 tool）
- 内部自动化任务调度
- 定时广播型任务（数据采集、健康检查、批处理）
- 需要动态增减服务节点的轻量编排场景

## 不适用场景

- 需要服务发现（Consul/etcd）的大规模微服务
- 需要熔断、限流、链路追踪的生产级网关
- 跨团队服务治理
