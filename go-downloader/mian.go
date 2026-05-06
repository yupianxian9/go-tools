// main.go
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 从 YAML 文件中读取的配置
type Config struct {
	// 原始下载链接列表（必填）
	Urls []string `yaml:"urls"`
	// 加速链接前缀（可选，例如 https://mirror.ghproxy.com/）
	AcceleratePrefix string `yaml:"accelerate_prefix"`
	// 是否启用加速
	EnableAccelerate bool `yaml:"enable_accelerate"`
	// 文件保存目录（默认为当前目录下的 downloads）
	OutputDir string `yaml:"output_dir"`
	// 下载失败时的重试次数（默认为 3）
	MaxRetries int `yaml:"max_retries"`
	// 代理地址（可选，例如 http://127.0.0.1:7890）
	Proxy string `yaml:"proxy"`
	// 下载缓冲区大小 (字节) ，默认 16MB
	BufferSize int `yaml:"buffer_size"`
	// 进度更新间隔（秒），默认 5 秒
	ProgressInterval int `yaml:"progress_interval"`
	// 并发下载数量（默认为 3）
	Concurrency int `yaml:"concurrency"`
}

// DownloadStats 全局下载统计，用于进度条展示
type DownloadStats struct {
	totalDownloaded int64
	totalSize       int64
	// 记录每个 URL 是否已经将大小贡献到 totalSize
	sizeContributed sync.Map
	// 缓存的各文件远端大小
	urlSizes sync.Map
}

// 原子地增加已下载的字节数
func (s *DownloadStats) AddDownloaded(n int64) {
	atomic.AddInt64(&s.totalDownloaded, n)
}

// 获取当前的已下载字节数和总大小
func (s *DownloadStats) Snapshot() (downloaded, totalSize int64) {
	downloaded = atomic.LoadInt64(&s.totalDownloaded)
	totalSize = atomic.LoadInt64(&s.totalSize)
	return
}

// 如果该 URL 的文件大小尚未贡献给 totalSize，则加上并记录下来
func (s *DownloadStats) ContributeSize(url string, size int64) {
	if _, loaded := s.sizeContributed.LoadOrStore(url, true); !loaded {
		atomic.AddInt64(&s.totalSize, size)
	}
	// 缓存远端大小，供后续断点判断使用
	s.urlSizes.Store(url, size)
}

// 从 URL 中提取文件名
func getFilenameFromURL(rawUrl string) string {
	parts := strings.Split(rawUrl, "/")
	if len(parts) == 0 {
		return "unknown_file"
	}
	return parts[len(parts)-1]
}

// 根据配置生成实际的下载链接
func getDownloadURL(cfg *Config, original string) string {
	if cfg.EnableAccelerate && cfg.AcceleratePrefix != "" {
		if strings.HasPrefix(original, cfg.AcceleratePrefix) {
			return original
		}
		return cfg.AcceleratePrefix + original
	}
	return original
}

// 创建配置好的 HTTP 客户端（代理、超时等）
func newHTTPClient(proxy string) (*http.Client, error) {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}
	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   0, // 不设置总超时，用于大文件下载
	}, nil
}

