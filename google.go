package main

import (
	"context"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type SuggestionWorker struct {
	mainKeyword string
	numWorkers  int
	outputFile  string
	queue       chan string
	mu          sync.Mutex
	results     map[string]struct{}
	processed   map[string]struct{}
	csvWriter   *csv.Writer
	file        *os.File
	timeout     time.Duration
	ctx         context.Context
	cancel      context.CancelFunc
}

type suggestionResponse struct {
	Suggestions []struct {
		Data string `xml:"data,attr"`
	} `xml:"CompleteSuggestion>suggestion"`
}

func NewSuggestionWorker(mainKeyword string, outputDir string, numWorkers, queueSize int, timeout time.Duration) *SuggestionWorker {
	timestamp := time.Now().Format("20060102_150405")
	outputFile := filepath.Join(outputDir, fmt.Sprintf("suggestions_%s_%s.csv", strings.ToLower(mainKeyword), timestamp))
	os.MkdirAll(outputDir, os.ModePerm)

	file, err := os.Create(outputFile)
	if err != nil {
		panic(fmt.Sprintf("创建CSV文件失败: %v", err))
	}

	csvWriter := csv.NewWriter(file)
	// 写入CSV头
	csvWriter.Write([]string{"Keyword", "Timestamp"})
	csvWriter.Flush()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	return &SuggestionWorker{
		mainKeyword: strings.ToLower(mainKeyword),
		numWorkers:  numWorkers,
		outputFile:  outputFile,
		queue:       make(chan string, queueSize),
		results:     make(map[string]struct{}),
		processed:   make(map[string]struct{}),
		csvWriter:   csvWriter,
		file:        file,
		timeout:     timeout,
		ctx:         ctx,
		cancel:      cancel,
	}
}

func (sw *SuggestionWorker) Close() {
	sw.cancel()     // 取消上下文
	close(sw.queue) // 关闭通道
	sw.csvWriter.Flush()
	sw.file.Close()
}

func (sw *SuggestionWorker) getSuggestions(query string) ([]string, error) {
	baseURL := "https://suggestqueries.google.com/complete/search"
	reqURL := fmt.Sprintf("%s?output=toolbar&hl=en&q=%s", baseURL, url.QueryEscape(query))

	// 创建带超时的请求
	ctx, cancel := context.WithTimeout(sw.ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var suggestions suggestionResponse
	if err := xml.Unmarshal(body, &suggestions); err != nil {
		return nil, fmt.Errorf("XML解析错误: %v", err)
	}

	var filtered []string
	for _, s := range suggestions.Suggestions {
		if strings.Contains(strings.ToLower(s.Data), sw.mainKeyword) {
			filtered = append(filtered, s.Data)
		}
	}
	return filtered, nil
}

func (sw *SuggestionWorker) saveSuggestion(suggestion string) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	err := sw.csvWriter.Write([]string{suggestion, timestamp})
	if err != nil {
		return fmt.Errorf("写入CSV错误: %v", err)
	}
	sw.csvWriter.Flush()
	return sw.csvWriter.Error()
}

func (sw *SuggestionWorker) worker(wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		select {
		case <-sw.ctx.Done():
			fmt.Println("任务超时或被取消")
			return
		case query, ok := <-sw.queue:
			if !ok {
				return
			}

			sw.mu.Lock()
			if _, exists := sw.processed[query]; exists {
				sw.mu.Unlock()
				continue
			}
			sw.processed[query] = struct{}{}
			sw.mu.Unlock()

			suggestions, err := sw.getSuggestions(query)
			if err != nil {
				select {
				case <-sw.ctx.Done():
					return
				default:
					fmt.Printf("获取建议时出错: %v\n", err)
					continue
				}
			}

			count := len(suggestions)
			fmt.Printf("收集到 %d 个建议关键词: %s\n", count, query)

			for _, suggestion := range suggestions {
				sw.mu.Lock()
				if _, exists := sw.results[suggestion]; !exists {
					sw.results[suggestion] = struct{}{}
					sw.mu.Unlock()

					if err := sw.saveSuggestion(suggestion); err != nil {
						fmt.Printf("保存建议时出错: %v\n", err)
					}

					select {
					case sw.queue <- suggestion:
					default:
						fmt.Printf("队列已满，跳过: %s\n", suggestion)
					}
				} else {
					sw.mu.Unlock()
				}
			}
			time.Sleep(time.Second) // 限制请求速率
		}
	}
}

func (sw *SuggestionWorker) printStats() {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	fmt.Printf("\n=== 爬虫统计 ===\n")
	fmt.Printf("已处理关键词数量: %d\n", len(sw.processed))
	fmt.Printf("已收集关键词数量: %d\n", len(sw.results))
	fmt.Printf("结果保存位置: %s\n", sw.outputFile)
	fmt.Printf("================\n")
}

func (sw *SuggestionWorker) run() {
	// 设置信号处理
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// 创建一个新的context，用于处理信号
	ctx, cancel := context.WithCancel(sw.ctx)
	defer func() {
		cancel()
		sw.Close()
		sw.printStats()
	}()

	var wg sync.WaitGroup
	// 启动workers
	for i := 0; i < sw.numWorkers; i++ {
		wg.Add(1)
		go sw.worker(&wg)
	}

	// 添加初始关键词
	sw.queue <- sw.mainKeyword

	// 等待所有worker完成或收到信号
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Printf("\n任务已超时!")
		}
	case <-sigChan:
		fmt.Printf("\n收到中断信号，正在优雅退出...")
		cancel() // 取消上下文，通知所有worker退出
	case <-done:
		fmt.Printf("\n任务已完成!")
	}
}

func main() {
	var (
		mainKeyword string
		numWorkers  int
		queueSize   int
		timeout     int
	)

	// 设置信号处理
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	fmt.Print("请输入起始关键词: ")
	fmt.Scanln(&mainKeyword)
	if mainKeyword == "" {
		fmt.Println("关键词不能为空")
		os.Exit(1)
	}

	fmt.Print("请输入并发数量（同时请求Google的最大网络数量，默认为10）: ")
	fmt.Scanln(&numWorkers)
	if numWorkers < 1 {
		numWorkers = 10
	}

	fmt.Print("请输入待爬关键词队列数量（默认1000）: ")
	fmt.Scanln(&queueSize)
	if queueSize < 1 {
		queueSize = 1000
	}

	fmt.Print("请输入任务超时时间（分钟，默认30分钟）: ")
	fmt.Scanln(&timeout)
	if timeout < 1 {
		timeout = 30
	}

	crawler := NewSuggestionWorker(mainKeyword, "results", numWorkers, queueSize, time.Duration(timeout)*time.Minute)
	fmt.Printf("爬虫已启动\n关键词：%s\n并发数量: %d\n队列数量: %d\n超时时间: %d分钟\n",
		mainKeyword, numWorkers, queueSize, timeout)

	// 在新的goroutine中处理信号
	go func() {
		<-sigChan
		fmt.Printf("\n程序正在退出，请等待当前任务完成...\n")
		// 信号处理已经在run方法中完成，这里只需要提示用户
	}()

	crawler.run()
}
