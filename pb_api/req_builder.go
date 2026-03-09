// Package pb_api 提供 ToolRequest / ToolResponse 的公有构造 API。
// 开发者无需手动填充 proto 字段，直接调用链式 Builder 或快捷函数即可。
package pb_api

import (
	"encoding/json"
	"fmt"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
)

// ════════════════════════════════════════════════════════════
//  RequestBuilder — 构造 ToolRequest
// ════════════════════════════════════════════════════════════

// RequestBuilder 链式构造 ToolRequest。
// 零值可用，所有字段均为可选（框架层会补全 hub_name / task_id）。
type RequestBuilder struct {
	req *pb.ToolRequest
	err error // 首个错误被记录，Build() 时统一返回
}

// NewRequest 快捷构造：指定 method + 任意可序列化的 params。
//
//	req, err := pb_api.NewRequest("Hello", map[string]int{"count": 3})
func NewRequest(method string, params any) (*pb.ToolRequest, error) {
	return Request().Method(method).Params(params).Build()
}

// MustRequest 同 NewRequest，params 序列化失败时 panic（适合测试/固定参数场景）。
func MustRequest(method string, params any) *pb.ToolRequest {
	r, err := NewRequest(method, params)
	if err != nil {
		panic(fmt.Sprintf("MustRequest: %v", err))
	}
	return r
}

// Request 返回空白 RequestBuilder，支持链式调用。
func Request() *RequestBuilder {
	return &RequestBuilder{req: &pb.ToolRequest{}} // ← 初始化指针
}
func (b *RequestBuilder) HubName(name string) *RequestBuilder {
	b.req.HubName = name
	return b
}

func (b *RequestBuilder) TaskID(id string) *RequestBuilder {
	b.req.TaskId = id
	return b
}

func (b *RequestBuilder) Method(method string) *RequestBuilder {
	b.req.Method = method
	return b
}

// Params 接受任意可 JSON 序列化的值（struct、map、[]byte 均可）。
// 传入 []byte 时直接使用，无需二次序列化。
// Params 方法中修改赋值逻辑（其他链式方法不变）
func (b *RequestBuilder) Params(v any) *RequestBuilder {
	if b.err != nil {
		return b
	}
	switch p := v.(type) {
	case []byte:
		b.req.Params = p
	case json.RawMessage:
		b.req.Params = []byte(p)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			b.err = fmt.Errorf("RequestBuilder.Params marshal: %w", err)
			return b
		}
		b.req.Params = data
	}
	return b
}

// RawParams 直接设置已序列化的 []byte，跳过 JSON 检查。
func (b *RequestBuilder) RawParams(p []byte) *RequestBuilder {
	b.req.Params = p
	return b
}

// Build 返回构造好的 *pb.ToolRequest，首个错误一并返回。
// Build 直接返回指针，无需拷贝
func (b *RequestBuilder) Build() (*pb.ToolRequest, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.req, nil // ← 直接返回，无拷贝
}

// MustBuild 同 Build，出错时 panic。
func (b *RequestBuilder) MustBuild() *pb.ToolRequest {
	r, err := b.Build()
	if err != nil {
		panic(fmt.Sprintf("RequestBuilder.MustBuild: %v", err))
	}
	return r
}

// ════════════════════════════════════════════════════════════
//  ResponseBuilder — 构造 ToolResponse
// ════════════════════════════════════════════════════════════

// ResponseBuilder 链式构造 ToolResponse。
type ResponseBuilder struct {
	resp *pb.ToolResponse // ← 改为指针
	err  error
}

// Resp 返回空白 ResponseBuilder。
func Resp() *ResponseBuilder {
	return &ResponseBuilder{resp: &pb.ToolResponse{}} // ← 初始化指针
}

// OKResp 快捷构造成功响应。
//
//	resp, err := pb_api.OKResp("hello", taskID, myData)
func OKResp(toolName, taskID string, data any) (*pb.ToolResponse, error) {
	return Resp().ToolName(toolName).TaskID(taskID).StatusOK().Result(data).Build()
}

