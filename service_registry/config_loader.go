package service_registry

import (
	"fmt"
	"log"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
)

// changeCh 在注册表内容变化时发出信号（buffered 1，不阻塞发送方）。
// poolManager 监听这个 channel 来热更新流池。
var changeCh = make(chan struct{}, 1)

func ChangeCh() <-chan struct{} {
	return changeCh
}

func notifyChange() {
	select {
	case changeCh <- struct{}{}:
	default:
	}
}

// Init 首次加载配置并启动热更新监听。
func Init(configPath string) error {
	cfg, err := readConfig(configPath)
	if err != nil {
		return fmt.Errorf("[Registry] 首次加载失败: %w", err)
	}
	applyConfig(cfg)
	watchConfig(configPath)
	return nil
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

func applyConfig(cfg registryConfig) {
	replaceAll(cfg)
	log.Printf("[Registry] 配置已加载 — tools=%d hubs=%d",
		len(cfg.Services.Tools), len(cfg.Services.Hubs))
	PrintRegistry()
	notifyChange()
}

func watchConfig(configPath string) {
	viper.OnConfigChange(func(e fsnotify.Event) {
		log.Printf("[Registry] 检测到配置变化: %s", e.Name)
		cfg, err := readConfig(configPath)
		if err != nil {
			log.Printf("[Registry] 解析失败，跳过: %v", err)
			return
		}
		applyConfig(cfg)
	})
	viper.WatchConfig()
	log.Println("[Registry] 开始监听配置变化...")
}

// PrintRegistry 打印当前注册表状态，调试用。
func PrintRegistry() {
	tools := GetAllTools()
	cfg := GetGrpcPoolConfig()

	fmt.Println("\n=== tools ===")
	for i, t := range tools {
		online := "online"
		if IsOffline(t.Addr) {
			online = "offline"
		}
		in, _ := t.ParseInputSchema()
		out, _ := t.ParseOutputSchema()
		fmt.Printf("  [%s] [%d] name=%-10s addr=%-22s method=%-10s",
			online, i, t.Name, t.Addr, t.Method)
		if in != nil {
			fmt.Printf(" in=%s", in.Type)
		}
		if out != nil {
			fmt.Printf(" out=%s", out.Type)
		}
		fmt.Println()
	}

	fmt.Println("=== pool.grpc_conn ===")
	fmt.Printf("  min=%d max=%d idle_buf=%.2f survive=%ds monitor=%ds "+
		"retries=%d retry_ms=%d reconnect=%v ping=%ds\n",
		cfg.MinSize, cfg.MaxSize, cfg.IdleBufferFactor,
		cfg.SurviveTimeSec, cfg.MonitorIntervalSec,
		cfg.MaxRetries, cfg.RetryIntervalMs,
		cfg.ReconnectOnGet, cfg.PingIntervalSec,
	)
}