// 单次下载尝试（不包含重试逻辑），返回错误则调用者可重试
func downloadFileOnce(ctx context.Context, client *http.Client, originalURL string, cfg *Config, stats *DownloadStats) error {
	downloadURL := getDownloadURL(cfg, originalURL)
	filename := getFilenameFromURL(originalURL)
	outputPath := filepath.Join(cfg.OutputDir, filename)

	// 确保输出目录存在
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	// 检查本地已有文件大小
	localSize := int64(0)
	if info, err := os.Stat(outputPath); err == nil {
		localSize = info.Size()
	}

	// 尝试获取远端文件大小（如果之前没有缓存）
	var remoteSize int64 = -1
	if cachedSize, ok := stats.urlSizes.Load(originalURL); ok {
		remoteSize = cachedSize.(int64)
	} else {
		// 尝试 HEAD 请求
		headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, downloadURL, nil)
		if err == nil {
			headResp, err := client.Do(headReq)
			if err == nil && headResp.StatusCode == http.StatusOK {
				if cls := headResp.Header.Get("Content-Length"); cls != "" {
					if sz, err := strconv.ParseInt(cls, 10, 64); err == nil {
						remoteSize = sz
						stats.ContributeSize(originalURL, remoteSize)
					}
				}
				headResp.Body.Close()
			}
			if headResp != nil {
				headResp.Body.Close()
			}
		}
	}

	// 如果已知远端大小且本地已完整，则跳过下载
	if remoteSize >= 0 && localSize == remoteSize {
		stats.ContributeSize(originalURL, remoteSize)
		fmt.Printf("✅ %s already completed, skip.\n", filename)
		return nil
	}
	// 本地大于远端，删除重新下载
	if remoteSize >= 0 && localSize > remoteSize {
		os.Remove(outputPath)
		localSize = 0
	}

	// 构造带断点续传 Range 的请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if localSize > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", localSize))
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// 服务器忽略了 Range，或没有断点续传，返回全文
		if localSize > 0 {
			// 本地有部分文件，需要从头下载，先删除
			os.Remove(outputPath)
			return fmt.Errorf("server ignored Range, restart download")
		}
		// 从头下载
		if remoteSize < 0 {
			if cls := resp.Header.Get("Content-Length"); cls != "" {
				if sz, err := strconv.ParseInt(cls, 10, 64); err == nil {
					remoteSize = sz
					stats.ContributeSize(originalURL, remoteSize)
				}
			}
		}
	case http.StatusPartialContent:
		// 断点续传正常
		if remoteSize < 0 {
			// 从 Content-Range 解析总大小
			if cr := resp.Header.Get("Content-Range"); cr != "" {
				parts := strings.Split(cr, "/")
				if len(parts) == 2 {
					if sz, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
						remoteSize = sz
						stats.ContributeSize(originalURL, remoteSize)
					}
				}
			}
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// 416，通常因为本地文件已完成或范围无效
		if localSize > 0 {
			if remoteSize >= 0 && localSize == remoteSize {
				// 文件已完整
				stats.ContributeSize(originalURL, remoteSize)
				fmt.Printf("✅ %s already completed.\n", filename)
				return nil
			}
			// 否则删除重试
			os.Remove(outputPath)
			return fmt.Errorf("range not satisfiable, restart")
		}
		return fmt.Errorf("unexpected 416 response for fresh download")
	default:
		return fmt.Errorf("unexpected HTTP status: %s", resp.Status)
	}

	// 打开文件准备写入
	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	if localSize > 0 {
		if _, err := file.Seek(localSize, io.SeekStart); err != nil {
			return fmt.Errorf("seek file: %w", err)
		}
	} else {
		if err := file.Truncate(0); err != nil {
			return fmt.Errorf("truncate file: %w", err)
		}
	}

	// 带统计的写入器
	cw := &countingWriter{
		Writer: file,
		stats:  stats,
	}

	bufSize := cfg.BufferSize
	if bufSize <= 0 {
		bufSize = 16 * 1024 * 1024 // 默认 16 MB
	}
	buf := make([]byte, bufSize)

	_, err = io.CopyBuffer(cw, resp.Body, buf)
	if err != nil {
		return fmt.Errorf("download body: %w", err)
	}

	// 校验文件大小
	if remoteSize >= 0 {
		fi, err := file.Stat()
		if err == nil && fi.Size() != remoteSize {
			return fmt.Errorf("downloaded size mismatch: got %d, expected %d", fi.Size(), remoteSize)
		}
	}

	fmt.Printf("✅ %s downloaded.\n", filename)
	return nil
}

