package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	tool "github.com/sukasukasuka123/microHub/root_class/tool"
)

// ── 请求参数结构 ─────────────────────────────────────────

type WorldRequest struct {
	Name  string `json:"name"`  // 称呼，默认 "World"
	Times int    `json:"count"` // 重复次数，默认 1
}

// ── 响应数据结构 ─────────────────────────────────────────

type WorldResponse struct {
	Greeting  string `json:"greeting"`
	Iteration int    `json:"iteration"`
}

// ── WorldHandler 实现 ────────────────────────────────────

type WorldHandler struct{}

func (w *WorldHandler) ServiceName() string { return "world" }

func (w *WorldHandler) Execute(req *pb.ToolRequest) ([]*pb.ToolResponse, error) {
	// 1️⃣ 解析参数
	var params WorldRequest
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, fmt.Errorf("parse params: %w", err)
		}
	}
	// 2️⃣ 默认值 + 校验
	if params.Name == "" {
		params.Name = "World"
	}
	if params.Times <= 0 {
		params.Times = 1
	}
	if params.Times > 5 {
		params.Times = 5
	}

	// 3️⃣ 执行业务：生成带感叹号的问候
	var resps []*pb.ToolResponse
	for i := 1; i <= params.Times; i++ {
		greeting := fmt.Sprintf("%s%s", strings.Repeat("!", i), params.Name)
		respData := WorldResponse{
			Greeting:  greeting,
			Iteration: i,
		}

		resp, err := tool.NewOKResp(w.ServiceName(), respData)
		if err != nil {
			return nil, fmt.Errorf("build response: %w", err)
		}
		resps = append(resps, resp)

		log.Printf("[%s] %s (iteration %d)", w.ServiceName(), greeting, i)
	}

	return resps, nil
}

func main() {
	handler := &WorldHandler{}
	t := tool.New(handler)
	log.Printf("[world] 启动服务 :50053")
	if err := t.Serve(":50053"); err != nil {
		log.Fatalf("[world] Serve failed: %v", err)
	}
}
