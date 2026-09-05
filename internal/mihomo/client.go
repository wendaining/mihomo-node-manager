package mihomo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Group struct {
	Name  string   `json:"name"`
	Type  string   `json:"type"`
	Now   string   `json:"now"`
	Fixed string   `json:"fixed"`
	Alive bool     `json:"alive"`
	All   []string `json:"all"`
}

type Version struct {
	Meta    bool   `json:"meta"`
	Version string `json:"version"`
}

type Client struct {
	baseURL string
	secret  string
	http    *http.Client
}

func NewClient(baseURL, secret string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		secret:  strings.TrimSpace(secret),
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	err := c.doJSON(ctx, http.MethodGet, "/version", nil, nil, &out)
	return out, err
}

func (c *Client) Group(ctx context.Context, group string) (Group, error) {
	var out Group
	err := c.doJSON(ctx, http.MethodGet, "/proxies/"+url.PathEscape(group), nil, nil, &out)
	if err != nil {
		return Group{}, err
	}
	if len(out.All) == 0 {
		return Group{}, fmt.Errorf("%q is not a selectable policy group", group)
	}
	return out, nil
}

func (c *Client) Providers(ctx context.Context) (map[string]string, error) {
	var response struct {
		Providers map[string]struct {
			Proxies []struct {
				Name string `json:"name"`
			} `json:"proxies"`
		} `json:"providers"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/providers/proxies", nil, nil, &response); err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for providerName, provider := range response.Providers {
		for _, proxy := range provider.Proxies {
			if proxy.Name == "" {
				continue
			}
			if previous, exists := result[proxy.Name]; exists && previous != providerName {
				return nil, fmt.Errorf("proxy %q appears in providers %q and %q", proxy.Name, previous, providerName)
			}
			result[proxy.Name] = providerName
		}
	}
	return result, nil
}

func (c *Client) Probe(ctx context.Context, provider, node, probeURL, expected string, timeout time.Duration) (int, error) {
	query := url.Values{}
	query.Set("url", probeURL)
	query.Set("timeout", fmt.Sprintf("%d", timeout.Milliseconds()))
	if expected != "" {
		query.Set("expected", expected)
	}
	var out struct {
		Delay int `json:"delay"`
	}
	path := "/proxies/" + url.PathEscape(node) + "/delay"
	if provider != "" {
		path = "/providers/proxies/" + url.PathEscape(provider) + "/" + url.PathEscape(node) + "/healthcheck"
	}
	err := c.doJSON(ctx, http.MethodGet, path, query, nil, &out)
	if err != nil {
		return 0, err
	}
	if out.Delay <= 0 {
		return 0, errors.New("probe returned a non-positive delay")
	}
	return out.Delay, nil
}

func (c *Client) Select(ctx context.Context, group, node string) (Group, error) {
	body := struct {
		Name string `json:"name"`
	}{Name: node}
	if err := c.doJSON(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), nil, body, nil); err != nil {
		return Group{}, err
	}
	actual, err := c.Group(ctx, group)
	if err != nil {
		return Group{}, fmt.Errorf("verify selection: %w", err)
	}
	if actual.Now != node {
		return Group{}, fmt.Errorf("selection verification failed: now=%q want=%q", actual.Now, node)
	}
	if (actual.Type == "URLTest" || actual.Type == "Fallback") && actual.Fixed != node {
		return Group{}, fmt.Errorf("selection verification failed: fixed=%q want=%q", actual.Fixed, node)
	}
	return actual, nil
}

func (c *Client) ClearFixed(ctx context.Context, group string) error {
	return c.doJSON(ctx, http.MethodDelete, "/proxies/"+url.PathEscape(group), nil, nil, nil)
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if len(query) > 0 {
		req.URL.RawQuery = query.Encode()
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("mihomo API %s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(limited)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode Mihomo response: %w", err)
	}
	return nil
}