// countingWriter 在写入数据的同时更新全局下载统计
type countingWriter struct {
	io.Writer
	stats *DownloadStats
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.Writer.Write(p)
	if n > 0 {
		cw.stats.AddDownloaded(int64(n))
	}
	return n, err
}

// 格式化为人类可读的字节大小
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// 格式化速度
func formatSpeed(bps float64) string {
	if bps < 1024 {
		return fmt.Sprintf("%.0f B/s", bps)
	} else if bps < 1024*1024 {
		return fmt.Sprintf("%.1f KB/s", bps/1024)
	}
	return fmt.Sprintf("%.1f MB/s", bps/(1024*1024))
}

// 格式化时间
func formatDuration(seconds float64) string {
	if seconds < 0 {
		return "???"
	}
	if seconds < 60 {
		return fmt.Sprintf("%.0fs", seconds)
	} else if seconds < 3600 {
		return fmt.Sprintf("%.1fm", seconds/60)
	}
	return fmt.Sprintf("%.1fh", seconds/3600)
}

// 绘制进度条（单行，通过 \r 原地刷新）
func printProgressBar(downloaded, total int64, speed float64, elapsed float64, final bool) {
	barWidth := 40
	var percent float64
	if total > 0 {
		percent = float64(downloaded) / float64(total) * 100
		if percent > 100 {
			percent = 100
		}
	} else {
		percent = 0
	}

	filled := int(percent / 100 * float64(barWidth))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

	downStr := formatBytes(downloaded)
	totalStr := "?"
	if total > 0 {
		totalStr = formatBytes(total)
	}
	sizeInfo := fmt.Sprintf("%s / %s", downStr, totalStr)

	speedStr := formatSpeed(speed)
	if speed <= 0 {
		speedStr = "0 B/s"
	}

	etaStr := "???"
	if speed > 0 && total > 0 {
		remaining := total - downloaded
		if remaining > 0 {
			etaStr = formatDuration(float64(remaining) / speed)
		} else {
			etaStr = "0s"
		}
	}
	if final {
		etaStr = "0s"
		speedStr = formatSpeed(0)
		bar = strings.Repeat("█", barWidth)
		percent = 100
	}

	line := fmt.Sprintf("\r%s %5.1f%% | %s | %s | ETA %s", bar, percent, sizeInfo, speedStr, etaStr)
	fmt.Print(line)
	if final {
		fmt.Println()
	}
}

// 进度条汇报协程
func progressReporter(ctx context.Context, stats *DownloadStats, interval time.Duration, done chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastDownloaded int64
	lastTime := time.Now()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			elapsed := now.Sub(lastTime).Seconds()
			downloaded, total := stats.Snapshot()
			speed := float64(downloaded-lastDownloaded) / elapsed
			printProgressBar(downloaded, total, speed, elapsed, false)
			lastDownloaded = downloaded
			lastTime = now
		case <-done:
			// 下载完成，输出最终进度
			downloaded, total := stats.Snapshot()
			printProgressBar(downloaded, total, 0, 0, true)
			return
		case <-ctx.Done():
			return
		}
	}
}

// 等待用户按键（按 Enter 退出）
func waitExit() {
	fmt.Print("\nPress Enter to exit...")
	bufio.NewReader(os.Stdin).ReadBytes('\n')
}

