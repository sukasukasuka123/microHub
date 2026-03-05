package main

import (
	"encoding/json"
	"fmt"
	"log"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	tool "github.com/sukasukasuka123/microHub/root_class/tool"
)

// ── 请求参数结构（对应 registry.yaml input_schema）────────

type HelloRequest struct {
	Count  int    `json:"count"`  // 循环次数，默认 1
	Prefix string `json:"prefix"` // 前缀字符串，默认 "Hello"
}

// ── 响应数据结构（对应 registry.yaml output_schema）────────

type HelloResponse struct {
	Message string `json:"message"`
	From    string `json:"from"`
	LoopIdx int    `json:"loop_idx"`
	Total   int    `json:"total"`
}

// ── HelloHandler 实现 ─────────────────────────────────────

type HelloHandler struct{}

func (h *HelloHandler) ServiceName() string { return "hello" }

func (h *HelloHandler) Execute(req *pb.ToolRequest) ([]*pb.ToolResponse, error) {
	// 1️⃣ 解析参数：[]byte → HelloRequest
	var params HelloRequest
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, fmt.Errorf("parse params: %w", err)
		}
	}
	// 2️⃣ 应用默认值 + 简单校验
	if params.Count <= 0 {
		params.Count = 1
	}
	if params.Count > 10 {
		params.Count = 10 // Demo 限流，生产环境应返回错误
	}
	if params.Prefix == "" {
		params.Prefix = "Hello"
	}

	// 3️⃣ 执行业务逻辑：生成多条响应
	var resps []*pb.ToolResponse
	for i := 1; i <= params.Count; i++ {
		respData := HelloResponse{
			Message: params.Prefix,
			From:    h.ServiceName(),
			LoopIdx: i,
			Total:   params.Count,
		}

		// ✅ 使用工具函数：自动序列化 Result 为 []byte
		resp, err := tool.NewOKResp(h.ServiceName(), respData)
		if err != nil {
			return nil, fmt.Errorf("build response: %w", err)
		}
		resps = append(resps, resp)

		// 日志（可选）
		log.Printf("[%s] loop %d/%d: %+v", h.ServiceName(), i, params.Count, respData)
	}

	return resps, nil
}

func main() {
	handler := &HelloHandler{}
	t := tool.New(handler)
	log.Printf("[hello] 启动服务 :50052")
	if err := t.Serve(":50052"); err != nil {
		log.Fatalf("[hello] Serve failed: %v", err)
	}
}
