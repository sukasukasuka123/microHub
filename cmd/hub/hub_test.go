package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/sukasukasuka123/microHub/proto/gen/proto"
	hubbase "github.com/sukasukasuka123/microHub/root_class/hub"
	registry "github.com/sukasukasuka123/microHub/service_registry"
)

// ════════════════════════════════════════════════════════════
//  测试初始化
//  所有用例共享同一个 hub 实例，避免重复建连接池
// ════════════════════════════════════════════════════════════

var testHub *hubbase.BaseHub

func TestMain(m *testing.M) {
	// ── 1. 启动 tool 子进程（若外部未启动）──────────────
	// 检测端口是否已在监听：外部手动跑了 go run ./cmd/tools/hello 时跳过，
	// 避免重复 bind 导致 "address already in use"。
	var helloCmd, worldCmd *exec.Cmd
	if !portInUse(":50052") {
		helloCmd = startTool("../../cmd/tools/hello", ":50052")
		defer helloCmd.Process.Kill()
	}
	if !portInUse(":50053") {
		worldCmd = startTool("../../cmd/tools/world", ":50053")
		_ = worldCmd
		defer worldCmd.Process.Kill()
	}

	// ── 2. 等 tool 端口就绪 ──────────────────────────────
	waitPort(":50052", 5*time.Second)
	waitPort(":50053", 5*time.Second)

	// ── 3. 初始化 registry，使用测试专用小配置 ───────────
	// benchmark 时调大 pool size 以体现并发能力；
	// 单测时 min=2 max=8 足够，避免预热 24 条流耗时。
	if os.Getenv("GRPC_POOL_MIN") == "" {
		os.Setenv("GRPC_POOL_MIN", "2")
	}
	if os.Getenv("GRPC_POOL_MAX") == "" {
		os.Setenv("GRPC_POOL_MAX", "8")
	}
	if err := registry.Init("../../config/registry.yaml"); err != nil {
		panic(fmt.Sprintf("registry init: %v", err))
	}

	testHub = hubbase.New(&MainHubHandler{})

	go func() {
		if err := testHub.ServeAsync(":50051", 0); err != nil {
			// 端口已被占用（前一次测试未清理）：直接忽略，testHub 仍可用于发请求
			if !portInUse(":50051") {
				panic(fmt.Sprintf("ServeAsync: %v", err))
			}
		}
	}()

	time.Sleep(300 * time.Millisecond)
	m.Run()
}

// portInUse 检测 addr 是否已有进程在监听。
func portInUse(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// startTool 用 go run 启动 tool 子进程，stdout/stderr 继承到测试输出。
func startTool(pkg, addr string) *exec.Cmd {
	cmd := exec.Command("go", "run", pkg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		panic(fmt.Sprintf("start tool %s: %v", pkg, err))
	}
	return cmd
}

// waitPort 轮询直到 addr 可 TCP 连接，或超时 panic。
func waitPort(addr string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic(fmt.Sprintf("waitPort: %s 未在 %v 内就绪", addr, timeout))
}

// ── 辅助 ─────────────────────────────────────────────────

func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal: %v", err))
	}
	return b
}

// newReq 构造路由到指定 method 的请求。
// 注意：hub 的 routeByMethod 读 req.Method，
// registry 里注册的 method 名称与 yaml 中 method 字段一致（大小写敏感）。
func newReq(method string, params interface{}) *pb.ToolRequest {
	return &pb.ToolRequest{
		Method: method,
		Params: mustMarshal(params),
	}
}

// ════════════════════════════════════════════════════════════
//  单元测试
// ════════════════════════════════════════════════════════════

// TestDispatch_Hello 基本冒烟：发 count=3，期望收到 3 帧。
// hello 与 world 语义一致：中间帧 status="partial"，最后帧 status="ok"。
func TestDispatch_Hello(t *testing.T) {
	const count = 3
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results := testHub.Dispatch(ctx, newReq("Hello", map[string]int{"count": count}))

	if len(results) == 0 {
		t.Fatal("results 为空，可能路由未命中或 tool 未响应")
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("dispatch err addr=%s: %v", r.Target.Addr, r.Err)
		}
		if got := len(r.Responses); got != count {
			t.Errorf("期望 %d 帧，实际 %d 帧", count, got)
		}
		n := len(r.Responses)
		for i, resp := range r.Responses {
			isLast := i == n-1
			want := "partial"
			if isLast {
				want = "ok"
			}
			if resp.Status != want {
				t.Errorf("帧[%d] status=%q，期望 %q errors=%v", i, resp.Status, want, resp.Errors)
			}
			var data map[string]interface{}
			if err := json.Unmarshal(resp.Result, &data); err != nil {
				t.Errorf("帧[%d] Result 非法 JSON: %v raw=%s", i, err, resp.Result)
			}
		}
	}
}

