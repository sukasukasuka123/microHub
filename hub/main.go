package main

import (
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

// ServiceName 与 registry.yaml hub 的 name 字段一致
func (h *MainHubHandler) ServiceName() string { return "hub" }

// Execute 路由策略：
//
//	req == nil  → 定时触发，广播给所有 tool
//	req != nil  → 上游调用，按 service_name 或 method 路由
func (h *MainHubHandler) Execute(req *pb.ToolRequest) ([]hubbase.DispatchTarget, error) {
	if req == nil {
		return h.broadcast(), nil
	}
	if req.ServiceName != "" {
		return h.routeByName(req)
	}
	return h.routeByMethod(req)
}

// OnResults 记录派发结果日志
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

	// hub.Serve(监听地址, handler实现, 定时派发间隔)
	// 传 0 则不启动定时派发，只响应上游调用
	if err := hubbase.Serve(":50051", &MainHubHandler{}, 5*time.Second); err != nil {
		log.Fatalf("%v", err)
	}
}
