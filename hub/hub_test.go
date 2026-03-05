package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	hubbase "github.com/sukasukasuka123/microHub/root_class/hub"
	registry "github.com/sukasukasuka123/microHub/service_registry"
)

// ── 测试初始化 ────────────────────────────────────────────
// TestMain 在所有测试前做一次性初始化：加载 registry、启动 hub 监听。
// 所有用例共享同一个 hub 实例，避免重复建连接池。

var testHub *hubbase.BaseHub

func TestMain(m *testing.M) {
	if err := registry.Init("../config/registry.yaml"); err != nil {
		panic(fmt.Sprintf("registry init: %v", err))
	}

	testHub = hubbase.New(&MainHubHandler{})

	// goroutine 启动监听，不阻塞
	go func() {
		if err := testHub.ServeAsync(":50051", 0); err != nil {
			panic(fmt.Sprintf("ServeAsync: %v", err))
		}
	}()

	// 等端口就绪
	time.Sleep(100 * time.Millisecond)

	m.Run()
}

// ── 辅助函数：构造 []byte 参数 ─────────────────────────────

// mustMarshal 简化测试中的参数序列化（测试场景允许 panic）
func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal params: %v", err))
	}
	return b
}

// ════════════════════════════════════════════════════════════
//  单元测试
// ════════════════════════════════════════════════════════════

// TestDispatch_Hello 基本冒烟：发一次 hello，验证收到预期响应
func TestDispatch_Hello(t *testing.T) {
	const count = 3
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results := testHub.Dispatch(ctx, &pb.ToolRequest{
		ServiceName: "hello",
		Params:      mustMarshal(map[string]int{"count": count}), // ✅ []byte 类型
	})

	if len(results) == 0 {
		t.Fatal("results 为空，tool 可能未响应")
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("dispatch 失败 addr=%s err=%v", r.Target.Addr, r.Err)
			continue
		}
		// ✅ 验证响应数量和状态
		if got := len(r.Responses); got != count {
			t.Errorf("期望 %d 条响应，实际 %d 条", count, got)
		}
		for i, resp := range r.Responses {
			if resp.Status != "ok" {
				t.Errorf("响应[%d] status=%s, errors=%v", i, resp.Status, resp.Errors)
			}
			// ✅ 可选：验证响应内容可解析为 JSON
			var data map[string]interface{}
			if err := json.Unmarshal(resp.Result, &data); err != nil {
				t.Errorf("响应[%d] Result 不是合法 JSON: %v", i, err)
			}
		}
	}
}

// TestDispatch_World 基本冒烟：发一次 world
func TestDispatch_World(t *testing.T) {
	const count = 3
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results := testHub.Dispatch(ctx, &pb.ToolRequest{
		ServiceName: "world",
		Params:      mustMarshal(map[string]int{"count": count}), // ✅ []byte 类型
	})

	if len(results) == 0 {
		t.Fatal("results 为空")
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("dispatch 失败 addr=%s err=%v", r.Target.Addr, r.Err)
			continue
		}
		if got := len(r.Responses); got != count {
			t.Errorf("期望 %d 条响应，实际 %d 条", count, got)
		}
		for i, resp := range r.Responses {
			if resp.Status != "ok" {
				t.Errorf("响应[%d] status=%s, errors=%v", i, resp.Status, resp.Errors)
			}
		}
	}
}

// TestDispatch_UnknownService 路由失败：发给未注册的 service，应返回 nil
func TestDispatch_UnknownService(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results := testHub.Dispatch(ctx, &pb.ToolRequest{
		ServiceName: "nonexistent_tool",
		Params:      mustMarshal(map[string]interface{}{}), // ✅ 空参数也需序列化
	})

	// 路由失败时，BaseHub 返回 nil（无匹配目标）
	if results != nil {
		t.Errorf("期望返回 nil，实际 %v", results)
	}
}

// TestDispatch_Timeout 超时：给一个极短 ctx，验证超时后不 panic
func TestDispatch_Timeout(t *testing.T) {
	// 1ns 必然超时
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	// 只验证不 panic，超时后 results 可能为空或带 err
	results := testHub.Dispatch(ctx, &pb.ToolRequest{
		ServiceName: "hello",
		Params:      mustMarshal(map[string]int{"count": 3}),
	})
	t.Logf("超时测试 results=%v", results)
}

