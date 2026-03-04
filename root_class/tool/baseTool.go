package tool

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"

	"google.golang.org/grpc"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
)

// ── ToolHandler：子类实现的接口 ───────────────────────────

type ToolHandler interface {
	// ServiceName 与 registry.yaml 的 name 字段一致，用于身份校验和日志
	ServiceName() string
	// Execute 接收请求，返回响应列表；业务逻辑完全由子类决定
	Execute(req *pb.ToolRequest) ([]*pb.ToolResponse, error)
}

// ── BaseTool ──────────────────────────────────────────────

type BaseTool struct {
	pb.UnimplementedHubServiceServer
	handler ToolHandler
}

func New(handler ToolHandler) *BaseTool {
	return &BaseTool{handler: handler}
}

// ── gRPC 方法实现 ─────────────────────────────────────────

// baseTool.go
func (b *BaseTool) DispatchSimple(_ context.Context, req *pb.ToolRequest) (*pb.ToolResponse, error) {
	log.Printf("[%s] DispatchSimple from=%s method=%s params=%v",
		b.handler.ServiceName(), req.From, req.Method, req.Params)

	if err := b.validateTarget(req); err != nil {
		return errorResp(b.handler.ServiceName(), err.Error()), nil
	}

	resps, err := b.handler.Execute(req)
	if err != nil {
		return errorResp(b.handler.ServiceName(), err.Error()), nil
	}

	// ── 修改点：Simple 语义 = 单响应，多条只取第一条并告警 ──
	if len(resps) == 0 {
		return errorResp(b.handler.ServiceName(), "execute returned no response"), nil
	}
	if len(resps) > 1 {
		log.Printf("[%s] DispatchSimple: Execute 返回 %d 条响应，仅取第一条；多响应请用 DispatchStream",
			b.handler.ServiceName(), len(resps))
	}
	return resps[0], nil
}

func (b *BaseTool) DispatchStream(stream pb.HubService_DispatchStreamServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		log.Printf("[%s] DispatchStream from=%s method=%s params=%v",
			b.handler.ServiceName(), req.From, req.Method, req.Params)

		if err := b.validateTarget(req); err != nil {
			if sendErr := stream.Send(errorResp(b.handler.ServiceName(), err.Error())); sendErr != nil {
				return sendErr
			}
			continue
		}

		resps, err := b.handler.Execute(req)
		if err != nil {
			if sendErr := stream.Send(errorResp(b.handler.ServiceName(), err.Error())); sendErr != nil {
				return sendErr
			}
			continue
		}

		for _, resp := range resps {
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
}

// ── Serve：构造实例并启动 gRPC 监听 ──────────────────────

func (b *BaseTool) Serve(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("[%s] 监听失败: %w", b.handler.ServiceName(), err)
	}
	srv := grpc.NewServer()
	pb.RegisterHubServiceServer(srv, b)
	log.Printf("[%s] 启动 gRPC 服务 %s", b.handler.ServiceName(), addr)
	return srv.Serve(lis)
}

// ── 内部辅助 ──────────────────────────────────────────────

// validateTarget 校验 service_name 是否与自身匹配
// service_name 为空时跳过（兼容广播场景）
func (b *BaseTool) validateTarget(req *pb.ToolRequest) error {
	if req.ServiceName != "" && req.ServiceName != b.handler.ServiceName() {
		return fmt.Errorf("service_name mismatch: got=%s want=%s",
			req.ServiceName, b.handler.ServiceName())
	}
	return nil
}

func errorResp(serviceName, msg string) *pb.ToolResponse {
	return &pb.ToolResponse{
		ServiceName: serviceName,
		Status:      "error",
		Result:      msg,
	}
}
