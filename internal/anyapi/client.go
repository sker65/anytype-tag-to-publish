// Package anyapi is the only place that talks to anytype-cli. It uses the
// local HTTP API v2 (port 31012) with a normal API key: reads, the property
// write-back, file downloads and the SSE search stream.
package anyapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotAllowed = errors.New("anyapi: property write not allowed")
	ErrTooLarge   = errors.New("anyapi: file exceeds size limit")
)

// APIError is the v2 error envelope {status, code, message}.
type APIError struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anytype api: %d %s: %s", e.Status, e.Code, e.Message)
}

// IsValidation reports a 400 validation_failed, e.g. a refused search filter.
func IsValidation(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusBadRequest
}

func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

type Client struct {
	base string
	key  string
	http *http.Client

	mu      sync.RWMutex
	allowed map[string]bool
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		key:  apiKey,
		// no global timeout: the stream is long-lived; unary calls use ctx deadlines
		http:    &http.Client{},
		allowed: map[string]bool{},
	}
}

// AllowWrites sets the property keys PatchProperties may touch. Every other
// key is rejected before a request is sent.
func (c *Client) AllowWrites(keys ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.allowed = map[string]bool{}
	for _, k := range keys {
		if k != "" {
			c.allowed[k] = true
		}
	}
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body any) (*http.Request, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Idempotency-Key", randomKey())
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := c.newRequest(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	// search is a read even though it is a POST
	if strings.HasSuffix(path, "/search") {
		req.Header.Del("Idempotency-Key")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return readError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func readError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	ae := &APIError{Status: resp.StatusCode}
	if json.Unmarshal(b, ae) != nil || ae.Message == "" {
		ae.Message = strings.TrimSpace(string(b))
	}
	ae.Status = resp.StatusCode
	return ae
}

// WhoAmI returns the raw whoami response; used as a startup connectivity check.
func (c *Client) WhoAmI(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/v2/auth/whoami", nil, nil, &out)
	return out, err
}

// Member is a member row; Me returns the bot's own participant in a space.
type Member struct {
	ID       string `json:"id"`
	Identity string `json:"identity"`
	Name     string `json:"name"`
	Role     string `json:"role"`
}

func (c *Client) Me(ctx context.Context, spaceID string) (Member, error) {
	var m Member
	err := c.do(ctx, http.MethodGet, "/v2/spaces/"+url.PathEscape(spaceID)+"/members/me", nil, nil, &m)
	return m, err
}

// Is reports whether a participant reference as served in created_by /
// last_modified_by ("participant-<identity>#Name") names this member.
func (m Member) Is(ref string) bool {
	ref, _, _ = strings.Cut(ref, "#")
	ref = strings.TrimSpace(strings.Trim(ref, `[]"`))
	if ref == "" {
		return false
	}
	if ref == m.ID {
		return true
	}
	return m.Identity != "" && (ref == "participant-"+m.Identity || strings.HasSuffix(ref, "_"+m.Identity))
}

// IsClientError reports a 4xx answer: retrying the same request will not help.
func IsClientError(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests
}

type listResponse[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}

func (c *Client) ListProperties(ctx context.Context, spaceID string) ([]Property, error) {
	var all []Property
	for offset := 0; ; {
		var page listResponse[Property]
		q := url.Values{"limit": {"1000"}, "offset": {fmt.Sprint(offset)}}
		if err := c.do(ctx, http.MethodGet, "/v2/spaces/"+url.PathEscape(spaceID)+"/properties", q, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		if !page.HasMore || len(page.Data) == 0 {
			return all, nil
		}
		offset += len(page.Data)
	}
}

// OptionExists reports whether a select property already has an option with
// this name (case-insensitive). The v2 API refuses a filter that names a
// missing option, and only creates options as a side effect of object writes.
func (c *Client) OptionExists(ctx context.Context, spaceID, key, name string) (bool, error) {
	for offset := 0; ; {
		var page listResponse[struct {
			Name string `json:"name"`
		}]
		q := url.Values{"prefix": {name}, "limit": {"100"}, "offset": {fmt.Sprint(offset)}}
		if err := c.do(ctx, http.MethodGet, "/v2/spaces/"+url.PathEscape(spaceID)+"/properties/"+url.PathEscape(key)+"/options", q, nil, &page); err != nil {
			return false, err
		}
		for _, o := range page.Data {
			if strings.EqualFold(o.Name, name) {
				return true, nil
			}
		}
		if !page.HasMore || len(page.Data) == 0 {
			return false, nil
		}
		offset += len(page.Data)
	}
}

func (c *Client) CreateProperty(ctx context.Context, spaceID, name, format string) (Property, error) {
	var out Property
	body := map[string]any{"name": name, "format": format}
	err := c.do(ctx, http.MethodPost, "/v2/spaces/"+url.PathEscape(spaceID)+"/properties", nil, body, &out)
	if out.Name == "" {
		out.Name = name
	}
	return out, err
}

// GetObject reads an object as an AnyBlock document with full ids.
func (c *Client) GetObject(ctx context.Context, spaceID, objectID string) (*Document, error) {
	var doc Document
	q := url.Values{"ids": {"full"}}
	err := c.do(ctx, http.MethodGet, "/v2/spaces/"+url.PathEscape(spaceID)+"/objects/"+url.PathEscape(objectID), q, nil, &doc)
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// PatchProperties sets and unsets properties through the set_properties op.
// Only keys passed to AllowWrites are accepted.
func (c *Client) PatchProperties(ctx context.Context, spaceID, objectID string, set map[string]any, unset []string) error {
	c.mu.RLock()
	for k := range set {
		if !c.allowed[k] {
			c.mu.RUnlock()
			return fmt.Errorf("%w: %q", ErrNotAllowed, k)
		}
	}
	for _, k := range unset {
		if !c.allowed[k] {
			c.mu.RUnlock()
			return fmt.Errorf("%w: %q", ErrNotAllowed, k)
		}
	}
	c.mu.RUnlock()
	if len(set) == 0 && len(unset) == 0 {
		return nil
	}
	op := map[string]any{"op": "set_properties"}
	if len(set) > 0 {
		op["set"] = set
	}
	if len(unset) > 0 {
		op["unset"] = unset
	}
	body := map[string]any{"ops": []any{op}}
	return c.do(ctx, http.MethodPatch, "/v2/spaces/"+url.PathEscape(spaceID)+"/objects/"+url.PathEscape(objectID), nil, body, nil)
}

// DownloadFile streams the content of a file object into w, refusing more than max bytes.
func (c *Client) DownloadFile(ctx context.Context, spaceID, fileID string, max int64, w io.Writer) (contentType string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, "/v2/spaces/"+url.PathEscape(spaceID)+"/files/"+url.PathEscape(fileID)+"/content", nil, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", readError(resp)
	}
	if max <= 0 {
		max = 1 << 62
	}
	if resp.ContentLength > max {
		return "", ErrTooLarge
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return "", err
	}
	if n > max {
		return "", ErrTooLarge
	}
	return resp.Header.Get("Content-Type"), nil
}

func randomKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