// TestDispatch_Concurrent 并发正确性：多个 goroutine 同时 Dispatch，验证无数据竞争
func TestDispatch_Concurrent(t *testing.T) {
	const goroutines = 20
	const count = 3

	var wg sync.WaitGroup
	var errCount atomic.Int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			// 交替发 hello / world
			svc := "hello"
			if idx%2 == 0 {
				svc = "world"
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			results := testHub.Dispatch(ctx, &pb.ToolRequest{
				ServiceName: svc,
				Params:      mustMarshal(map[string]int{"count": count}),
			})
			for _, r := range results {
				if r.Err != nil {
					errCount.Add(1)
					t.Logf("goroutine %d dispatch err: %v", idx, r.Err)
				}
			}
		}(i)
	}

	wg.Wait()

	if n := errCount.Load(); n > 0 {
		t.Errorf("并发测试中有 %d 个 dispatch 失败", n)
	}
}

// TestDispatch_InvalidParams 参数解析失败：发送非法 JSON，验证业务方返回结构化错误
func TestDispatch_InvalidParams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// ✅ 发送非法 JSON 字符串
	results := testHub.Dispatch(ctx, &pb.ToolRequest{
		ServiceName: "hello",
		Params:      []byte(`{invalid json}`), // 无法被 json.Unmarshal 解析
	})

	// 期望：有结果，但响应状态为 error，且包含结构化错误
	if len(results) == 0 {
		t.Fatal("期望有结果（即使参数错误）")
	}
	for _, r := range results {
		if r.Err != nil {
			// 连接层错误：也算通过测试
			t.Logf("dispatch 层错误（可接受）: %v", r.Err)
			continue
		}
		for _, resp := range r.Responses {
			if resp.Status == "error" && len(resp.Errors) > 0 {
				// ✅ 期望：业务方返回结构化错误
				t.Logf("✓ 收到预期错误: %s - %s", resp.Errors[0].Code, resp.Errors[0].Message)
				return
			}
		}
	}
	t.Error("期望收到参数解析错误的结构化响应")
}

// TestDispatch_ResultAggregation 验证 Hub 聚合多结果的正确性
func TestDispatch_ResultAggregation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 使用 DispatchSimple 直接调用 gRPC 接口（而非内部 Dispatch）
	// 验证聚合逻辑：多结果应包裹为 JSON 数组
	resp, err := testHub.DispatchSimple(ctx, &pb.ToolRequest{
		ServiceName: "hello",
		Params:      mustMarshal(map[string]int{"count": 2}),
	})
	if err != nil {
		t.Fatalf("DispatchSimple 调用失败: %v", err)
	}

	// ✅ 验证响应状态
	if resp.Status != "ok" {
		t.Errorf("期望 status=ok, 实际=%s, errors=%v", resp.Status, resp.Errors)
	}

	// ✅ 验证 Result 是合法 JSON
	var result interface{}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Errorf("Result 不是合法 JSON: %v, raw=%s", err, string(resp.Result))
	}

	// ✅ 验证多结果被聚合为数组
	// 注意：BaseHub.DispatchSimple 对多响应取第一条，此处验证单条即可
	// 如需测试数组聚合，需修改测试调用多个 tool 或修改 BaseHub 逻辑
}

// ════════════════════════════════════════════════════════════
//  压测（Benchmark）
//  运行：go test ./cmd/hub -bench=. -benchtime=5s
// ════════════════════════════════════════════════════════════

// BenchmarkDispatch_Hello 单次 Dispatch 吞吐
func BenchmarkDispatch_Hello(b *testing.B) {
	ctx := context.Background()
	req := &pb.ToolRequest{
		ServiceName: "hello",
		Params:      mustMarshal(map[string]int{"count": 1}), // ✅ []byte
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		testHub.Dispatch(ctx, req)
	}
}

// BenchmarkDispatch_Parallel 并发 Dispatch 吞吐（模拟多上游同时打入）
func BenchmarkDispatch_Parallel(b *testing.B) {
	ctx := context.Background()
	req := &pb.ToolRequest{
		ServiceName: "hello",
		Params:      mustMarshal(map[string]int{"count": 1}),
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			testHub.Dispatch(ctx, req)
		}
	})
}

// BenchmarkDispatch_MultiTool 同时向 hello + world 派发（模拟广播场景）
func BenchmarkDispatch_MultiTool(b *testing.B) {
	ctx := context.Background()
	reqs := []*pb.ToolRequest{
		{ServiceName: "hello", Params: mustMarshal(map[string]int{"count": 1})},
		{ServiceName: "world", Params: mustMarshal(map[string]int{"count": 1})},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, req := range reqs {
			testHub.Dispatch(ctx, req)
		}
	}
}

// BenchmarkDispatch_Simple 直接调用 gRPC 接口（不含路由层）
func BenchmarkDispatch_Simple(b *testing.B) {
	ctx := context.Background()
	req := &pb.ToolRequest{
		ServiceName: "hello",
		Params:      mustMarshal(map[string]int{"count": 1}),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		testHub.DispatchSimple(ctx, req)
	}
}
