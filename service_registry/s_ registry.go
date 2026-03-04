package service_registry

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
)

// ── 服务元数据 ────────────────────────────────────────────

type ToolEntry struct {
	Name   string `yaml:"name"   mapstructure:"name"`
	Addr   string `yaml:"addr"   mapstructure:"addr"`
	Method string `yaml:"method" mapstructure:"method"`
}

type HubEntry struct {
	Name         string `yaml:"name"          mapstructure:"name"`
	Addr         string `yaml:"addr"          mapstructure:"addr"`
	RegisteredAt string `yaml:"registered_at" mapstructure:"registered_at"`
}

// ── 连接池配置（从 yaml pool.grpc_conn 节读取） ───────────

type GrpcPoolConfig struct {
	MinSize            int64   `mapstructure:"min_size"`
	MaxSize            int64   `mapstructure:"max_size"`
	IdleBufferFactor   float64 `mapstructure:"idle_buffer_factor"`
	SurviveTimeSec     int     `mapstructure:"survive_time_sec"`
	MonitorIntervalSec int     `mapstructure:"monitor_interval_sec"`
	MaxRetries         int     `mapstructure:"max_retries"`
	RetryIntervalMs    int     `mapstructure:"retry_interval_ms"`
	ReconnectOnGet     bool    `mapstructure:"reconnect_on_get"`
}

// SurviveTime / MonitorInterval / RetryInterval 转换为 time.Duration，
// 方便直接传入 PoolConfig。
func (c GrpcPoolConfig) SurviveTime() time.Duration {
	return time.Duration(c.SurviveTimeSec) * time.Second
}
func (c GrpcPoolConfig) MonitorInterval() time.Duration {
	return time.Duration(c.MonitorIntervalSec) * time.Second
}
func (c GrpcPoolConfig) RetryInterval() time.Duration {
	return time.Duration(c.RetryIntervalMs) * time.Millisecond
}

// ── yaml 整体映射 ─────────────────────────────────────────

type registryConfig struct {
	Services struct {
		Tools []ToolEntry `mapstructure:"tools"`
		Hubs  []HubEntry  `mapstructure:"hubs"`
	} `mapstructure:"services"`
	Pool struct {
		GrpcConn GrpcPoolConfig `mapstructure:"grpc_conn"`
	} `mapstructure:"pool"`
}

// ── 注册表状态 ────────────────────────────────────────────

var (
	mu       sync.RWMutex
	tools    []ToolEntry
	Hubs     []HubEntry
	grpcPool GrpcPoolConfig

	passedMu    sync.RWMutex
	passedTools = make(map[string]bool)
	// ── 新增：配置变更通知 channel ──
	changeCh = make(chan struct{}, 1)
)

// ChangeCh 返回只读的配置变更通知 channel，供 Hub 等外部组件阻塞监听。
func ChangeCh() <-chan struct{} {
	return changeCh
}

// ── Tool API ──────────────────────────────────────────────

func GetAllTools() []ToolEntry {
	mu.RLock()
	defer mu.RUnlock()
	cp := make([]ToolEntry, len(tools))
	copy(cp, tools)
	return cp
}

func SelectToolByName(name string) (ToolEntry, bool) {
	mu.RLock()
	defer mu.RUnlock()
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return ToolEntry{}, false
}

func SelectToolByMethod(method string) (ToolEntry, bool) {
	mu.RLock()
	defer mu.RUnlock()
	for _, t := range tools {
		if t.Method == method {
			return t, true
		}
	}
	return ToolEntry{}, false
}

// ── Hub API ───────────────────────────────────────────────

func GetAllHubs() []HubEntry {
	mu.RLock()
	defer mu.RUnlock()
	cp := make([]HubEntry, len(Hubs))
	copy(cp, Hubs)
	return cp
}

// ── 连接池配置 API ────────────────────────────────────────

