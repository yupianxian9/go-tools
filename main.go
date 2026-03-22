package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-github/v61/github"
	"golang.org/x/oauth2"
)

// readToken 从 JSON 文件中读取 GitHub Token
func readToken(jsonPath string) (string, error) {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}
	var token struct {
		GithubKey string `json:"githubkey"`
	}
	if err := json.Unmarshal(data, &token); err != nil {
		return "", fmt.Errorf("解析 JSON 失败: %w", err)
	}
	if token.GithubKey == "" {
		return "", fmt.Errorf("JSON 中未找到 githubkey 字段")
	}
	return token.GithubKey, nil
}

// promptInput 提示用户输入字符串
func promptInput(reader *bufio.Reader, prompt string) string {
	fmt.Print(prompt)
	input, _ := reader.ReadString('\n')
	return strings.TrimSpace(input)
}

// promptBool 提示用户输入布尔值（y/n），支持默认值
func promptBool(reader *bufio.Reader, prompt string, defaultVal bool) bool {
	fmt.Printf("%s (y/n, 默认 %v): ", prompt, defaultVal)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(strings.ToLower(input))
	switch input {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return defaultVal
	}
}

// getFilesFromFolder 递归获取文件夹中的所有文件路径
func getFilesFromFolder(folderPath string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(folderPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// waitWithCountdown 等待指定秒数，期间每隔15秒或最后1秒打印剩余时间
func waitWithCountdown(seconds int) {
	for i := seconds; i > 0; i-- {
		if i%15 == 0 || i <= 1 {
			fmt.Printf("    剩余等待时间: %d 秒\n", i)
		}
		time.Sleep(1 * time.Second)
	}
}

// retry 执行带重试的操作，最多重试 maxRetries 次，每次间隔递增
func retry(operation func() error, maxRetries int) error {
	var err error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		err = operation()
		if err == nil {
			return nil
		}
		if attempt < maxRetries {
			wait := time.Duration(1<<uint(attempt)) * time.Second // 指数退避: 1s, 2s, 4s, ...
			fmt.Printf("操作失败，%v 秒后重试 (%d/%d): %v\n", wait.Seconds(), attempt+1, maxRetries, err)
			time.Sleep(wait)
		}
	}
	return fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, err)
}

