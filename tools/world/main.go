package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	tool "github.com/sukasukasuka123/microHub/root_class/tool"
)

type WorldPayload struct {
	Message string `json:"message"`
	From    string `json:"from"`
	LoopIdx int    `json:"loop_idx"`
	Total   int    `json:"total"`
}

type WorldHandler struct{}

func (w *WorldHandler) ServiceName() string { return "world" }

func (w *WorldHandler) Execute(req *pb.ToolRequest) ([]*pb.ToolResponse, error) {
	count := parseCount(req.Params)
	resps := make([]*pb.ToolResponse, 0, count)
	for i := 1; i <= count; i++ {
		b, err := json.Marshal(WorldPayload{
			Message: "World",
			From:    w.ServiceName(),
			LoopIdx: i,
			Total:   count,
		})
		if err != nil {
			return nil, err
		}
		line := fmt.Sprintf("[%d/%d] %s", i, count, string(b))
		fmt.Println(line)
		resps = append(resps, &pb.ToolResponse{
			ServiceName: w.ServiceName(),
			Status:      "ok",
			Result:      line,
		})
	}
	return resps, nil
}

func main() {
	t := tool.New(&WorldHandler{})
	if err := t.Serve(":50053"); err != nil {
		log.Fatalf("%v", err)
	}
}

func parseCount(params map[string]string) int {
	if v, ok := params["count"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}
