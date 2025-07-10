package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v2"
)

// Command 定义了 YAML 文件中单个命令的结构。
// 注意：YAML 标签已修正为标准格式。
type Command struct {
	Name   string `yaml:"name"`
	Cmd    string `yaml:"cmd"`
	Output string `yaml:"output"`
}

// Config 持有一系列命令。
type Config struct {
	Commands []Command `yaml:"commands"`
}

// JobResult 保存单个命令的执行结果。
type JobResult struct {
	Command Command
	Err     error
}

func main() {
	// 1. 使用 flag 包提供更灵活的命令行配置
	configPath := flag.String("config", "command.yml", "YAML 配置文件路径。")
	domainsPath := flag.String("domains", "domains.txt", "包含域名的文件路径。")
	outputDir := flag.String("output", "results", "用于保存输出文件的目录。")
	concurrency := flag.Int("c", 10, "并发运行的命令数量。")
	clean := flag.Bool("clean", false, "运行时清理输出目录。")
	flag.Parse()

	log.SetFlags(log.Ltime) // 设置日志格式，输出时间

	// 2. 加载配置文件
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("致命错误: 加载配置文件 '%s' 失败: %v", *configPath, err)
	}

	// 3. 根据 -clean 参数准备输出目录
	if *clean {
		log.Printf("信息: 检测到 '-clean' 参数，正在删除目录: %s", *outputDir)
		if err := os.RemoveAll(*outputDir); err != nil {
			log.Fatalf("致命错误: 清理输出目录失败: %v", err)
		}
	}
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		log.Fatalf("致命错误: 创建输出目录 '%s' 失败: %v", *outputDir, err)
	}

	// 4. 设置 Worker Pool 以控制并发
	var wg sync.WaitGroup
	jobs := make(chan Command, len(cfg.Commands))
	results := make(chan JobResult, len(cfg.Commands))

	log.Printf("信息: 启动 %d 个 Worker 处理 %d 个命令。", *concurrency, len(cfg.Commands))

	for i := 1; i <= *concurrency; i++ {
		wg.Add(1)
		go worker(i, &wg, jobs, results, *domainsPath, *outputDir)
	}

	// 5. 将所有任务推送到任务管道
	for _, cmd := range cfg.Commands {
		jobs <- cmd
	}
	close(jobs)

	// 6. 等待所有 Worker 完成并关闭结果管道
	wg.Wait()
	close(results)

	// 7. 处理并报告结果
	var failedCommands []JobResult
	for result := range results {
		if result.Err != nil {
			failedCommands = append(failedCommands, result)
		}
	}

	log.Println("信息: 所有命令已执行完毕。")

	// 8. 汇总结果到单个文件
	if err := consolidateResults(*outputDir, cfg.Commands); err != nil {
		log.Printf("错误: 汇总结果失败: %v", err)
	}

	// 9. 打印最终的执行摘要
	log.Println("--- 执行摘要 ---")
	log.Printf("总命令数: %d", len(cfg.Commands))
	log.Printf("成功: %d", len(cfg.Commands)-len(failedCommands))
	log.Printf("失败: %d", len(failedCommands))
	if len(failedCommands) > 0 {
		log.Println("失败命令详情:")
		for _, failed := range failedCommands {
			log.Printf("  - 名称: %s, 错误: %v", failed.Command.Name, failed.Err)
		}
	}
	log.Println("--------------------")
}

// worker 是一个处理任务的 goroutine。
func worker(id int, wg *sync.WaitGroup, jobs <-chan Command, results chan<- JobResult, domainsPath, outputDir string) {
	defer wg.Done()
	for cmd := range jobs {
		log.Printf("WORKER %d: 开始执行 '%s'", id, cmd.Name)
		err := runCommand(cmd, domainsPath, outputDir)
		if err != nil {
			log.Printf("WORKER %d: 执行 '%s' 失败: %v", id, cmd.Name, err)
		} else {
			log.Printf("WORKER %d: 完成 '%s'", id, cmd.Name)
		}
		results <- JobResult{Command: cmd, Err: err}
	}
}

// loadConfig 读取并解析 YAML 配置文件。
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// runCommand 执行单个 shell 命令。
func runCommand(cmd Command, domainsPath, outputDir string) error {
	fullOutputPath := filepath.Join(outputDir, cmd.Output)
	cmdStr := strings.ReplaceAll(cmd.Cmd, "{domains}", domainsPath)
	cmdStr = strings.ReplaceAll(cmdStr, "{output}", fullOutputPath)

	// 使用 buffer 捕获输出，以便在出错时提供更详细的日志
	var stderrBuf bytes.Buffer
	c := exec.Command("bash", "-c", cmdStr)
	c.Stdout = os.Stdout // 实时输出到终端
	c.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

	err := c.Run()
	if err != nil {
		return fmt.Errorf("执行失败: %v\n--- 错误输出 ---\n%s", err, stderrBuf.String())
	}
	return nil
}

// consolidateResults 将所有单个输出文件合并为一个，并对所有内容进行去重。
func consolidateResults(outputDir string, commands []Command) error {
	consolidatedFilePath := filepath.Join(outputDir, "all_results.txt")
	finalFile, err := os.Create(consolidatedFilePath)
	if err != nil {
		return fmt.Errorf("无法创建汇总文件: %w", err)
	}
	defer finalFile.Close()

	log.Printf("信息: 正在汇总结果到 %s", consolidatedFilePath)

	uniqueLines := make(map[string]struct{})
	for _, cmd := range commands {
		sourceFilePath := filepath.Join(outputDir, cmd.Output)

		if _, err := os.Stat(sourceFilePath); os.IsNotExist(err) {
			log.Printf("警告: 未找到 '%s' 的结果文件 %s，已跳过。", cmd.Name, sourceFilePath)
			continue
		}

		content, err := os.ReadFile(sourceFilePath)
		if err != nil {
			log.Printf("警告: 读取文件 %s 失败: %v", sourceFilePath, err)
			continue
		}

		lines := strings.Split(string(content), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			uniqueLines[line] = struct{}{}
		}
	}

	// 输出所有唯一行到文件
	finalFile.WriteString("\n\n--- 去重后的所有结果 ---\n\n")
	for line := range uniqueLines {
		finalFile.WriteString(line + "\n")
	}

	log.Printf("信息: 已成功汇总并去重所有结果。")
	return nil
}