// getAllReleaseAssets 分页获取 release 的所有 assets
func getAllReleaseAssets(ctx context.Context, client *github.Client, owner, repo string, releaseID int64) ([]*github.ReleaseAsset, error) {
	var allAssets []*github.ReleaseAsset
	opts := &github.ListOptions{PerPage: 100}
	for {
		assets, resp, err := client.Repositories.ListReleaseAssets(ctx, owner, repo, releaseID, opts)
		if err != nil {
			return nil, err
		}
		allAssets = append(allAssets, assets...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return allAssets, nil
}

// ensureAssetDeleted 确保指定的 asset 已被删除，最多等待 maxWait 秒
func ensureAssetDeleted(ctx context.Context, client *github.Client, owner, repo string, releaseID int64, assetName string, maxWait int) error {
	timeout := time.After(time.Duration(maxWait) * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("等待删除超时，asset %s 可能仍然存在", assetName)
		case <-ticker.C:
			assets, err := getAllReleaseAssets(ctx, client, owner, repo, releaseID)
			if err != nil {
				return err
			}
			found := false
			for _, a := range assets {
				if a.GetName() == assetName {
					found = true
					break
				}
			}
			if !found {
				return nil // 已删除
			}
			fmt.Printf("  等待删除完成...\n")
		}
	}
}

// uploadWithRetry 带智能重试的上传函数（处理 422 already_exists）
func uploadWithRetry(ctx context.Context, client *github.Client, owner, repo string, releaseID int64, localPath, fileName string, uploadOpts *github.UploadOptions) error {
	maxRetries := 3
	for attempt := 0; attempt <= maxRetries; attempt++ {
		// 每次尝试都重新打开文件
		f, openErr := os.Open(localPath)
		if openErr != nil {
			return fmt.Errorf("打开文件失败: %w", openErr)
		}
		uploadErr := func() error {
			defer f.Close()
			_, _, err := client.Repositories.UploadReleaseAsset(ctx, owner, repo, releaseID, uploadOpts, f)
			return err
		}()

		if uploadErr == nil {
			return nil
		}

		// 如果是 already_exists，尝试重新删除
		if strings.Contains(uploadErr.Error(), "already_exists") {
			fmt.Printf("  上传时发现 asset 仍存在，尝试重新删除...\n")
			// 重新获取 assets，找到同名并删除
			assets, listErr := getAllReleaseAssets(ctx, client, owner, repo, releaseID)
			if listErr != nil {
				return fmt.Errorf("重新获取 assets 列表失败: %w", listErr)
			}
			for _, a := range assets {
				if a.GetName() == fileName {
					_, delErr := client.Repositories.DeleteReleaseAsset(ctx, owner, repo, a.GetID())
					if delErr != nil {
						fmt.Printf("  重新删除失败: %v\n", delErr)
					} else {
						// 等待删除生效
						if err := ensureAssetDeleted(ctx, client, owner, repo, releaseID, fileName, 5); err != nil {
							fmt.Printf("  等待删除确认失败: %v\n", err)
						}
					}
					break
				}
			}
			// 重试继续
			continue
		}

		// 其他错误，普通重试
		if attempt < maxRetries {
			wait := time.Duration(1<<uint(attempt)) * time.Second
			fmt.Printf("  上传失败，%v 秒后重试: %v\n", wait.Seconds(), uploadErr)
			time.Sleep(wait)
		} else {
			return fmt.Errorf("上传失败，已达最大重试次数: %w", uploadErr)
		}
	}
	return fmt.Errorf("上传失败，已达最大重试次数")
}

// uploadToRelease 执行文件上传到 GitHub Release
func uploadToRelease(
	ctx context.Context,
	client *github.Client,
	repoName, tagName, releaseName string,
	filePaths []string,
	overwrite, draft, prerelease bool,
) error {
	// 解析仓库名
	parts := strings.Split(repoName, "/")
	if len(parts) != 2 {
		return fmt.Errorf("仓库名格式错误，应为 owner/repo")
	}
	owner, repo := parts[0], parts[1]

	// 获取或创建 Release
	release, resp, err := client.Repositories.GetReleaseByTag(ctx, owner, repo, tagName)
	if err != nil && resp != nil && resp.StatusCode == 404 {
		// Release 不存在，创建新的
		fmt.Printf("创建新 Release: %s\n", tagName)
		releaseNameVal := releaseName
		if releaseNameVal == "" {
			releaseNameVal = tagName
		}
		release, _, err = client.Repositories.CreateRelease(ctx, owner, repo, &github.RepositoryRelease{
			TagName:    github.String(tagName),
			Name:       github.String(releaseNameVal),
			Body:       github.String(releaseNameVal),
			Draft:      github.Bool(draft),
			Prerelease: github.Bool(prerelease),
		})
		if err != nil {
			return fmt.Errorf("创建 Release 失败: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("获取 Release 失败: %w", err)
	} else {
		fmt.Printf("找到已有 Release: %s\n", tagName)
	}

	totalFiles := len(filePaths)
	fmt.Printf("找到 %d 个文件准备上传\n", totalFiles)

	uploadedCount := 0
	skippedCount := 0
	failedFiles := []string{}
	startTime := time.Now()

	for idx, localPath := range filePaths {
		fileName := filepath.Base(localPath)
		currentTime := time.Now().Format("15:04:05")
		fmt.Printf("\n[%s] 处理文件 %d/%d: %s\n", currentTime, idx+1, totalFiles, fileName)

		// 分页获取当前 Release 的所有 assets
		assets, err := getAllReleaseAssets(ctx, client, owner, repo, release.GetID())
		if err != nil {
			fmt.Printf("  获取 assets 列表失败，跳过该文件: %v\n", err)
			failedFiles = append(failedFiles, fileName)
			continue
		}

		// 检查是否存在同名文件
		var existingAsset *github.ReleaseAsset
		for _, asset := range assets {
			if asset.GetName() == fileName {
				existingAsset = asset
				break
			}
		}

		if existingAsset != nil {
			if overwrite {
				fmt.Printf("  删除已存在的文件: %s\n", fileName)
				err := retry(func() error {
					_, err := client.Repositories.DeleteReleaseAsset(ctx, owner, repo, existingAsset.GetID())
					return err
				}, 3)
				if err != nil {
					fmt.Printf("  删除失败: %v\n", err)
					failedFiles = append(failedFiles, fileName)
					continue
				}
				// 等待并确认删除
				if err := ensureAssetDeleted(ctx, client, owner, repo, release.GetID(), fileName, 10); err != nil {
					fmt.Printf("  等待删除确认失败: %v\n", err)
					failedFiles = append(failedFiles, fileName)
					continue
				}
			} else {
				fmt.Printf("  跳过已存在文件: %s\n", fileName)
				skippedCount++
				continue
			}
		}

		// 上传 asset，带智能重试
		uploadOpts := &github.UploadOptions{
			Name:  fileName,
			Label: fileName,
		}
		err = uploadWithRetry(ctx, client, owner, repo, release.GetID(), localPath, fileName, uploadOpts)
		if err != nil {
			fmt.Printf("  上传失败: %v\n", err)
			failedFiles = append(failedFiles, fileName)
			continue
		}
		fmt.Printf("  成功上传: %s\n", fileName)
		uploadedCount++

		// 如果不是最后一个文件，随机等待 10~15 秒（带倒计时）
		if idx+1 < totalFiles {
			waitTime := rand.Intn(6) + 10 // 10-15 秒
			fmt.Printf("  等待 %d 秒后继续...\n", waitTime)
			waitWithCountdown(waitTime)
			fmt.Println("  继续上传下一个文件")
		}
	}

	elapsed := time.Since(startTime)
	fmt.Printf("\n上传完成!\n")
	fmt.Printf("总文件数: %d\n", totalFiles)
	fmt.Printf("成功上传: %d\n", uploadedCount)
	fmt.Printf("跳过文件: %d\n", skippedCount)
	fmt.Printf("失败文件: %d\n", len(failedFiles))
	if len(failedFiles) > 0 {
		fmt.Printf("失败文件列表: %s\n", strings.Join(failedFiles, ", "))
	}
	fmt.Printf("总耗时: %.2f 秒 (%.2f 分钟)\n", elapsed.Seconds(), elapsed.Minutes())

	if len(failedFiles) > 0 {
		return fmt.Errorf("部分文件上传失败")
	}
	return nil
}

// waitForExit 等待用户按键后退出
func waitForExit() {
	fmt.Print("按 Enter 键退出...")
	fmt.Scanln()
}

func main() {
	// 初始化随机数种子（Go 1.20+ 会自动初始化，但保留兼容）
	rand.Seed(time.Now().UnixNano())

	// 读取 GitHub Token
	token, err := readToken("github.json")
	if err != nil {
		fmt.Printf("读取 Token 失败: %v\n", err)
		waitForExit()
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)

	// 交互式获取配置
	repoName := promptInput(reader, "请输入仓库名 (格式 owner/repo): ")
	if repoName == "" {
		fmt.Println("仓库名不能为空")
		waitForExit()
		os.Exit(1)
	}

	tagName := promptInput(reader, "请输入标签名 (如 v1.0.0): ")
	if tagName == "" {
		fmt.Println("标签名不能为空")
		waitForExit()
		os.Exit(1)
	}

	releaseName := promptInput(reader, "请输入 Release 显示名称 (留空则使用标签名): ")

	// 选择上传方式：文件夹或文件列表
	fmt.Print("上传方式: 文件夹 (f) 或文件列表 (l) [f/l]: ")
	mode, _ := reader.ReadString('\n')
	mode = strings.TrimSpace(strings.ToLower(mode))

	var filePaths []string
	if mode == "l" {
		filesInput := promptInput(reader, "请输入文件路径列表，用逗号分隔: ")
		files := strings.Split(filesInput, ",")
		for _, f := range files {
			f = strings.TrimSpace(f)
			if f != "" {
				if _, err := os.Stat(f); os.IsNotExist(err) {
					fmt.Printf("警告: 文件 %s 不存在，跳过\n", f)
					continue
				}
				filePaths = append(filePaths, f)
			}
		}
		if len(filePaths) == 0 {
			fmt.Println("没有有效的文件")
			waitForExit()
			os.Exit(1)
		}
	} else {
		folderPath := promptInput(reader, "请输入文件夹路径: ")
		if folderPath == "" {
			fmt.Println("文件夹路径不能为空")
			waitForExit()
			os.Exit(1)
		}
		files, err := getFilesFromFolder(folderPath)
		if err != nil {
			fmt.Printf("读取文件夹失败: %v\n", err)
			waitForExit()
			os.Exit(1)
		}
		if len(files) == 0 {
			fmt.Println("文件夹中没有文件")
			waitForExit()
			os.Exit(1)
		}
		filePaths = files
	}

	overwrite := promptBool(reader, "是否覆盖已存在的文件", true)
	draft := promptBool(reader, "是否作为草稿发布", false)
	prerelease := promptBool(reader, "是否标记为预发布", false)

	// 创建 GitHub 客户端
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	// 执行上传
	err = uploadToRelease(ctx, client, repoName, tagName, releaseName, filePaths, overwrite, draft, prerelease)
	if err != nil {
		fmt.Printf("操作出现错误: %v\n", err)
	}
	waitForExit()
}