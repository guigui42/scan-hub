// Package paperless uploads PDFs to Paperless-ngx (API version 10, as served
// by Paperless 3.3) through a durable outbox.
package paperless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// APIVersion is pinned so response shapes don't change under us.
const APIVersion = 10

const maxJSONBytes = 4 << 20

// ErrPermanent marks an upload the server rejected for good (bad file, too
// large); retrying the same bytes won't help.
var ErrPermanent = errors.New("rejected by Paperless")

// Client is a minimal Paperless REST client. The token is never logged.
type Client struct {
	base  *url.URL
	token string
	hc    *http.Client
}

// NewClient validates the base URL (https://paperless.example.com).
func NewClient(baseURL, token string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("paperless: invalid URL %q (want https://host[:port])", baseURL)
	}
	if token == "" {
		return nil, errors.New("paperless: token is required")
	}
	u.Path = ""
	if hc == nil {
		hc = &http.Client{}
	}
	// Never forward the token to another host via a redirect.
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: u, token: token, hc: &c}, nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Request, error) {
	u := c.base.String() + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", fmt.Sprintf("application/json; version=%d", APIVersion))
	return req, nil
}

// UploadMeta are optional post_document fields.
type UploadMeta struct {
	Title string
	Tags  []int
}

// Upload posts a document and returns the consumption task UUID. On error,
// maybe=true means Paperless may have accepted it anyway (the outcome is
// unknown and must be resolved before retrying); maybe=false means it
// certainly did not.
//
// size must be the exact length of body. The request carries a
// Content-Length because Django's multipart parser reads a chunked body
// without one as empty ("No file was submitted").
func (c *Client) Upload(ctx context.Context, filename string, body io.Reader, size int64, meta UploadMeta) (taskID string, maybe bool, err error) {
	var head, tail bytes.Buffer
	mw := multipart.NewWriter(&head)
	if meta.Title != "" {
		if err := mw.WriteField("title", meta.Title); err != nil {
			return "", false, err
		}
	}
	for _, t := range meta.Tags {
		if err := mw.WriteField("tags", strconv.Itoa(t)); err != nil {
			return "", false, err
		}
	}
	if _, err := mw.CreateFormFile("document", filename); err != nil {
		return "", false, err
	}
	headLen := head.Len()
	if err := mw.Close(); err != nil {
		return "", false, err
	}
	tail.Write(head.Bytes()[headLen:])
	head.Truncate(headLen)

	content := io.MultiReader(&head, io.LimitReader(body, size), &tail)
	req, err := c.newRequest(ctx, http.MethodPost, "/api/documents/post_document/", nil, content)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(headLen) + size + int64(tail.Len())
	var wrote atomic.Bool
	req.Body = &trackingReader{r: io.NopCloser(content), read: &wrote}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", wrote.Load(), fmt.Errorf("paperless upload: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return "", true, fmt.Errorf("paperless upload response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusRequestEntityTooLarge,
		resp.StatusCode == http.StatusUnsupportedMediaType:
		return "", false, fmt.Errorf("%w: HTTP %d: %s", ErrPermanent, resp.StatusCode, snippet(b))
	case resp.StatusCode >= 500:
		// A proxy or server error may come after Paperless queued the file.
		return "", true, fmt.Errorf("paperless upload: HTTP %d: %s", resp.StatusCode, snippet(b))
	default:
		// 401/403/429...: refused before consuming; safe to retry later.
		return "", false, fmt.Errorf("paperless upload: HTTP %d: %s", resp.StatusCode, snippet(b))
	}
	if err := json.Unmarshal(b, &taskID); err != nil || taskID == "" {
		return "", true, fmt.Errorf("paperless upload: unexpected response %q", snippet(b))
	}
	return taskID, true, nil
}

// trackingReader records whether any request body byte was handed to the
// transport, i.e. whether the server might have seen the upload.
type trackingReader struct {
	r    io.ReadCloser
	read *atomic.Bool
}

func (t *trackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.read.Store(true)
	}
	return n, err
}

func (t *trackingReader) Close() error { return t.r.Close() }

// Task is the subset of a v10 task the outbox needs.
type Task struct {
	TaskID      string     `json:"task_id"`
	Status      string     `json:"status"` // pending, started, success, failure, revoked
	DateCreated time.Time  `json:"date_created"`
	InputData   InputData  `json:"input_data"`
	ResultData  ResultData `json:"result_data"`
}

// InputData of a consume task.
type InputData struct {
	Filename string `json:"filename"`
}

// ResultData of a consume task (one of the shapes is set).
type ResultData struct {
	DocumentID   int    `json:"document_id"`
	DuplicateOf  int    `json:"duplicate_of"`
	Reason       string `json:"reason"`
	ErrorMessage string `json:"error_message"`
}

// UnmarshalJSON tolerates a null or non-object result_data.
func (r *ResultData) UnmarshalJSON(b []byte) error {
	type plain ResultData
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return nil
	}
	*r = ResultData(p)
	return nil
}

// Task returns the task with this UUID; found=false if Paperless doesn't
// (yet) know it.
func (c *Client) Task(ctx context.Context, taskID string) (Task, bool, error) {
	ts, err := c.tasks(ctx, url.Values{"task_id": {taskID}})
	if err != nil {
		return Task{}, false, err
	}
	for _, t := range ts {
		if t.TaskID == taskID {
			return t, true, nil
		}
	}
	return Task{}, false, nil
}

// TasksByFilename returns consume tasks whose input filename contains name.
func (c *Client) TasksByFilename(ctx context.Context, name string) ([]Task, error) {
	return c.tasks(ctx, url.Values{"name": {name}, "task_type": {"consume_file"}, "page_size": {"100"}})
}

// tasks parses both the v10 paginated shape and a plain list (v9).
func (c *Client) tasks(ctx context.Context, q url.Values) ([]Task, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/tasks/", q, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paperless tasks: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("paperless tasks: HTTP %d: %s", resp.StatusCode, snippet(b))
	}
	// v10 is paginated ({"results": [...]}); v9 is a plain list.
	if t := bytes.TrimSpace(b); len(t) > 0 && t[0] == '{' {
		var page struct {
			Results []Task `json:"results"`
		}
		if err := json.Unmarshal(t, &page); err != nil {
			return nil, fmt.Errorf("paperless tasks: unexpected response: %w", err)
		}
		return page.Results, nil
	}
	var list []Task
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("paperless tasks: unexpected response: %w", err)
	}
	return list, nil
}

// Ping checks reachability and the token.
func (c *Client) Ping(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/tasks/", url.Values{"page_size": {"1"}}, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
