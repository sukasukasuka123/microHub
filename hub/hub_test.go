package main

import (
	"context"
	"fmt"
	"strconv"
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

// ════════════════════════════════════════════════════════════
//  单元测试
// ════════════════════════════════════════════════════════════

// TestDispatch_Hello 基本冒烟：发一次 hello，验证收到预期响应数量
func TestDispatch_Hello(t *testing.T) {
	const count = 3
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results := testHub.Dispatch(ctx, &pb.ToolRequest{
		ServiceName: "hello",
		Params:      map[string]string{"count": strconv.Itoa(count)},
	})

	if len(results) == 0 {
		t.Fatal("results 为空，tool 可能未响应")
	}
	for _, r := range results {
		if !r.AllOK() {
			t.Errorf("dispatch 失败 addr=%s err=%v", r.Target.Addr, r.Err)
			continue
		}
		if got := len(r.Responses); got != count {
			t.Errorf("期望 %d 条响应，实际 %d 条", count, got)
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
		Params:      map[string]string{"count": strconv.Itoa(count)},
	})

	if len(results) == 0 {
		t.Fatal("results 为空")
	}
	for _, r := range results {
		if !r.AllOK() {
			t.Errorf("dispatch 失败 addr=%s err=%v", r.Target.Addr, r.Err)
		}
		if got := len(r.Responses); got != count {
			t.Errorf("期望 %d 条响应，实际 %d 条", count, got)
		}
	}
}

// TestDispatch_UnknownService 路由失败：发给未注册的 service，应返回 nil
func TestDispatch_UnknownService(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results := testHub.Dispatch(ctx, &pb.ToolRequest{
		ServiceName: "nonexistent_tool",
		Params:      map[string]string{"count": "1"},
	})

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
		Params:      map[string]string{"count": "3"},
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
				Params:      map[string]string{"count": strconv.Itoa(count)},
			})
			for _, r := range results {
				if !r.AllOK() {
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

// ════════════════════════════════════════════════════════════
//  压测（Benchmark）
//  运行：go test ./hub/ -bench=. -benchtime=5s
// ════════════════════════════════════════════════════════════

// BenchmarkDispatch_Hello 单次 Dispatch 吞吐
func BenchmarkDispatch_Hello(b *testing.B) {
	ctx := context.Background()
	req := &pb.ToolRequest{
		ServiceName: "hello",
		Params:      map[string]string{"count": "1"},
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
		Params:      map[string]string{"count": "1"},
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
		{ServiceName: "hello", Params: map[string]string{"count": "1"}},
		{ServiceName: "world", Params: map[string]string{"count": "1"}},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 两个 tool 串行触发（每次 Dispatch 内部是并发的）
		for _, req := range reqs {
			testHub.Dispatch(ctx, req)
		}
	}
}