// MustOKResp 同 OKResp，出错 panic（适合测试场景）。
func MustOKResp(toolName, taskID string, data any) *pb.ToolResponse {
	r, err := OKResp(toolName, taskID, data)
	if err != nil {
		panic(fmt.Sprintf("MustOKResp: %v", err))
	}
	return r
}

// ErrorResp 快捷构造错误响应。
//
//	resp := pb_api.ErrorResp("hello", taskID, "INVALID_PARAM", "count must > 0", "count")
func ErrorResp(toolName, taskID, code, message, field string) *pb.ToolResponse {
	return Resp().ToolName(toolName).TaskID(taskID).StatusError().
		AddError(code, message, field, "").MustBuild()
}

// PartialResp 快捷构造部分成功响应（流式场景中间帧）。
func PartialResp(toolName, taskID string, data any) (*pb.ToolResponse, error) {
	return Resp().ToolName(toolName).TaskID(taskID).StatusPartial().Result(data).Build()
}

func (b *ResponseBuilder) ToolName(name string) *ResponseBuilder {
	b.resp.ToolName = name
	return b
}

func (b *ResponseBuilder) TaskID(id string) *ResponseBuilder {
	b.resp.TaskId = id
	return b
}

func (b *ResponseBuilder) StatusOK() *ResponseBuilder {
	b.resp.Status = "ok"
	return b
}

func (b *ResponseBuilder) StatusError() *ResponseBuilder {
	b.resp.Status = "error"
	return b
}

func (b *ResponseBuilder) StatusPartial() *ResponseBuilder {
	b.resp.Status = "partial"
	return b
}

// Status 直接设置状态字符串（"ok"/"error"/"partial"）。
func (b *ResponseBuilder) Status(s string) *ResponseBuilder {
	b.resp.Status = s
	return b
}

// Result 接受任意可 JSON 序列化的值。[]byte 直接使用。
func (b *ResponseBuilder) Result(v any) *ResponseBuilder {
	if b.err != nil {
		return b
	}
	switch r := v.(type) {
	case []byte:
		b.resp.Result = r
	case json.RawMessage:
		b.resp.Result = []byte(r)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			b.err = fmt.Errorf("ResponseBuilder.Result marshal: %w", err)
			return b
		}
		b.resp.Result = data
	}
	return b
}

// RawResult 直接设置已序列化的 []byte。
func (b *ResponseBuilder) RawResult(r []byte) *ResponseBuilder {
	b.resp.Result = r
	return b
}

// AddError 追加一条结构化错误。
func (b *ResponseBuilder) AddError(code, message, field, help string) *ResponseBuilder {
	b.resp.Errors = append(b.resp.Errors, &pb.ErrorDetail{
		Code:    code,
		Message: message,
		Field:   field,
		Help:    help,
	})
	return b
}

// AddErrorDetail 直接追加已构造的 ErrorDetail。
func (b *ResponseBuilder) AddErrorDetail(e *pb.ErrorDetail) *ResponseBuilder {
	b.resp.Errors = append(b.resp.Errors, e)
	return b
}

// Build 方法
func (b *ResponseBuilder) Build() (*pb.ToolResponse, error) {
	if b.err != nil {
		return nil, b.err
	}
	if b.resp.Status == "" {
		b.resp.Status = "ok"
	}
	if b.resp.Result == nil {
		b.resp.Result = []byte("{}")
	}
	return b.resp, nil // ← 直接返回指针
}

// MustBuild 同 Build，出错 panic。
func (b *ResponseBuilder) MustBuild() *pb.ToolResponse {
	r, err := b.Build()
	if err != nil {
		panic(fmt.Sprintf("ResponseBuilder.MustBuild: %v", err))
	}
	return r
}

// ════════════════════════════════════════════════════════════
//  ErrorDetail 构造辅助
// ════════════════════════════════════════════════════════════

// NewError 构造单条 ErrorDetail。
func NewError(code, message string) *pb.ErrorDetail {
	return &pb.ErrorDetail{Code: code, Message: message}
}

// NewErrorFull 构造带 field/help 的 ErrorDetail。
func NewErrorFull(code, message, field, help string) *pb.ErrorDetail {
	return &pb.ErrorDetail{Code: code, Message: message, Field: field, Help: help}
}
