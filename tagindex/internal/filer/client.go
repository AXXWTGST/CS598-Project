package filer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

type ListDirectoryResponse struct {
	Entries               []DirectoryEntry
	LastFileName          string
	ShouldDisplayLoadMore bool
}

type DirectoryEntry struct {
	FullPath string
	Mode     uint32
	FileSize uint64
	Mime     string
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (c *Client) SetTags(path string, tags []string, version string) error {
	if len(tags) == 0 {
		return fmt.Errorf("at least one tag is required")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	endpoint := c.BaseURL + path + "?tagging"
	req, err := http.NewRequest(http.MethodPut, endpoint, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Seaweed-Tags", strings.Join(tags, ","))
	if version != "" {
		req.Header.Set("Seaweed-Tag-Version", version)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("set tags on %s failed: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *Client) GetTags(path string) ([]string, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	req, err := http.NewRequest(http.MethodHead, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("get tags on %s failed: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}

	return SplitTags(resp.Header.Get("Seaweed-Tags")), nil
}

func (c *Client) ClearTags(path string) error {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+path+"?tagging=Tags,Tag-Version", nil)
	if err != nil {
		return err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("clear tags on %s failed: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *Client) Download(path string) (io.ReadCloser, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	req, err := http.NewRequest(http.MethodGet, c.BaseURL+EscapePath(path), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("download %s failed: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

func (c *Client) ListFilesRecursive(root string) ([]string, error) {
	if !strings.HasPrefix(root, "/") {
		root = "/" + root
	}
	root = strings.TrimRight(root, "/")
	if root == "" {
		root = "/"
	}

	var files []string
	if err := c.walkDirectory(root, &files); err != nil {
		return nil, err
	}
	return files, nil
}

func (c *Client) walkDirectory(dir string, files *[]string) error {
	lastFileName := ""
	for {
		page, err := c.ListDirectory(dir, lastFileName)
		if err != nil {
			return err
		}

		for _, entry := range page.Entries {
			if entry.IsDirectory() {
				if err := c.walkDirectory(entry.FullPath, files); err != nil {
					return err
				}
				continue
			}
			*files = append(*files, entry.FullPath)
		}

		if !page.ShouldDisplayLoadMore {
			return nil
		}
		lastFileName = page.LastFileName
	}
}

func (c *Client) ListDirectory(dir, lastFileName string) (*ListDirectoryResponse, error) {
	if !strings.HasPrefix(dir, "/") {
		dir = "/" + dir
	}
	endpoint := c.BaseURL + EscapePath(dir) + "/?limit=1000"
	if lastFileName != "" {
		endpoint += "&lastFileName=" + url.QueryEscape(lastFileName)
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list directory %s failed: %s: %s", dir, resp.Status, strings.TrimSpace(string(body)))
	}

	var page ListDirectoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, err
	}
	return &page, nil
}

func SplitTags(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' '
	})
	var tags []string
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field != "" {
			tags = append(tags, field)
		}
	}
	return tags
}

func EscapePath(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func (e DirectoryEntry) IsDirectory() bool {
	return os.FileMode(e.Mode)&os.ModeDir != 0
}
