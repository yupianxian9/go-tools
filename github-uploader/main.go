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

	"github.com/google/go-github/v60/github"
	"golang.org/x/oauth2"
)

type Config struct {
	GithubKey string `json:"githubkey"`
}

type ReleaseManager struct {
	Client    *github.Client
	Ctx       context.Context
	Owner     string
	Repo      string
	TagName   string
	RelName   string
	Folder    string
	Overwrite bool
}

func main() {
	defer func() {
		fmt.Println("\n===========================================")
		fmt.Println("程序运行结束，请按 Enter 键退出...")
		bufio.NewReader(os.Stdin).ReadBytes('\n')
	}()

	token, err := loadToken("github.json")
	if err != nil {
		fmt.Printf("❌ 配置文件错误: %v\n", err)
		return
	}

	manager, err := getInteractiveConfig(token)
	if err != nil {
		fmt.Printf("❌ 参数错误: %v\n", err)
		return
	}

	if err := manager.Run(); err != nil {
		fmt.Printf("❌ 任务失败: %v\n", err)
	}
}

func loadToken(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("找不到 %s", path)
	}
	var c Config
	if err := json.Unmarshal(content, &c); err != nil {
		return "", err
	}
	return c.GithubKey, nil
}

func getInteractiveConfig(token string) (*ReleaseManager, error) {
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Print("📝 仓库名 (owner/repo): ")
	scanner.Scan()
	rawRepo := strings.TrimSpace(scanner.Text())
	idx := strings.Index(rawRepo, "/")
	if idx <= 0 {
		return nil, fmt.Errorf("格式错误")
	}

	fmt.Print("📝 标签名 (v1.1.5): ")
	scanner.Scan()
	tag := strings.TrimSpace(scanner.Text())

	fmt.Print("📝 Release 标题 (回车同标签): ")
	scanner.Scan()
	title := strings.TrimSpace(scanner.Text())
	if title == "" {
		title = tag
	}

	fmt.Print("📝 文件夹路径: ")
	scanner.Scan()
	dir := strings.TrimSpace(scanner.Text())

	fmt.Print("📝 是否覆盖同名文件? (y/n): ")
	scanner.Scan()
	ovw := strings.ToLower(strings.TrimSpace(scanner.Text())) == "y"

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	client := github.NewClient(oauth2.NewClient(ctx, ts))

	return &ReleaseManager{
		Client:    client,
		Ctx:       ctx,
		Owner:     rawRepo[:idx],
		Repo:      rawRepo[idx+1:],
		TagName:   tag,
		RelName:   title,
		Folder:    dir,
		Overwrite: ovw,
	}, nil
}

func (m *ReleaseManager) Run() error {
	entries, err := os.ReadDir(m.Folder)
	if err != nil {
		return err
	}

	rel, _, err := m.Client.Repositories.GetReleaseByTag(m.Ctx, m.Owner, m.Repo, m.TagName)
	if err != nil {
		fmt.Printf("🚀 创建新 Release: %s\n", m.TagName)
		newRel := &github.RepositoryRelease{TagName: &m.TagName, Name: &m.RelName}
		rel, _, err = m.Client.Repositories.CreateRelease(m.Ctx, m.Owner, m.Repo, newRel)
		if err != nil {
			return err
		}
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileName := entry.Name()
		fullPath := filepath.Join(m.Folder, fileName)

		fmt.Printf("📦 正在处理: %s\n", fileName)

		// 改进点 1: 彻底清理重名资产
		if m.Overwrite {
			if err := m.findAndDeleteAsset(rel.GetID(), fileName); err != nil {
				fmt.Printf("  ⚠️ 清理旧文件失败: %v\n", err)
				// 继续尝试上传，可能删除成功了但由于 API 延迟返回了错误
			}
		}

		// 改进点 2: 真正的上传逻辑
		if err := m.upload(rel.GetID(), fullPath, fileName); err != nil {
			fmt.Printf("  ❌ 上传失败: %v\n", err)
		} else {
			fmt.Printf("  ✨ 上传成功\n")
		}

		m.sleep()
	}
	return nil
}

// findAndDeleteAsset 增加分页支持，确保能找到所有已存在文件
func (m *ReleaseManager) findAndDeleteAsset(releaseID int64, name string) error {
	opts := &github.ListOptions{PerPage: 100}
	for {
		assets, resp, err := m.Client.Repositories.ListReleaseAssets(m.Ctx, m.Owner, m.Repo, releaseID, opts)
		if err != nil {
			return err
		}

		for _, a := range assets {
			if a.GetName() == name {
				fmt.Printf("  🗑️  检测到同名文件，正在删除...")
				_, err := m.Client.Repositories.DeleteReleaseAsset(m.Ctx, m.Owner, m.Repo, a.GetID())
				if err != nil {
					return err
				}
				fmt.Printf(" 完成\n")
				// 改进点 3: 删除后强制等待 2 秒，给 GitHub 后端同步时间
				time.Sleep(2 * time.Second)
				return nil
			}
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return nil
}

func (m *ReleaseManager) upload(releaseID int64, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	_, _, err = m.Client.Repositories.UploadReleaseAsset(
		m.Ctx, m.Owner, m.Repo, releaseID,
		&github.UploadOptions{Name: name}, f,
	)
	return err
}

func (m *ReleaseManager) sleep() {
	s := rand.Intn(6) + 10
	fmt.Printf("  ⏳ 等待 %d 秒...\n", s)
	time.Sleep(time.Duration(s) * time.Second)
}