package filer

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
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
