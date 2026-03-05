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

// ── MainHubHandler ────────────────────────────────────────

const defaultCount = 3

type MainHubHandler struct{}

func (h *MainHubHandler) ServiceName() string { return "hub" }

// Execute 路由策略：
//   - req == nil  → 定时触发，广播给所有 tool
//   - req != nil  → 上游调用，按 service_name 或 method 路由
func (h *MainHubHandler) Execute(req *pb.ToolRequest) ([]hubbase.DispatchTarget, error) {
	if req == nil {
		log.Println("[Hub] Execute: nil req (定时触发预留)")
		return nil, nil
	}
	if req.ServiceName != "" {
		return h.routeByName(req)
	}
	return h.routeByMethod(req)
}

// OnResults 处理派发结果：日志 + 可扩展的监控/告警
func (h *MainHubHandler) OnResults(results []hubbase.DispatchResult) {
	for _, r := range results {
		if r.Err != nil {
			// 派发层错误（如连接失败）
			log.Printf("[Hub] ✗ dispatch failed addr=%s err=%v", r.Target.Addr, r.Err)
			continue
		}
		for _, resp := range r.Responses {
			// ✅ 业务层响应：Result 是 []byte (JSON)
			if resp.Status == "ok" {
				log.Printf("[Hub] ✓ [%s] result=%s", resp.ServiceName, string(resp.Result))
			} else {
				// 记录结构化错误
				for _, e := range resp.Errors {
					log.Printf("[Hub] ✗ [%s] %s: %s (field=%s)",
						resp.ServiceName, e.Code, e.Message, e.Field)
				}
			}
		}
	}
}

// Addrs 返回所有已注册工具的地址，供连接池热更新使用
func (h *MainHubHandler) Addrs() []string {
	tools := registry.GetAllTools()
	addrs := make([]string, 0, len(tools))
	for _, t := range tools {
		addrs = append(addrs, t.Addr)
	}
	return addrs
}

// ── 路由辅助 ─────────────────────────────────────────────

// buildRequest 构造下游请求：透传原始请求，确保 Params 是 []byte
func (h *MainHubHandler) buildRequest(req *pb.ToolRequest, targetName, targetMethod string) *pb.ToolRequest {
	return &pb.ToolRequest{
		From:        h.ServiceName(),
		ServiceName: targetName,
		Method:      targetMethod,
		Params:      req.Params, // ✅ 直接透传 []byte，无需转换
	}
}

// broadcast 广播请求给所有注册工具（定时触发场景）
func (h *MainHubHandler) broadcast() []hubbase.DispatchTarget {
	// ✅ 构造默认参数（JSON 序列化）
	defaultParams, _ := json.Marshal(map[string]int{"count": defaultCount})

	var targets []hubbase.DispatchTarget
	for _, t := range registry.GetAllTools() {
		targets = append(targets, hubbase.DispatchTarget{
			Addr: t.Addr,
			Request: &pb.ToolRequest{
				From:        h.ServiceName(),
				ServiceName: t.Name,
				Method:      t.Method,
				Params:      defaultParams, // ✅ []byte 类型
			},
			Stream: true,
		})
	}
	return targets
}

// routeByName 按 service_name 路由到单个工具
func (h *MainHubHandler) routeByName(req *pb.ToolRequest) ([]hubbase.DispatchTarget, error) {
	t, ok := registry.SelectToolByName(req.ServiceName)
	if !ok {
		log.Printf("[Hub] tool=%s 未注册", req.ServiceName)
		return nil, nil
	}
	return []hubbase.DispatchTarget{
		{Addr: t.Addr, Request: h.buildRequest(req, t.Name, t.Method), Stream: true},
	}, nil
}

// routeByMethod 按 method 路由到单个工具
func (h *MainHubHandler) routeByMethod(req *pb.ToolRequest) ([]hubbase.DispatchTarget, error) {
	t, ok := registry.SelectToolByMethod(req.Method)
	if !ok {
		log.Printf("[Hub] method=%s 未注册", req.Method)
		return nil, nil
	}
	return []hubbase.DispatchTarget{
		{Addr: t.Addr, Request: h.buildRequest(req, t.Name, t.Method), Stream: true},
	}, nil
}

// ── main ──────────────────────────────────────────────────

func main() {
	// 1️⃣ 初始化注册表（监听 yaml 热更新）
	if err := registry.Init("config/registry.yaml"); err != nil {
		log.Fatalf("[Hub] 注册表初始化失败: %v", err)
	}

	// 2️⃣ 创建 BaseHub 实例（建好连接池，不监听端口）
	hub := hubbase.New(&MainHubHandler{})

	// 3️⃣ 启动 gRPC 监听（异步，复用当前实例的连接池）
	//    定时广播间隔 5s，传 0 则不启动定时派发
	go func() {
		if err := hub.ServeAsync(":50051", 5*time.Second); err != nil {
			log.Fatalf("[Hub] ServeAsync 退出: %v", err)
		}
	}()

	// 4️⃣ 等待 gRPC 服务就绪（简单 sleep，生产环境可用 TCP 探测）
	time.Sleep(100 * time.Millisecond)

	// 5️⃣ 主动发送测试请求（按 service_name 路由）
	// ✅ 参数需序列化为 []byte
	helloParams, _ := json.Marshal(map[string]int{"count": defaultCount})
	worldParams, _ := json.Marshal(map[string]int{"count": defaultCount})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	hub.Dispatch(ctx, &pb.ToolRequest{
		ServiceName: "hello",
		Params:      helloParams, // ✅ []byte 类型
	})
	cancel()

	ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
	hub.Dispatch(ctx1, &pb.ToolRequest{
		ServiceName: "world",
		Params:      worldParams, // ✅ []byte 类型
	})
	cancel1()

	// 6️⃣ 主进程阻塞，保持 gRPC 服务持续运行
	log.Println("[Hub] 主进程阻塞中，按 Ctrl+C 退出")
	select {}
}