// GetGrpcPoolConfig 返回当前 gRPC 连接池配置快照。
// BaseHub 在创建新连接池时调用，热更新后新建的池会使用新配置。
func GetGrpcPoolConfig() GrpcPoolConfig {
	mu.RLock()
	defer mu.RUnlock()
	return grpcPool
}

// ── 测试通过状态 API ──────────────────────────────────────

func MarkPassed(toolName string) {
	passedMu.Lock()
	defer passedMu.Unlock()
	passedTools[toolName] = true
	log.Printf("[Registry] tool=%s 已通过测试", toolName)
}

func MarkFailed(toolName string) {
	passedMu.Lock()
	defer passedMu.Unlock()
	passedTools[toolName] = false
	log.Printf("[Registry] tool=%s 测试失败，已移除", toolName)
}

func IsPassed(toolName string) bool {
	passedMu.RLock()
	defer passedMu.RUnlock()
	return passedTools[toolName]
}

func GetPassedTools() []ToolEntry {
	mu.RLock()
	defer mu.RUnlock()
	passedMu.RLock()
	defer passedMu.RUnlock()
	var result []ToolEntry
	for _, t := range tools {
		if passedTools[t.Name] {
			result = append(result, t)
		}
	}
	return result
}

// ── 打印 ──────────────────────────────────────────────────

func PrintRegistry() {
	mu.RLock()
	defer mu.RUnlock()
	passedMu.RLock()
	defer passedMu.RUnlock()

	fmt.Println("\n=== tools ===")
	for i, t := range tools {
		mark := "[ ]"
		if passedTools[t.Name] {
			mark = "[✓]"
		}
		fmt.Printf("  %s [%d] name=%-10s addr=%-22s method=%s\n",
			mark, i, t.Name, t.Addr, t.Method)
	}
	fmt.Println("=== pool.grpc_conn ===")
	fmt.Printf("  min=%d max=%d idle_buf=%.2f survive=%ds monitor=%ds retries=%d retry_ms=%d reconnect=%v\n",
		grpcPool.MinSize, grpcPool.MaxSize, grpcPool.IdleBufferFactor,
		grpcPool.SurviveTimeSec, grpcPool.MonitorIntervalSec,
		grpcPool.MaxRetries, grpcPool.RetryIntervalMs, grpcPool.ReconnectOnGet,
	)
}

// ── 内部 ──────────────────────────────────────────────────

func replaceAll(cfg registryConfig) {
	mu.Lock()
	defer mu.Unlock()
	tools = cfg.Services.Tools
	Hubs = cfg.Services.Hubs
	grpcPool = cfg.Pool.GrpcConn
}

func readConfig(configPath string) (registryConfig, error) {
	var cfg registryConfig
	viper.SetConfigFile(configPath)
	viper.SetConfigType("yaml")
	if err := viper.ReadInConfig(); err != nil {
		return cfg, err
	}
	return cfg, viper.Unmarshal(&cfg)
}

func onConfigUpdated(cfg registryConfig) {
	replaceAll(cfg)
	log.Printf("[Registry] 注册表已更新 — tools=%d Hubs=%d",
		len(cfg.Services.Tools), len(cfg.Services.Hubs))
	PrintRegistry()

	// ── 新增：非阻塞投递变更信号（buffer=1，防止积压） ──
	select {
	case changeCh <- struct{}{}:
	default:
	}
}

func Watch(configPath string) {
	viper.OnConfigChange(func(e fsnotify.Event) {
		log.Printf("[Registry] 检测到 yaml 变化: %s", e.Name)
		cfg, err := readConfig(configPath)
		if err != nil {
			log.Printf("[Registry] 解析失败，跳过: %v", err)
			return
		}
		onConfigUpdated(cfg)
	})
	viper.WatchConfig()
	log.Println("[Registry] 开始监听 yaml 变化...")
}

func Init(configPath string) error {
	cfg, err := readConfig(configPath)
	if err != nil {
		return fmt.Errorf("[Registry] 首次加载失败: %w", err)
	}
	onConfigUpdated(cfg)
	Watch(configPath)
	return nil
}
