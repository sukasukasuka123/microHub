package hub

import pb "github.com/RedHuang-0622/microHub/proto/gen/proto"

// DispatchTarget 描述一次派发的目标与请求。
// Stream=true  → 走长连接双向流池（推荐）
// Stream=false → 走 DispatchSimple 短连接（兼容简单场景）
type DispatchTarget struct {
	Addr    string
	Request *pb.ToolRequest
	Stream  bool
}

// DispatchResult 单次派发的聚合结果。
type DispatchResult struct {
	Target    DispatchTarget
	Responses []*pb.ToolResponse
	Err       error
}

// AllOK 当且仅当无派发错误且所有响应均为 "ok" 时返回 true。
func (r DispatchResult) AllOK() bool {
	if r.Err != nil {
		return false
	}
	for _, resp := range r.Responses {
		if resp.Status != "ok" {
			return false
		}
	}
	return true
}

// HubHandler 由业务方实现，负责路由策略和结果处理。
type HubHandler interface {
	ServiceName() string
	// Execute 根据请求决定派发目标；req==nil 表示定时触发。
	Execute(req *pb.ToolRequest) ([]DispatchTarget, error)
	// OnResults 在所有目标响应聚合完毕后调用（日志/监控/告警）。
	OnResults(results []DispatchResult)
	// Addrs 返回当前所有在线 Tool 的地址，供连接池热更新使用。
	Addrs() []string
}
