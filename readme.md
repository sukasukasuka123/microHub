# 🚀 microHub 使用指南

一个基于 gRPC 的轻量级微服务调度框架，支持**服务注册发现**、**连接池热更新**、**结构化错误传递**和**契约优先的接口定义**。

---

## 📐 架构概览

```
┌─────────────────┐
│   Client / Hub  │
└────────┬────────┘
         │ gRPC (DispatchSimple/Stream)
         ▼
┌─────────────────┐
│   BaseHub       │  ← 路由转发 + 连接池 + 结果聚合
│   (框架层)       │
└────────┬────────┘
         │ 按 service_name / method 路由
         ▼
┌─────────────────┐     ┌─────────────────┐
│   BaseTool      │     │   BaseTool      │
│   hello:50052   │     │   world:50053   │
│   (业务方实现)   │     │   (业务方实现)   │
└─────────────────┘     └─────────────────┘
```

**核心组件**：
| 组件 | 职责 | 是否需业务方实现 |
|------|------|-----------------|
| `BaseHub` | 路由转发、连接池管理、结果聚合、结构化错误包装 | ❌ 框架提供 |
| `BaseTool` | gRPC 服务端封装、参数透传、错误包装 | ❌ 框架提供 |
| `ToolHandler` | 业务逻辑：解析参数、执行业务、构造响应 | ✅ 业务方实现 |
| `HubHandler` | 路由策略：按 name/method 路由、结果回调 | ✅ 业务方实现 |
| `registry.yaml` | 服务注册表 + 连接池配置 + 接口契约文档 | ✅ 配置文件 |

---

## 🎯 快速开始

### 1️⃣ 准备项目结构

```
my-microhub/
├── config/
│   └── registry.yaml          # 服务注册表 + 配置
├── cmd/
│   ├── hub/                   # Hub 服务入口
│   │   └── main.go
│   ├── hello/                 # hello 工具服务
│   │   └── main.go
│   └── world/                 # world 工具服务
│       └── main.go
├── proto/
│   └── hub.proto              # gRPC 协议定义
└── go.mod
```

### 2️⃣ 编写 `config/registry.yaml`

```yaml
services:
  tools:
    - name: "hello"
      addr: "localhost:50052"
      method: "Hello"
      # 接口契约：输入参数格式（供文档/校验使用）
      input_schema: |
        {
          "type": "object",
          "data": {
            "count":  { "type": "integer", "default": 1, "min": 1, "max": 10 },
            "prefix": { "type": "string",  "default": "Hello" }
          }
        }
      # 接口契约：输出响应格式
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
            "count": { "type": "integer", "default": 1, "min": 1, "max": 5 },
            "name":  { "type": "string",  "default": "World" }
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

# gRPC 连接池配置
pool:
  grpc_conn:
    min_size: 24              # 最小连接数
    max_size: 130             # 最大连接数
    idle_buffer_factor: 0.55  # 空闲缓冲比例
    survive_time_sec: 180     # 空闲连接存活时间
    monitor_interval_sec: 6   # 健康检查间隔
    max_retries: 3            # 连接失败重试次数
    retry_interval_ms: 200    # 重试间隔
    reconnect_on_get: false   # 获取连接时是否重连（gRPC 自带重连，通常设为 false）
```

### 3️⃣ 实现 Tool 服务（以 `hello` 为例）

```go
// cmd/hello/main.go
package main

import (
	"encoding/json"
	"fmt"
	"log"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	tool "github.com/sukasukasuka123/microHub/root_class/tool"
)

// 请求参数结构（与 input_schema 对应）
type HelloRequest struct {
	Count  int    `json:"count"`
	Prefix string `json:"prefix"`
}

// 响应数据结构（与 output_schema 对应）
type HelloResponse struct {
	Message string `json:"message"`
	From    string `json:"from"`
	LoopIdx int    `json:"loop_idx"`
	Total   int    `json:"total"`
}

type HelloHandler struct{}

func (h *HelloHandler) ServiceName() string { return "hello" }

func (h *HelloHandler) Execute(req *pb.ToolRequest) ([]*pb.ToolResponse, error) {
	// 1. 解析参数（[]byte → struct）
	var params HelloRequest
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, fmt.Errorf("parse params: %w", err)
		}
	}
	// 2. 默认值 + 校验
	if params.Count <= 0 { params.Count = 1 }
	if params.Count > 10 { params.Count = 10 }
	if params.Prefix == "" { params.Prefix = "Hello" }

	// 3. 执行业务逻辑
	var resps []*pb.ToolResponse
	for i := 1; i <= params.Count; i++ {
		respData := HelloResponse{
			Message: params.Prefix, From: "hello", LoopIdx: i, Total: params.Count,
		}
		// ✅ 使用工具函数自动序列化 Result 为 []byte
		resp, _ := tool.NewOKResp("hello", respData)
		resps = append(resps, resp)
	}
	return resps, nil
}

func main() {
	t := tool.New(&HelloHandler{})
	log.Println("[hello] 启动服务 :50052")
	if err := t.Serve(":50052"); err != nil {
		log.Fatalf("[hello] Serve failed: %v", err)
	}
}
```

