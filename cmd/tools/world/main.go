package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/sukasukasuka123/microHub/pb_api"
	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	"github.com/sukasukasuka123/microHub/root_class/tool"
)

// ── 请求/响应结构 ─────────────────────────────────────────

type WorldParams struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type WorldResult struct {
	Greeting  string `json:"greeting"`
	Iteration int    `json:"iteration"`
}

// ── WorldHandler ──────────────────────────────────────────
// 演示：
//   1. 参数校验失败直接 return nil, err（框架包装为错误帧）
//   2. 流式返回多帧，中间帧用 StatusPartial，最后帧用 StatusOK
//      这样 Hub 侧知道何时任务真正结束

type WorldHandler struct{}

func (w *WorldHandler) ServiceName() string { return "world" }

func (w *WorldHandler) Execute(req *pb.ToolRequest) (<-chan *pb.ToolResponse, error) {
	// ── 参数解析 ──────────────────────────────────────────
	var params WorldParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, fmt.Errorf("parse params: %w", err)
		}
	}

	// ── 校验：非法参数直接 return error，不分配 chan ──────
	if params.Count < 0 {
		return nil, fmt.Errorf("count must be >= 0, got %d", params.Count)
	}

	// ── 默认值 ────────────────────────────────────────────
	if params.Name == "" {
		params.Name = "World"
	}
	if params.Count == 0 {
		params.Count = 1
	}
	if params.Count > 5 {
		params.Count = 5
	}

	ch := make(chan *pb.ToolResponse, params.Count)

	go func() {
		defer close(ch)

		for i := 1; i <= params.Count; i++ {
			greeting := fmt.Sprintf("%s%s", strings.Repeat("!", i), params.Name)

			isLast := i == params.Count

			var resp *pb.ToolResponse
			var err error

			if isLast {
				// 最后一帧：status="ok"，Hub 侧 pendingTask.finish() 被触发
				resp, err = pb_api.OKResp("world", req.TaskId, WorldResult{
					Greeting:  greeting,
					Iteration: i,
				})
			} else {
				// 中间帧：status="partial"，Hub 持续收集
				resp, err = pb_api.PartialResp("world", req.TaskId, WorldResult{
					Greeting:  greeting,
					Iteration: i,
				})
			}

			if err != nil {
				ch <- pb_api.ErrorResp("world", req.TaskId, "BUILD_RESP", err.Error(), "")
				return
			}

			ch <- resp
			log.Printf("[world] task=%s %s (iter %d/%d, last=%v)",
				req.TaskId, greeting, i, params.Count, isLast)
		}
	}()

	return ch, nil
}

// ── main ──────────────────────────────────────────────────

func main() {
	t := tool.New(&WorldHandler{})
	log.Println("[world] 启动 :50053")
	if err := t.Serve(":50053"); err != nil {
		log.Fatalf("[world] %v", err)
	}
}