func main() {
	// 命令行参数
	configPath := flag.String("config", "config.yaml", "path to YAML configuration file")
	flag.Parse()

	// 如果配置文件不存在，则生成默认配置并退出
	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		defaultCfg := Config{
			Urls:              []string{"https://example.com/file1.zip", "https://example.com/file2.zip"},
			AcceleratePrefix:  "https://ghf.xn--eqrr82bzpe.top/",
			EnableAccelerate:  false,
			OutputDir:         "downloads",
			MaxRetries:        3,
			Proxy:             "",
			BufferSize:        16 * 1024 * 1024,
			ProgressInterval:  5,
			Concurrency:       3,
		}
		data, err := yaml.Marshal(&defaultCfg)
		if err != nil {
			fmt.Printf("Failed to marshal default config: %v\n", err)
			waitExit()
			return
		}
		if err := os.WriteFile(*configPath, data, 0o644); err != nil {
			fmt.Printf("Failed to write default config: %v\n", err)
			waitExit()
			return
		}
		fmt.Printf("Configuration file '%s' not found.\nA default one has been created. Please edit it and run again.\n", *configPath)
		waitExit()
		return
	}

	// 读取并解析 YAML 配置
	data, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Printf("Failed to read config: %v\n", err)
		waitExit()
		return
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		fmt.Printf("Failed to parse config: %v\n", err)
		waitExit()
		return
	}

	// 设置默认值
	if len(cfg.Urls) == 0 {
		fmt.Println("No URLs provided in config, nothing to do.")
		waitExit()
		return
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 3
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 16 * 1024 * 1024
	}
	if cfg.ProgressInterval <= 0 {
		cfg.ProgressInterval = 5
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = "downloads"
	}

	// 创建 HTTP 客户端
	client, err := newHTTPClient(cfg.Proxy)
	if err != nil {
		fmt.Printf("Failed to create HTTP client: %v\n", err)
		waitExit()
		return
	}

	// 打印启动信息
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("Batch Downloader (Go version)")
	fmt.Printf("Output directory: %s\n", cfg.OutputDir)
	if cfg.EnableAccelerate {
		fmt.Printf("Accelerate prefix: %s\n", cfg.AcceleratePrefix)
	} else {
		fmt.Println("Acceleration: disabled")
	}
	fmt.Printf("Max retries: %d\n", cfg.MaxRetries)
	fmt.Printf("Concurrency: %d\n", cfg.Concurrency)
	fmt.Printf("Progress interval: %ds\n", cfg.ProgressInterval)
	if cfg.Proxy != "" {
		fmt.Printf("Proxy: %s\n", cfg.Proxy)
	} else {
		fmt.Println("Proxy: none")
	}
	fmt.Println(strings.Repeat("=", 60))

	// 全局统计
	stats := &DownloadStats{}

	// 用于同步和并发控制的工具
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, cfg.Concurrency) // 令牌桶
	var successCount int32

	// 上下文，用于优雅停止（此处未实际用到取消，但保留接口）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 启动进度条协程
	doneCh := make(chan struct{})
	progressInterval := time.Duration(cfg.ProgressInterval) * time.Second
	go progressReporter(ctx, stats, progressInterval, doneCh)

	// 并发下载每个 URL
	for _, originalURL := range cfg.Urls {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()

			// 获取并发令牌
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			filename := getFilenameFromURL(url)
			for attempt := 1; attempt <= cfg.MaxRetries; attempt++ {
				err := downloadFileOnce(ctx, client, url, &cfg, stats)
				if err == nil {
					atomic.AddInt32(&successCount, 1)
					break
				}
				if attempt < cfg.MaxRetries {
					fmt.Printf("⚠️  %s attempt %d failed: %v. Retrying in 2s...\n", filename, attempt, err)
					time.Sleep(2 * time.Second)
				} else {
					fmt.Printf("❌ %s failed after %d attempts: %v\n", filename, attempt, err)
				}
			}
		}(originalURL)
	}

	// 等待所有下载任务结束
	wg.Wait()

	// 停止进度条，打印最终结果
	close(doneCh)
	// 给进度条协程一点时间输出最终行
	time.Sleep(100 * time.Millisecond)

	// 汇总结果
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("All done! Success: %d / %d\n", successCount, len(cfg.Urls))
	if int(successCount) < len(cfg.Urls) {
		fmt.Println("Some downloads failed, please check the log above.")
	} else {
		fmt.Println("All files downloaded successfully.")
	}
	fmt.Println(strings.Repeat("=", 60))

	waitExit()
}