### 4️⃣ 实现 Hub 服务

```go
// cmd/hub/main.go（简化版）
package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	hubbase "github.com/sukasukasuka123/microHub/root_class/hub"
	registry "github.com/sukasukasuka123/microHub/service_registry"
)

type MainHubHandler struct{}

func (h *MainHubHandler) ServiceName() string { return "hub" }

// 路由策略：按 service_name 或 method 路由
func (h *MainHubHandler) Execute(req *pb.ToolRequest) ([]hubbase.DispatchTarget, error) {
	if req.ServiceName != "" {
		t, ok := registry.SelectToolByName(req.ServiceName)
		if !ok { return nil, nil }
		return []hubbase.DispatchTarget{{Addr: t.Addr, Request: req, Stream: true}}, nil
	}
	// 可按 method 路由或广播...
	return nil, nil
}

// 结果回调：日志/监控/告警
func (h *MainHubHandler) OnResults(results []hubbase.DispatchResult) {
	for _, r := range results {
		if r.Err != nil {
			log.Printf("[Hub] ✗ addr=%s err=%v", r.Target.Addr, r.Err)
			continue
		}
		for _, resp := range r.Responses {
			if resp.Status == "ok" {
				log.Printf("[Hub] ✓ [%s] %s", resp.ServiceName, string(resp.Result))
			}
		}
	}
}

// 返回所有工具地址，供连接池热更新
func (h *MainHubHandler) Addrs() []string {
	tools := registry.GetAllTools()
	addrs := make([]string, 0, len(tools))
	for _, t := range tools { addrs = append(addrs, t.Addr) }
	return addrs
}

func main() {
	// 1. 初始化注册表（监听热更新）
	registry.Init("config/registry.yaml")
	
	// 2. 创建 Hub 实例
	hub := hubbase.New(&MainHubHandler{})
	
	// 3. 启动 gRPC 监听
	go hub.ServeAsync(":50051", 0) // 0 = 不启动定时广播
	time.Sleep(100 * time.Millisecond)
	
	// 4. 发送测试请求
	params, _ := json.Marshal(map[string]int{"count": 3})
	ctx := context.Background()
	hub.Dispatch(ctx, &pb.ToolRequest{
		ServiceName: "hello", Params: params,
	})
	
	// 5. 阻塞主进程
	select {}
}
```

### 5️⃣ 启动服务

```bash
# terminal 1: hello 服务
go run ./cmd/hello

# terminal 2: world 服务
go run ./cmd/world

# terminal 3: hub 服务（含测试请求）
go run ./cmd/hub
```

**预期输出**：
```
[Hub] ✓ [hello] result={"message":"Hello","from":"hello","loop_idx":1,"total":3}
[Hub] ✓ [hello] result={"message":"Hello","from":"hello","loop_idx":2,"total":3}
[Hub] ✓ [hello] result={"message":"Hello","from":"hello","loop_idx":3,"total":3}
```

---

## 🔧 配置详解：`registry.yaml`

### 服务注册格式

```yaml
services:
  tools:
    - name: "your_service"      # ✅ 必填：服务唯一标识，与 ToolHandler.ServiceName() 一致
      addr: "host:port"         # ✅ 必填：gRPC 服务地址
      method: "MethodName"      # ✅ 必填：日志/路由标识
      input_schema: |           # 📝 可选：输入参数契约（JSON Schema 格式）
        {
          "type": "object",
          "data": {
            "field1": { "type": "string", "default": "value" },
            "field2": { "type": "integer", "min": 0, "max": 100 }
          },
          "required": ["field1"]
        }
      output_schema: |          # 📝 可选：输出响应契约
        {
          "type": "object",
          "data": {
            "result": { "type": "string" }
          }
        }
```

