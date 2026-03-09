package main

import (
	"context"
	"log"
	"time"

	"github.com/sukasukasuka123/microHub/pb_api"
	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	hubbase "github.com/sukasukasuka123/microHub/root_class/hub"
	registry "github.com/sukasukasuka123/microHub/service_registry"
)

// ════════════════════════════════════════════════════════════
//  MainHubHandler
// ════════════════════════════════════════════════════════════

type MainHubHandler struct{}

func (h *MainHubHandler) ServiceName() string { return "main_hub" }

// Execute 路由策略：
//   - req == nil  → 定时触发，广播给所有已注册的 tool
//   - req != nil  → 按 method 精确路由到单个 tool
func (h *MainHubHandler) Execute(req *pb.ToolRequest) ([]hubbase.DispatchTarget, error) {
	if req == nil {
		return h.broadcast()
	}
	return h.routeByMethod(req)
}

// OnResults 处理聚合结果：区分成功/失败分别记录。
// 生产环境可在此接入 metrics / 告警系统。
func (h *MainHubHandler) OnResults(results []hubbase.DispatchResult) {
	for _, r := range results {
		if r.Err != nil {
			log.Printf("[Hub] ✗ addr=%s dispatch err: %v", r.Target.Addr, r.Err)
			continue
		}
		for _, resp := range r.Responses {
			switch resp.Status {
			case "ok":
				log.Printf("[Hub] ✓ tool=%s task=%s result=%s",
					resp.ToolName, resp.TaskId, string(resp.Result))
			case "partial":
				log.Printf("[Hub] ↻ tool=%s task=%s partial=%s",
					resp.ToolName, resp.TaskId, string(resp.Result))
			default:
				for _, e := range resp.Errors {
					log.Printf("[Hub] ✗ tool=%s task=%s [%s] %s (field=%s)",
						resp.ToolName, resp.TaskId, e.Code, e.Message, e.Field)
				}
			}
		}
	}
}

// Addrs 返回所有已注册 tool 的地址，供流池热更新使用。
func (h *MainHubHandler) Addrs() []string {
	tools := registry.GetAllTools()
	addrs := make([]string, 0, len(tools))
	for _, t := range tools {
		addrs = append(addrs, t.Addr)
	}
	return addrs
}

// ── 路由辅助 ─────────────────────────────────────────────

// routeByMethod 按 method 字段精确路由到单个 tool（stream 模式）。
func (h *MainHubHandler) routeByMethod(req *pb.ToolRequest) ([]hubbase.DispatchTarget, error) {
	t, ok := registry.SelectToolByMethod(req.Method)
	if !ok {
		log.Printf("[Hub] method=%q 未注册", req.Method)
		return nil, nil
	}
	// hub_name / task_id 由框架层（dispatchAll）自动填充，无需手动设置
	return []hubbase.DispatchTarget{
		{Addr: t.Addr, Request: req, Stream: true},
	}, nil
}

// broadcast 广播给所有已注册的 tool（定时触发场景）。
func (h *MainHubHandler) broadcast() ([]hubbase.DispatchTarget, error) {
	tools := registry.GetAllTools()
	if len(tools) == 0 {
		return nil, nil
	}

	// 每个 tool 单独 Build 一个新的 *pb.ToolRequest。
	// 不能用值拷贝（req := *defaultReq），因为 protobuf 结构体内含
	// sync.Mutex（via MessageState），值拷贝会触发 copylocks 警告并引发竞态。
	targets := make([]hubbase.DispatchTarget, 0, len(tools))
	for _, t := range tools {
		req, err := pb_api.Request().
			Method(t.Method).
			Params(map[string]int{"count": 1}).
			Build()
		if err != nil {
			return nil, err
		}
		targets = append(targets, hubbase.DispatchTarget{
			Addr:    t.Addr,
			Request: req,
			Stream:  true,
		})
	}
	return targets, nil
}

// ════════════════════════════════════════════════════════════
//  main
// ════════════════════════════════════════════════════════════

func main() {
	// ── 1. 初始化注册表（含热更新监听）──────────────────
	if err := registry.Init("config/registry.yaml"); err != nil {
		log.Fatalf("[Hub] registry init: %v", err)
	}

	// ── 2. 创建 Hub（预热流池）───────────────────────────
	hub := hubbase.New(&MainHubHandler{})

	// ── 3. 启动 gRPC 服务端（5s 定时广播）───────────────
	go func() {
		if err := hub.ServeAsync(":50051", 5*time.Second); err != nil {
			log.Fatalf("[Hub] ServeAsync: %v", err)
		}
	}()

	// 等待 gRPC 就绪
	time.Sleep(100 * time.Millisecond)

	// ── 4. 主动派发示例 ───────────────────────────────────

	// 示例 A：向 hello 发 count=3 的请求，等待所有响应帧
	log.Println("[Hub] === 示例 A: 向 hello 派发 ===")
	helloReq, _ := pb_api.Request().
		Method("Hello").
		Params(map[string]interface{}{
			"count":  3,
			"prefix": "Hi",
		}).
		Build()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	results := hub.Dispatch(ctx, helloReq)
	cancel()

	log.Printf("[Hub] hello 返回 %d 个目标结果", len(results))
	for _, r := range results {
		log.Printf("[Hub]   addr=%s ok=%v responses=%d err=%v",
			r.Target.Addr, r.AllOK(), len(r.Responses), r.Err)
	}

	// 示例 B：向 world 发请求，world 会返回 partial + ok 混合帧
	log.Println("[Hub] === 示例 B: 向 world 派发（partial 帧演示）===")
	worldReq, _ := pb_api.Request().
		Method("World").
		Params(map[string]interface{}{
			"name":  "MicroHub",
			"count": 3,
		}).
		Build()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	results2 := hub.Dispatch(ctx2, worldReq)
	cancel2()

	for _, r := range results2 {
		for i, resp := range r.Responses {
			log.Printf("[Hub]   frame[%d] status=%s result=%s", i, resp.Status, string(resp.Result))
		}
	}

	// 示例 C：走 DispatchSimpleCall（短连接，兼容场景）
	log.Println("[Hub] === 示例 C: DispatchSimpleCall ===")
	simpleReq, _ := pb_api.Request().
		Method("Hello").
		Params(map[string]int{"count": 1}).
		Build()

	ctx3, cancel3 := context.WithTimeout(context.Background(), 5*time.Second)
	resp, err := hub.DispatchSimpleCall(ctx3, simpleReq)
	cancel3()
	if err != nil {
		log.Printf("[Hub] DispatchSimpleCall err: %v", err)
	} else {
		log.Printf("[Hub] DispatchSimpleCall result: status=%s result=%s", resp.Status, string(resp.Result))
	}

	// ── 5. 主进程阻塞，保持服务运行 ─────────────────────
	log.Println("[Hub] 运行中，Ctrl+C 退出")
	select {}
}
