// Package octopus is a small read-only client for the Octopus Energy REST API.
package octopus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the public Octopus Energy REST API.
const DefaultBaseURL = "https://api.octopus.energy/v1/"

// ErrNotFound is returned (wrapped) when the API answers 404.
var ErrNotFound = errors.New("not found")

// Client performs authenticated GET requests. It never issues anything but GET.
type Client struct {
	base    *url.URL
	key     string
	hc      *http.Client
	retries int
	backoff time.Duration
	sleep   func(context.Context, time.Duration) error
}

// NewClient builds a client. The API key is sent as the basic-auth username
// with an empty password, and is never included in errors or logs.
func NewClient(baseURL, apiKey string, hc *http.Client) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("empty API key")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid base URL %q", baseURL)
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{base: u, key: apiKey, hc: hc, retries: 5, backoff: time.Second, sleep: sleepCtx}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// resolve turns a relative path into a full URL, and only accepts absolute
// URLs (pagination "next" links) that stay on the API host, so credentials
// can never be sent elsewhere.
func (c *Client) resolve(ref string) (*url.URL, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return nil, err
	}
	if u.IsAbs() {
		if u.Scheme != c.base.Scheme || u.Host != c.base.Host {
			return nil, fmt.Errorf("refusing to follow link to unexpected host %q", u.Host)
		}
		return u, nil
	}
	rel := &url.URL{Path: strings.TrimLeft(u.Path, "/"), RawQuery: u.RawQuery}
	return c.base.ResolveReference(rel), nil
}

func (c *Client) get(ctx context.Context, ref string, out any) error {
	u, err := c.resolve(ref)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(c.key, "")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "octo-mcp")

		wait := c.backoffFor(attempt)
		resp, err := c.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("request to %s failed: %w", u.Path, errors.Unwrap(err))
		} else {
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
			resp.Body.Close()
			switch {
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				if rerr != nil {
					return fmt.Errorf("reading response from %s: %w", u.Path, rerr)
				}
				if err := json.Unmarshal(body, out); err != nil {
					return fmt.Errorf("decoding response from %s: %w", u.Path, err)
				}
				return nil
			case resp.StatusCode == http.StatusNotFound:
				return fmt.Errorf("%w: %s", ErrNotFound, u.Path)
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
				lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, u.Path)
				if ra := retryAfter(resp); ra > 0 {
					wait = ra
				}
			default:
				return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, u.Path, snippet(body))
			}
		}
		if attempt >= c.retries {
			return fmt.Errorf("giving up after %d attempts: %w", attempt+1, lastErr)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (c *Client) backoffFor(attempt int) time.Duration {
	d := c.backoff << attempt
	if d > 30*time.Second || d <= 0 {
		d = 30 * time.Second
	}
	return d
}

func retryAfter(resp *http.Response) time.Duration {
	s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After")))
	if err != nil || s < 0 {
		return 0
	}
	d := time.Duration(s) * time.Second
	if d > time.Minute {
		d = time.Minute
	}
	return d
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

type page[T any] struct {
	Count   int    `json:"count"`
	Next    string `json:"next"`
	Results []T    `json:"results"`
}

const maxPages = 1000

// getAll follows the "next" links of a paginated endpoint.
func getAll[T any](ctx context.Context, c *Client, path string, q url.Values) ([]T, error) {
	ref := path
	if len(q) > 0 {
		ref += "?" + q.Encode()
	}
	var all []T
	for i := 0; ref != ""; i++ {
		if i >= maxPages {
			return nil, fmt.Errorf("too many pages fetching %s", path)
		}
		var p page[T]
		if err := c.get(ctx, ref, &p); err != nil {
			return nil, err
		}
		all = append(all, p.Results...)
		ref = p.Next
	}
	return all, nil
}