### Schema 规范（无歧义递归格式）

```json
{
  "type": "object",           // ✅ 每个节点必须有 type
  "data": {                   // ✅ object 类型的子字段放在 data 中
    "username": { "type": "string" },
    "age": { "type": "integer", "min": 0, "max": 120 },
    "tags": {                 // ✅ array 类型用 items 定义元素
      "type": "array",
      "items": { "type": "string" }
    },
    "profile": {              // ✅ 嵌套 object
      "type": "object",
      "data": {
        "email": { "type": "string" }
      }
    }
  },
  "required": ["username"]    // ✅ 必填字段列表
}
```

**支持的类型**：`string` | `integer` | `number` | `boolean` | `object` | `array`

**支持的校验规则**：
| 规则 | 适用类型 | 说明 |
|------|----------|------|
| `default` | 所有 | 默认值 |
| `min` / `max` | integer/number | 数值范围 |
| `enum` | 所有 | 枚举值列表 `["a", "b", "c"]` |
| `required` | object | 必填字段列表 |

---

## 🛠️ 开发指南

### 创建新 Tool 服务

1. **定义参数/响应结构**（与 `input_schema`/`output_schema` 保持一致）
2. **实现 `ToolHandler` 接口**：
   ```go
   type MyHandler struct{}
   func (h *MyHandler) ServiceName() string { return "my_service" }
   func (h *MyHandler) Execute(req *pb.ToolRequest) ([]*pb.ToolResponse, error) {
       // 1. 解析参数
       var params MyRequest
       json.Unmarshal(req.Params, &params)
       
       // 2. 业务逻辑
       result := doSomething(params)
       
       // 3. 构造响应（自动序列化）
       return tool.NewOKResp("my_service", result)
   }
   ```
3. **启动服务**：`tool.New(&MyHandler{}).Serve(":500xx")`
4. **注册到 `registry.yaml`**

### 创建新 Hub 服务

1. **实现 `HubHandler` 接口**：
   ```go
   type MyHubHandler struct{}
   func (h *MyHubHandler) ServiceName() string { return "my_hub" }
   
   // 路由策略
   func (h *MyHubHandler) Execute(req *pb.ToolRequest) ([]hubbase.DispatchTarget, error) {
       // 按 service_name 路由
       t, ok := registry.SelectToolByName(req.ServiceName)
       if !ok { return nil, nil }
       return []hubbase.DispatchTarget{{Addr: t.Addr, Request: req}}, nil
   }
   
   // 结果处理
   func (h *MyHubHandler) OnResults(results []hubbase.DispatchResult) {
       // 日志/监控/告警
   }
   
   // 返回工具地址列表
   func (h *MyHubHandler) Addrs() []string {
       // 从 registry 或配置读取
   }
   ```
2. **启动 Hub**：
   ```go
   registry.Init("config/registry.yaml")
   hub := hubbase.New(&MyHubHandler{})
   hub.ServeAsync(":50051", 0)  // 0 = 不启动定时广播
   ```

---

## 📡 参数传递规范

### ✅ 统一参数名约定

| 参数名 | 类型 | 含义 | 适用场景 |
|--------|------|------|----------|
| `count` | integer | 循环/重复次数 | hello, world, 列表类服务 |
| `limit` | integer | 分页每页数量 | 查询类服务 |
| `offset` | integer | 分页偏移量 | 查询类服务 |
| `name` / `id` | string | 资源标识 | 所有服务 |
| `prefix` / `suffix` | string | 字符串修饰 | 文本处理服务 |

> 🎯 **原则**：相同语义的参数，在所有服务中使用**相同的字段名**，降低集成成本。

### ✅ 参数序列化

```go
// 发送方：序列化为 []byte
params, _ := json.Marshal(map[string]interface{}{
    "count": 3,
    "prefix": "Hi",
})
req := &pb.ToolRequest{
    ServiceName: "hello",
    Params:      params,  // ✅ []byte 类型
}

// 接收方：解析为 struct
var p HelloRequest
json.Unmarshal(req.Params, &p)  // ✅ 自动映射
```

