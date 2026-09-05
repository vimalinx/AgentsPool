package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiyuehuajin/agentspool/internal/config"
)

type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("AgentsPool API returned HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("AgentsPool API returned HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
}

type Client struct {
	BaseURL        *url.URL
	Token          string
	HTTP           *http.Client
	UserAgent      string
	DefaultTimeout time.Duration
}

func New(baseURL, token string, timeout time.Duration, insecureSkipTLSVerify bool) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse server URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("server URL must use http or https")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecureSkipTLSVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // explicitly opt-in for private lab deployments
	}
	if timeout < 0 {
		return nil, errors.New("timeout must be zero or greater")
	}
	return &Client{
		BaseURL:        parsed,
		Token:          token,
		HTTP:           &http.Client{Transport: transport},
		UserAgent:      "AgentsPool/" + config.Version,
		DefaultTimeout: timeout,
	}, nil
}

func (c *Client) withDefaultTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); hasDeadline || c.DefaultTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.DefaultTimeout)
}

func (c *Client) endpoint(apiPath string, query url.Values) string {
	copyURL := *c.BaseURL
	copyURL.Path = path.Join(c.BaseURL.Path, apiPath)
	if strings.HasSuffix(apiPath, "/") && !strings.HasSuffix(copyURL.Path, "/") {
		copyURL.Path += "/"
	}
	copyURL.RawQuery = query.Encode()
	return copyURL.String()
}

func (c *Client) DoJSON(ctx context.Context, method, apiPath string, query url.Values, body any, out any, headers map[string]string) error {
	ctx, cancel := c.withDefaultTimeout(ctx)
	defer cancel()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(apiPath, query), reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.applyHeaders(req, headers)
	response, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeAPIError(response)
	}
	if out == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<20))
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("decode API response: %w", err)
	}
	return nil
}

func (c *Client) UploadFile(ctx context.Context, apiPath string, query url.Values, filePath string, headers map[string]string, out any) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	return c.Upload(ctx, apiPath, query, file, stat.Size(), headers, out)
}

func (c *Client) Upload(ctx context.Context, apiPath string, query url.Values, reader io.Reader, size int64, headers map[string]string, out any) error {
	ctx, cancel := c.withDefaultTimeout(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(apiPath, query), reader)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	c.applyHeaders(req, headers)
	response, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeAPIError(response)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(out)
}

func (c *Client) Download(ctx context.Context, apiPath string, query url.Values, destination string, headers map[string]string) error {
	ctx, cancel := c.withDefaultTimeout(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(apiPath, query), nil)
	if err != nil {
		return err
	}
	c.applyHeaders(req, headers)
	response, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeAPIError(response)
	}

	directory := filepath.Dir(destination)
	if directory == "" {
		directory = "."
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create download directory: %w", err)
	}
	temp, err := os.CreateTemp(directory, ".agentspool-download-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temp, hash), response.Body); err != nil {
		return err
	}
	actualDigest := hex.EncodeToString(hash.Sum(nil))
	if expectedDigest := sha256FromETag(response.Header.Get("ETag")); expectedDigest != "" && !strings.EqualFold(actualDigest, expectedDigest) {
		return fmt.Errorf("artifact integrity check failed: expected sha256 %s, received %s", expectedDigest, actualDigest)
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, destination); err != nil {
		return fmt.Errorf("install downloaded file: %w", err)
	}
	ok = true
	return nil
}

func sha256FromETag(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "W/") {
		return ""
	}
	value = strings.Trim(value, "\"")
	const prefix = "sha256:"
	if !strings.HasPrefix(strings.ToLower(value), prefix) {
		return ""
	}
	digest := value[len(prefix):]
	if len(digest) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return ""
	}
	return strings.ToLower(digest)
}

func (c *Client) applyHeaders(req *http.Request, headers map[string]string) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
}

func decodeAPIError(response *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error.Message != "" {
		return &APIError{StatusCode: response.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	message := strings.TrimSpace(string(data))
	if message == "" {
		message = response.Status
	}
	return &APIError{StatusCode: response.StatusCode, Message: message}
}