// TestDispatch_World world 返回 partial+ok 混合帧：
// 中间帧 status="partial"，最后帧 status="ok"，共 count 帧
func TestDispatch_World(t *testing.T) {
	const count = 3
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results := testHub.Dispatch(ctx, newReq("World", map[string]int{"count": count}))

	if len(results) == 0 {
		t.Fatal("results 为空")
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("dispatch err addr=%s: %v", r.Target.Addr, r.Err)
		}
		if got := len(r.Responses); got != count {
			t.Errorf("期望 %d 帧，实际 %d 帧", count, got)
		}

		n := len(r.Responses)
		for i, resp := range r.Responses {
			isLast := i == n-1
			if isLast {
				if resp.Status != "ok" {
					t.Errorf("最后帧 status=%q，期望 ok", resp.Status)
				}
			} else {
				if resp.Status != "partial" {
					t.Errorf("中间帧[%d] status=%q，期望 partial", i, resp.Status)
				}
			}
		}
	}
}

// TestDispatch_UnknownMethod 路由未命中：未注册的 method，Dispatch 应返回 nil
func TestDispatch_UnknownMethod(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results := testHub.Dispatch(ctx, newReq("NonexistentTool", map[string]interface{}{}))

	// routeByMethod 找不到 → Execute 返回 nil targets → Dispatch 返回 nil
	if results != nil {
		t.Errorf("期望 nil，实际 %v", results)
	}
}

// TestDispatch_Timeout ctx 极短，验证超时后不 panic、正确返回 ctx.Err
func TestDispatch_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	// 不验证具体结果，只确保不 panic
	results := testHub.Dispatch(ctx, newReq("Hello", map[string]int{"count": 3}))
	for _, r := range results {
		if r.Err != nil && r.Err != context.DeadlineExceeded {
			t.Logf("超时结果 err=%v（可接受）", r.Err)
		}
	}
}

// TestDispatch_InvalidParams 非法 JSON 参数：tool Execute 返回 error 帧
func TestDispatch_InvalidParams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results := testHub.Dispatch(ctx, &pb.ToolRequest{
		Method: "Hello",
		Params: []byte(`{invalid json}`),
	})

	if len(results) == 0 {
		t.Fatal("期望有结果")
	}
	for _, r := range results {
		if r.Err != nil {
			t.Logf("dispatch 层错误（可接受）: %v", r.Err)
			return
		}
		for _, resp := range r.Responses {
			if resp.Status == "error" && len(resp.Errors) > 0 {
				t.Logf("✓ 收到预期错误: [%s] %s", resp.Errors[0].Code, resp.Errors[0].Message)
				return
			}
		}
	}
	t.Error("期望收到 status=error 的结构化错误帧")
}

// TestDispatch_Concurrent 并发安全：多 goroutine 同时 Dispatch，不得有数据竞争
func TestDispatch_Concurrent(t *testing.T) {
	const goroutines = 20

	var wg sync.WaitGroup
	var errCount atomic.Int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			method := "Hello"
			if idx%2 == 0 {
				method = "World"
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			results := testHub.Dispatch(ctx, newReq(method, map[string]int{"count": 2}))
			for _, r := range results {
				if r.Err != nil {
					errCount.Add(1)
					t.Logf("goroutine %d err: %v", idx, r.Err)
				}
			}
		}(i)
	}

	wg.Wait()
	if n := errCount.Load(); n > 0 {
		t.Errorf("并发测试中有 %d 个 dispatch 失败", n)
	}
}

// TestDispatchSimpleCall_Hello 走短连接 DispatchSimpleCall，验证单帧返回
func TestDispatchSimpleCall_Hello(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// DispatchSimpleCall 内部也走 handler.Execute 路由，但用短连接发到 tool
	resp, err := testHub.DispatchSimpleCall(ctx, newReq("Hello", map[string]int{"count": 1}))
	if err != nil {
		t.Fatalf("DispatchSimpleCall err: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status=%q errors=%v", resp.Status, resp.Errors)
	}
	var data map[string]interface{}
	if err := json.Unmarshal(resp.Result, &data); err != nil {
		t.Errorf("Result 非法 JSON: %v", err)
	}
}