---

## 🧪 测试指南

### 单元测试示例

```go
func TestDispatch_Hello(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    
    results := testHub.Dispatch(ctx, &pb.ToolRequest{
        ServiceName: "hello",
        Params:      mustMarshal(map[string]int{"count": 3}), // ✅ []byte
    })
    
    if len(results) == 0 { t.Fatal("no results") }
    for _, r := range results {
        if r.Err != nil { t.Errorf("dispatch failed: %v", r.Err) }
        if len(r.Responses) != 3 { t.Errorf("expected 3 responses") }
    }
}

// 辅助函数：简化参数序列化
func mustMarshal(v interface{}) []byte {
    b, err := json.Marshal(v)
    if err != nil { panic(err) }
    return b
}
```

### 运行测试

```bash
# 启动依赖服务后运行
go test ./cmd/hub -v -run ^TestDispatch_Hello$

# 压测
go test ./cmd/hub -bench=. -benchtime=5s

# 竞争检测
go test ./cmd/hub -race -count=10
```

---

## ❓ 常见问题

### Q: `params` 为什么是 `[]byte` 而不是 `map`？
**A**：`[]byte` 是透明传输的 JSON 序列化结果，框架层不解析业务参数，保证：
- ✅ 业务方完全控制解析策略（严格/宽松/默认值）
- ✅ 避免框架与业务方的参数校验逻辑冲突
- ✅ 支持任意复杂嵌套结构

### Q: 如何调试参数解析失败？
**A**：业务方 `Execute` 中解析失败时，直接 `return nil, err`，BaseTool 会自动包装为结构化错误：
```json
{
  "status": "error",
  "result": "{}",
  "errors": [{
    "code": "EXECUTE_FAILED",
    "message": "parse params: invalid character 'x'...",
    "field": ""
  }]
}
```

### Q: `registry.yaml` 热更新生效吗？
**A**：✅ 是的。`service_registry.Watch()` 监听文件变化，自动：
1. 重新解析配置
2. 重建连接池（关闭旧连接，新建连接）
3. 通过 `changeCh` 通知 Hub 更新路由

### Q: 如何添加新字段到 `ToolRequest`？
**A**：
1. 修改 `proto/hub.proto`，添加字段（注意字段号递增）
2. `protoc` 重新生成 Go 代码
3. 业务方按需使用新字段（旧代码兼容，新字段为零值）

---

## 📦 项目依赖

```go
// go.mod 示例
require (
    github.com/fsnotify/fsnotify v1.7.0      // 配置文件热更新
    github.com/spf13/viper v1.18.0           // YAML 配置解析
    google.golang.org/grpc v1.60.0           // gRPC 框架
    github.com/sukasukasuka123/TemplatePoolByGO v0.1.0  // 连接池
)
```

---

## 🗂️ 目录结构参考

```
microHub/
|   go.mod
|   go.sum
|   README.md
|   
+---cmd
|   +---hub
|   |       hub_test.go
|   |       main.go
|   |
|   \---tools
|       +---hello
|       |       main.go
|       |
|       \---world
|               main.go
|
+---config
|       registry.yaml
|
+---jsonSchema
|       schema.go
|
+---proto
|   |   service.proto
|   |
|   \---gen
|       \---proto
|               service.pb.go
|               service_grpc.pb.go
|
+---root_class
|   +---hub
|   |       baseHub.go
|   |
|   \---tool
|           baseTool.go
|
+---service_registry
|       s_ registry.go
|
\---test_log
        Benchmark.log
        unit_test.log
```

---

> 💡 **核心设计原则**：  
> 1. **框架轻量**：`BaseHub`/`BaseTool` 只负责转发和错误包装，不干涉业务逻辑  
> 2. **契约明确**：`registry.yaml` 的 `input_schema`/`output_schema` 是人和工具的契约文档  
> 3. **业务自主**：参数解析、校验、默认值完全由业务方在 `Execute` 中控制  
> 4. **错误结构化**：`ErrorDetail` 贯穿全链路，便于定位和监控  

🚀 现在你可以基于此框架快速构建自己的微服务集群了！如有问题，欢迎查阅代码注释或提交 Issue。