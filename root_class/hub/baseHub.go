package hub

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	pb "github.com/RedHuang-0622/microHub/proto/gen/proto"
	registry "github.com/RedHuang-0622/microHub/service_registry"
	"google.golang.org/grpc"
)

var taskSeq atomic.Int64

func newTaskID(hubName string) string {
	return fmt.Sprintf("%s-%d-%d", hubName, time.Now().UnixNano(), taskSeq.Add(1))
}

// BaseHub 是框架提供的 Hub 基类。
// 业务方实现 HubHandler 接口定制路由和结果处理，
// 不需要关心流池管理、task_id 生成、并发安全等细节。
type BaseHub struct {
	pb.UnimplementedHubServiceServer
	handler HubHandler
	pm      *poolManager
	ctx     context.Context
	cancel  context.CancelFunc
}

func New(handler HubHandler) *BaseHub {
	ctx, cancel := context.WithCancel(context.Background())
	b := &BaseHub{
		handler: handler,
		pm:      newPoolManager(),
		ctx:     ctx,
		cancel:  cancel,
	}
	b.pm.rebuild(handler.Addrs())
	return b
}

// Serve 包级快捷函数。
func Serve(addr string, handler HubHandler, loopInterval time.Duration) error {
	return New(handler).serve(addr, loopInterval)
}

// ServeAsync 启动 gRPC 监听（阻塞直到退出）。
func (b *BaseHub) ServeAsync(addr string, loopInterval time.Duration) error {
	return b.serve(addr, loopInterval)
}

func (b *BaseHub) serve(addr string, loopInterval time.Duration) error {
	if loopInterval > 0 {
		go b.timerLoop(loopInterval)
	}
	go b.watchRegistry()

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("[%s] listen: %w", b.handler.ServiceName(), err)
	}
	srv := grpc.NewServer()
	pb.RegisterHubServiceServer(srv, b)
	log.Printf("[%s] gRPC 已启动 %s", b.handler.ServiceName(), addr)

	defer func() {
		b.cancel()
		b.pm.closeAll()
	}()
	return srv.Serve(lis)
}

// watchRegistry 监听注册表变化，触发流池热更新。
func (b *BaseHub) watchRegistry() {
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-registry.ChangeCh():
			b.pm.rebuild(b.handler.Addrs())
			log.Printf("[%s] 流池已热更新", b.handler.ServiceName())
		}
	}
}

// Handler 返回底层 HubHandler。
func (b *BaseHub) Handler() HubHandler { return b.handler }

// ── 对外 Dispatch API ────────────────────────────────────

// Dispatch Hub 主动向 Tool 派发任务（核心 API）。
func (b *BaseHub) Dispatch(ctx context.Context, req *pb.ToolRequest) []DispatchResult {
	targets, err := b.handler.Execute(req)
	if err != nil {
		log.Printf("[%s] Dispatch Execute err: %v", b.handler.ServiceName(), err)
		return nil
	}
	if len(targets) == 0 {
		method := ""
		if req != nil {
			method = req.GetMethod()
		}
		log.Printf("[%s] Dispatch 无目标 method=%s", b.handler.ServiceName(), method)
		return nil
	}
	results := b.dispatchAll(ctx, targets)
	b.handler.OnResults(results)
	return results
}

// DispatchSimpleCall Hub 主动向 Tool 发简单请求（兼容 Stream=false 场景）。
func (b *BaseHub) DispatchSimpleCall(ctx context.Context, req *pb.ToolRequest) (*pb.ToolResponse, error) {
	targets, err := b.handler.Execute(req)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no target for method=%s", req.GetMethod())
	}
	return b.callSimple(ctx, targets[0].Addr, targets[0].Request)
}

// ── gRPC Server 实现（Hub 作为被调用方）─────────────────

// DispatchSimple Hub 作为 gRPC Server 接受单次调用。
func (b *BaseHub) DispatchSimple(ctx context.Context, req *pb.ToolRequest) (*pb.ToolResponse, error) {
	targets, err := b.handler.Execute(req)
	if err != nil {
		return errResp(b.handler.ServiceName(), req.TaskId, "EXECUTE_FAILED", err.Error()), nil
	}
	results := b.dispatchAll(ctx, targets)
	b.handler.OnResults(results)
	return b.aggregate(results), nil
}

// DispatchStream Hub 作为 gRPC Server 接受双向流调用。
func (b *BaseHub) DispatchStream(stream pb.HubService_DispatchStreamServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		targets, err := b.handler.Execute(req)
		if err != nil {
			_ = stream.Send(errResp(b.handler.ServiceName(), req.TaskId, "EXECUTE_FAILED", err.Error()))
			continue
		}
		results := b.dispatchAll(stream.Context(), targets)
		b.handler.OnResults(results)
		for _, r := range results {
			if r.Err != nil {
				_ = stream.Send(errResp(b.handler.ServiceName(), req.TaskId, "DISPATCH_ERROR", r.Err.Error()))
				continue
			}
			for _, resp := range r.Responses {
				if err := stream.Send(resp); err != nil {
					return err
				}
			}
		}
	}
}

// ── 定时派发 ─────────────────────────────────────────────

func (b *BaseHub) timerLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			targets, err := b.handler.Execute(nil)
			if err != nil || len(targets) == 0 {
				continue
			}
			results := b.dispatchAll(b.ctx, targets)
			log.Printf("[%s] timeloop 完成 targets=%d results=%d", b.handler.ServiceName(), len(targets), len(results))
			b.handler.OnResults(results)
		}
	}
}

func errResp(hubName, taskID, code, msg string) *pb.ToolResponse {
	return &pb.ToolResponse{
		ToolName: hubName,
		TaskId:   taskID,
		Status:   "error",
		Result:   []byte("{}"),
		Errors:   []*pb.ErrorDetail{{Code: code, Message: msg}},
	}
}