// TestAllOK_Helper 验证 DispatchResult.AllOK() 辅助方法的判断逻辑
func TestAllOK_Helper(t *testing.T) {
	ok := hubbase.DispatchResult{
		Responses: []*pb.ToolResponse{{Status: "ok"}, {Status: "ok"}},
	}
	if !ok.AllOK() {
		t.Error("全 ok 帧应返回 AllOK()=true")
	}

	hasPartial := hubbase.DispatchResult{
		Responses: []*pb.ToolResponse{{Status: "partial"}, {Status: "ok"}},
	}
	if hasPartial.AllOK() {
		t.Error("含 partial 帧应返回 AllOK()=false")
	}

	hasErr := hubbase.DispatchResult{Err: fmt.Errorf("dispatch failed")}
	if hasErr.AllOK() {
		t.Error("有 Err 时应返回 AllOK()=false")
	}
}

// ════════════════════════════════════════════════════════════
//  压测
//
//  单测（pool min=2 max=8，快速）：
//    go test ./cmd/hub -run=^Test -v
//
//  压测（pool 用生产配置，先手动启动 tool 进程）：
//    go run ./cmd/tools/hello &
//    go run ./cmd/tools/world &
//    GRPC_POOL_MIN=24 GRPC_POOL_MAX=130 //      go test ./cmd/hub -bench=BenchmarkDispatch_Concurrency -benchtime=5s -v
// ════════════════════════════════════════════════════════════

func BenchmarkDispatch_Hello(b *testing.B) {
	ctx := context.Background()
	req := newReq("Hello", map[string]int{"count": 1})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		testHub.Dispatch(ctx, req)
	}
}

// BenchmarkDispatch_Parallel 并发 Dispatch 吞吐（RunParallel，GOMAXPROCS 个 goroutine）。
// 这是衡量流池并发复用能力的核心 benchmark：
// 串行 benchmark（Hello/SimpleCall）每次等响应返回才发下一个，流池里 24 条流只用了 1 条；
// 并发 benchmark 才能真正压到流池，体现多流并发的吞吐优势。
func BenchmarkDispatch_Parallel(b *testing.B) {
	ctx := context.Background()
	req := newReq("Hello", map[string]int{"count": 1})
	b.ResetTimer()
	b.RunParallel(func(pbt *testing.PB) {
		for pbt.Next() {
			testHub.Dispatch(ctx, req)
		}
	})
}

// BenchmarkDispatch_Concurrency 固定并发 goroutine 数压测，对标池子的测法。
// 每个子 benchmark 启动 c 个 goroutine 同时持续打请求，b.N 均分到每个 goroutine。
// ns/op 反映的是单次请求在该并发压力下的平均延迟。
// 运行：go test ./cmd/hub -bench=BenchmarkDispatch_Concurrency -benchtime=5s -v
func BenchmarkDispatch_Concurrency(b *testing.B) {
	concurrencies := []int{10, 50, 100, 200, 500}

	for _, c := range concurrencies {
		c := c
		b.Run(fmt.Sprintf("concurrency=%d", c), func(b *testing.B) {
			ctx := context.Background()
			req := newReq("Hello", map[string]int{"count": 1})

			b.ResetTimer()

			// 每个 goroutine 分到 b.N/c 个请求（最少 1 个）
			perG := b.N / c
			if perG < 1 {
				perG = 1
			}

			var wg sync.WaitGroup
			wg.Add(c)
			for i := 0; i < c; i++ {
				go func() {
					defer wg.Done()
					for j := 0; j < perG; j++ {
						testHub.Dispatch(ctx, req)
					}
				}()
			}
			wg.Wait()
		})
	}
}

// BenchmarkDispatch_MultiTool 同时向 hello + world 并发广播。
// hub.Execute(nil) 触发 broadcast，让 dispatchAll 并行派发两个 tool，
// 测量的是真正并发广播的延迟（应接近单次而非两倍）。
func BenchmarkDispatch_MultiTool(b *testing.B) {
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// req=nil 触发 MainHubHandler.Execute 里的 broadcast() 分支
		testHub.Dispatch(ctx, nil)
	}
}

// BenchmarkDispatchSimpleCall 短连接吞吐，与长连接对比用。
// allocs 远高于长连接（~90 vs ~37）是 gRPC unary RPC 的结构性开销：
// 每次请求需要独立的 header/metadata/response 分配，无法通过缓存 client 消除。
// 长连接流复用已建立的 HTTP/2 stream，开销更低。SimpleCall 适合低频、一次性查询。
func BenchmarkDispatchSimpleCall(b *testing.B) {
	ctx := context.Background()
	req := newReq("Hello", map[string]int{"count": 1})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		testHub.DispatchSimpleCall(ctx, req)
	}
}
