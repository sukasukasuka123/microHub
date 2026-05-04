package hub

import (
	"context"
	"fmt"
	"sync"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	"google.golang.org/protobuf/proto"
)

// dispatchAll 并发向所有目标发送任务，等待全部完成后返回。
func (b *BaseHub) dispatchAll(ctx context.Context, targets []DispatchTarget) []DispatchResult {
	results := make([]DispatchResult, len(targets))
	var wg sync.WaitGroup

	for i, t := range targets {
		wg.Add(1)
		go func(idx int, target DispatchTarget) {
			defer wg.Done()

			// proto.Clone 深拷贝，避免并发写同一个 protobuf 对象
			req := proto.Clone(target.Request).(*pb.ToolRequest)
			if req.HubName == "" {
				req.HubName = b.handler.ServiceName()
			}
			if req.TaskId == "" {
				req.TaskId = newTaskID(b.handler.ServiceName())
			}

			var resps []*pb.ToolResponse
			var err error

			if target.Stream {
				resps, err = b.callStream(ctx, target.Addr, req)
			} else {
				var resp *pb.ToolResponse
				resp, err = b.callSimple(ctx, target.Addr, req)
				if err == nil && resp != nil {
					resps = []*pb.ToolResponse{resp}
				}
			}
			results[idx] = DispatchResult{Target: target, Responses: resps, Err: err}
		}(i, t)
	}

	wg.Wait()
	return results
}

// callStream 通过流池发送任务，阻塞等待所有响应帧返回。
func (b *BaseHub) callStream(ctx context.Context, addr string, req *pb.ToolRequest) ([]*pb.ToolResponse, error) {
	sp := b.pm.get(addr)
	if sp == nil {
		return nil, fmt.Errorf("no stream pool for addr=%s", addr)
	}

	res, err := sp.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("StreamPool.Get addr=%s: %w", addr, err)
	}
	stream := res.Conn

	task, err := stream.Send(req)
	if err != nil {
		// 流损坏不归还，Pool 的 Reset/Ping 负责清理和补充
		return nil, err
	}
	sp.Put(res)

	var responses []*pb.ToolResponse
	for {
		select {
		case resp := <-task.resChan:
			responses = append(responses, resp)
		case <-task.done:
			// done 关闭后排空 resChan 中可能残留的帧
			for {
				select {
				case resp := <-task.resChan:
					responses = append(responses, resp)
				default:
					return responses, nil
				}
			}
		case <-ctx.Done():
			return responses, ctx.Err()
		}
	}
}

// callSimple 复用缓存的 gRPC client 走短连接。
func (b *BaseHub) callSimple(ctx context.Context, addr string, req *pb.ToolRequest) (*pb.ToolResponse, error) {
	sp := b.pm.get(addr)
	if sp == nil {
		return nil, fmt.Errorf("no stream pool for addr=%s", addr)
	}
	if req.HubName == "" {
		req.HubName = b.handler.ServiceName()
	}
	if req.TaskId == "" {
		req.TaskId = newTaskID(b.handler.ServiceName())
	}
	return sp.client.DispatchSimple(ctx, req)
}

// aggregate 把多个 DispatchResult 合并成一个 ToolResponse。
// 有错误且有成功 → partial；全部失败 → error；全部成功 → ok。
func (b *BaseHub) aggregate(results []DispatchResult) *pb.ToolResponse {
	var allErrors []*pb.ErrorDetail
	var parts [][]byte

	for _, r := range results {
		if r.Err != nil {
			allErrors = append(allErrors, &pb.ErrorDetail{
				Code:    "DISPATCH_ERROR",
				Message: fmt.Sprintf("[%s] %v", r.Target.Addr, r.Err),
				Field:   "target.addr",
			})
			continue
		}
		for _, resp := range r.Responses {
			allErrors = append(allErrors, resp.Errors...)
			if len(resp.Result) > 0 {
				parts = append(parts, resp.Result)
			}
		}
	}

	status := "ok"
	if len(allErrors) > 0 {
		if len(parts) > 0 {
			status = "partial"
		} else {
			status = "error"
		}
	}

	result := buildResultJSON(parts)

	return &pb.ToolResponse{
		ToolName: b.handler.ServiceName(),
		Status:   status,
		Result:   result,
		Errors:   allErrors,
	}
}

// buildResultJSON 把多个 JSON 片段合并。
// 0个 → {}；1个 → 直接返回；多个 → [a,b,c]
func buildResultJSON(parts [][]byte) []byte {
	switch len(parts) {
	case 0:
		return []byte("{}")
	case 1:
		return parts[0]
	default:
		sz := 2
		for _, p := range parts {
			sz += len(p) + 1
		}
		buf := make([]byte, 0, sz)
		buf = append(buf, '[')
		for i, p := range parts {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, p...)
		}
		return append(buf, ']')
	}
}
