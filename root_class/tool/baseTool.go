// root_class/tool/basetool.go
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"

	"google.golang.org/grpc"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
)

// ── ToolHandler：最小必要接口 ─────────────────────────────

type ToolHandler interface {
	ServiceName() string
	Execute(req *pb.ToolRequest) ([]*pb.ToolResponse, error)
	// ✅ 移除 GetSchema：schema 用于文档/配置，校验由业务方在 Execute 中完成
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

func (b *BaseTool) DispatchSimple(_ context.Context, req *pb.ToolRequest) (*pb.ToolResponse, error) {
	// ✅ 精简日志：只打印必要信息
	log.Printf("[%s] DispatchSimple from=%s method=%s params_len=%d",
		b.handler.ServiceName(), req.From, req.Method, len(req.Params))

	if err := b.validateTarget(req); err != nil {
		return b.errorResp("TARGET_VALIDATE", err.Error(), "service_name"), nil
	}

	// ✅ 业务执行：参数校验 + 业务逻辑完全由 handler.Execute 决定
	resps, err := b.handler.Execute(req)
	if err != nil {
		// ✅ 业务错误：直接包装返回，不重复校验
		return b.errorResp("EXECUTE_FAILED", err.Error(), ""), nil
	}

	// ── Simple 语义 = 单响应 ──
	if len(resps) == 0 {
		return b.errorResp("EMPTY_RESPONSE", "Execute returned no response", ""), nil
	}
	if len(resps) > 1 {
		log.Printf("[%s] DispatchSimple: Execute 返回 %d 条响应，仅取第一条；多响应请用 DispatchStream",
			b.handler.ServiceName(), len(resps))
	}

	first := resps[0]
	if first.Status == "" {
		first.Status = "ok"
	}
	return first, nil
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

		log.Printf("[%s] DispatchStream from=%s method=%s params_len=%d",
			b.handler.ServiceName(), req.From, req.Method, len(req.Params))

		if err := b.validateTarget(req); err != nil {
			_ = stream.Send(b.errorResp("TARGET_VALIDATE", err.Error(), "service_name"))
			continue
		}

		resps, err := b.handler.Execute(req)
		if err != nil {
			_ = stream.Send(b.errorResp("EXECUTE_FAILED", err.Error(), ""))
			continue
		}

		for _, resp := range resps {
			if resp.Status == "" {
				resp.Status = "ok"
			}
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
}

// ── Serve ─────────────────────────────────────────────────

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

func (b *BaseTool) validateTarget(req *pb.ToolRequest) error {
	if req.ServiceName != "" && req.ServiceName != b.handler.ServiceName() {
		return fmt.Errorf("service_name mismatch: got=%s want=%s",
			req.ServiceName, b.handler.ServiceName())
	}
	return nil
}

func (b *BaseTool) errorResp(code, msg, field string) *pb.ToolResponse {
	return &pb.ToolResponse{
		ServiceName: b.handler.ServiceName(),
		Status:      "error",
		Result:      []byte("{}"), // 错误时返回空对象，保持 JSON 兼容性
		Errors: []*pb.ErrorDetail{
			{
				Code:    code,
				Message: msg,
				Field:   field,
			},
		},
	}
}

// ── 业务方辅助函数（保持不变）──────────────────────────────

// NewOKResp 构造成功响应，自动序列化 data 为 JSON bytes
func NewOKResp(serviceName string, data interface{}) (*pb.ToolResponse, error) {
	resultBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}
	return &pb.ToolResponse{
		ServiceName: serviceName,
		Status:      "ok",
		Result:      resultBytes,
		Errors:      nil,
	}, nil
}

// NewErrorDetail 快速构造单个错误详情
func NewErrorDetail(code, message, field, help string) *pb.ErrorDetail {
	return &pb.ErrorDetail{
		Code:    code,
		Message: message,
		Field:   field,
		Help:    help,
	}
}

// MergeErrors 合并多个错误详情到响应中
func MergeErrors(resp *pb.ToolResponse, errors ...*pb.ErrorDetail) {
	if resp.Errors == nil {
		resp.Errors = make([]*pb.ErrorDetail, 0, len(errors))
	}
	resp.Errors = append(resp.Errors, errors...)
	if resp.Status == "ok" && len(resp.Errors) > 0 {
		resp.Status = "error"
	}
}
