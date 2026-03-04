package main

import (
	"context"
	"log"
	"strconv"
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
//
//	req == nil  → 定时触发，广播给所有 tool
//	req != nil  → 上游调用，按 service_name 或 method 路由
func (h *MainHubHandler) Execute(req *pb.ToolRequest) ([]hubbase.DispatchTarget, error) {
	if req == nil {
		log.Println("nil req!")
		return nil, nil
	}
	if req.ServiceName != "" {
		return h.routeByName(req)
	}
	return h.routeByMethod(req)
}

func (h *MainHubHandler) OnResults(results []hubbase.DispatchResult) {
	for _, r := range results {
		if !r.AllOK() {
			log.Printf("[Hub] ✗ addr=%s err=%v", r.Target.Addr, r.Err)
			continue
		}
		for _, resp := range r.Responses {
			log.Printf("[Hub] ✓ [%s] %s", resp.ServiceName, resp.Result)
		}
	}
}

func (h *MainHubHandler) Addrs() []string {
	tools := registry.GetAllTools()
	addrs := make([]string, len(tools))
	for i, t := range tools {
		addrs[i] = t.Addr
	}
	return addrs
}

// ── 路由辅助 ─────────────────────────────────────────────

func (h *MainHubHandler) broadcast() []hubbase.DispatchTarget {
	var targets []hubbase.DispatchTarget
	for _, t := range registry.GetAllTools() {
		targets = append(targets, hubbase.DispatchTarget{
			Addr: t.Addr,
			Request: &pb.ToolRequest{
				From:        h.ServiceName(),
				ServiceName: t.Name,
				Method:      t.Method,
				Params:      map[string]string{"count": strconv.Itoa(defaultCount)},
			},
			Stream: true,
		})
	}
	return targets
}

func (h *MainHubHandler) routeByName(req *pb.ToolRequest) ([]hubbase.DispatchTarget, error) {
	t, ok := registry.SelectToolByName(req.ServiceName)
	if !ok {
		log.Printf("[Hub] tool=%s 未注册", req.ServiceName)
		return nil, nil
	}
	req.From = h.ServiceName()
	return []hubbase.DispatchTarget{
		{Addr: t.Addr, Request: req, Stream: true},
	}, nil
}

func (h *MainHubHandler) routeByMethod(req *pb.ToolRequest) ([]hubbase.DispatchTarget, error) {
	t, ok := registry.SelectToolByMethod(req.Method)
	if !ok {
		log.Printf("[Hub] method=%s 未注册", req.Method)
		return nil, nil
	}
	req.From = h.ServiceName()
	return []hubbase.DispatchTarget{
		{Addr: t.Addr, Request: req, Stream: true},
	}, nil
}

// ── main ──────────────────────────────────────────────────

func main() {
	if err := registry.Init("config/registry.yaml"); err != nil {
		log.Fatalf("%v", err)
	}

	// New 初始化 BaseHub，建好连接池，不监听端口
	hub := hubbase.New(&MainHubHandler{})

	// ── 先在 goroutine 里启动 gRPC 监听 ─────────────────────
	// ServeAsync 复用当前 hub 实例的连接池，与后续 Dispatch 共享同一套连接。
	// 定时广播间隔 5s，传 0 则不启动定时派发。
	go func() {
		if err := hub.ServeAsync(":50051", 5*time.Second); err != nil {
			log.Fatalf("[Hub] ServeAsync 退出: %v", err)
		}
	}()

	// 等待 gRPC 服务就绪（端口 listen 完成）再发送
	// 简单 sleep 足够；生产环境可换成对 :50051 的 TCP 探测循环
	time.Sleep(100 * time.Millisecond)

	// ── 主动发送：按 service_name 路由到 hello ───────────────
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	hub.Dispatch(ctx, &pb.ToolRequest{
		ServiceName: "hello",
		Params:      map[string]string{"count": strconv.Itoa(defaultCount)},
	})
	cancel()

	ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
	hub.Dispatch(ctx1, &pb.ToolRequest{
		ServiceName: "world",
		Params:      map[string]string{"count": strconv.Itoa(defaultCount)},
	})
	cancel1()

	// ── 主进程阻塞，保持 gRPC 服务持续运行 ──────────────────
	select {}
}
