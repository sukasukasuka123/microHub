package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/sukasukasuka123/microHub/pb_api"
	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	"github.com/sukasukasuka123/microHub/root_class/tool"
)

// ── 请求/响应结构 ─────────────────────────────────────────

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

// ── HelloHandler ──────────────────────────────────────────
// 演示两个能力：
//   1. 基础：Execute 返回多帧响应
//   2. 进阶：实现 ActiveHandler，在流建立后定时主动推送心跳
//
// 注意：HelloHandler 是单例（tool.New 接收一个 handler 实例），
// 但 OnRegister 会被每条流调用一次（连接池有多少条流就调多少次）。
// 每条流有独立的 sender 生命周期，因此 heartbeat 必须绑定到 sender，
// 不能存到 h.sender 字段（会被后续流覆写，且并发不安全）。

type HelloHandler struct{}

func (h *HelloHandler) ServiceName() string { return "hello" }

// OnRegister 实现 tool.ActiveHandler 接口。
// 框架在每条双向流建立时调用，每条流对应独立的 sender。
// 不要把 sender 存到 handler 字段：handler 是单例，sender 是 per-stream 的。
func (h *HelloHandler) OnRegister(s tool.ActiveSender) {
	// 每条流独立启动一个心跳 goroutine，绑定到该流的 sender。
	// sender.Send 出错（流关闭）时 goroutine 自动退出，不会泄漏。
	go h.heartbeat(s)
}

func (h *HelloHandler) heartbeat(s tool.ActiveSender) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		resp, _ := pb_api.OKResp("hello", "", map[string]string{
			"type": "heartbeat",
			"from": "hello",
		})
		if err := s.Send(resp); err != nil {
			// sender 返回 error 说明该条流已关闭，正常退出
			log.Printf("[hello] heartbeat stopped: %v", err)
			return
		}
		log.Printf("[hello] heartbeat sent")
	}
}

// Execute 解析参数（同步），在 goroutine 里流式生产响应帧。
func (h *HelloHandler) Execute(req *pb.ToolRequest) (<-chan *pb.ToolResponse, error) {
	// ── 参数解析：同步、快速失败 ──────────────────────────
	var params HelloParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, fmt.Errorf("parse params: %w", err)
		}
	}

	// ── 默认值 + 简单校验 ─────────────────────────────────
	switch {
	case params.Count <= 0:
		params.Count = 1
	case params.Count > 10:
		return nil, fmt.Errorf("count %d exceeds max 10", params.Count)
	}
	if params.Prefix == "" {
		params.Prefix = "Hello"
	}

	// ── 生产响应帧（异步） ────────────────────────────────
	// 缓冲 = Count，生产方写完即关闭，不依赖消费速度
	ch := make(chan *pb.ToolResponse, params.Count)

	go func() {
		defer close(ch)

		for i := 1; i <= params.Count; i++ {
			isLast := i == params.Count

			var resp *pb.ToolResponse
			var err error
			if isLast {
				// 最后帧 status="ok"：框架侧 single_stream 收到后触发 finish()
				resp, err = pb_api.OKResp("hello", req.TaskId, HelloResult{
					Message: params.Prefix,
					From:    "hello",
					LoopIdx: i,
					Total:   params.Count,
				})
			} else {
				// 中间帧 status="partial"：框架继续等待后续帧
				resp, err = pb_api.PartialResp("hello", req.TaskId, HelloResult{
					Message: params.Prefix,
					From:    "hello",
					LoopIdx: i,
					Total:   params.Count,
				})
			}
			if err != nil {
				ch <- pb_api.ErrorResp("hello", req.TaskId, "BUILD_RESP", err.Error(), "")
				return
			}
			ch <- resp
			log.Printf("[hello] task=%s frame %d/%d last=%v", req.TaskId, i, params.Count, isLast)
		}
	}()

	return ch, nil
}

// ── main ──────────────────────────────────────────────────

func main() {
	t := tool.New(&HelloHandler{})
	log.Println("[hello] 启动 :50052")
	if err := t.Serve(":50052"); err != nil {
		log.Fatalf("[hello] %v", err)
	}
}
