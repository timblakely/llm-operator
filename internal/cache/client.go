/*
 * Copyright 2025.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client wraps the cache-manager HTTP Service.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New creates a cache client for the given service URL.
func New(baseURL string) *Client {
	return NewWithHTTPClient(baseURL, &http.Client{})
}

// NewWithHTTPClient creates a cache client using the supplied HTTP client.
// Sharing the transition client's timeout and transport makes cache requests
// bounded and testable alongside runtime health requests.
func NewWithHTTPClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		httpClient: httpClient,
	}
}

// CacheRequest is the body sent to /v1/ensure and /v1/sweep.
type CacheRequest struct {
	Model   string    `json:"model"`
	Backend string    `json:"backend"`
	Cache   CacheSpec `json:"cache"`
}

// CacheSpec describes an immutable model artifact for caching.
type CacheSpec struct {
	Kind                  string   `json:"kind"`
	RepoID                string   `json:"repo_id"`
	Revision              string   `json:"revision"`
	Size                  int64    `json:"size_bytes"`
	Files                 []string `json:"files,omitempty"`
	MaterializationTarget string   `json:"materialization_target,omitempty"`
}

// CacheResult is the cache location returned by the cache-manager.
type CacheResult string

const (
	CacheHot      CacheResult = "hot"
	CacheCold     CacheResult = "cold"
	CacheExternal CacheResult = "external"
)

// Ensure asks the cache-manager to materialize the artifact in the hot cache.
// Returns the cache result (hot/cold/external) from the X-LLM-Cache-Result header.
func (c *Client) Ensure(ctx context.Context, req *CacheRequest) (CacheResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal cache request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/ensure", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create ensure request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("ensure cached model %q: %w", req.Model, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("ensure cached model %q: %s: %s", req.Model, resp.Status, strings.TrimSpace(string(message)))
	}

	result := CacheResult(resp.Header.Get("X-LLM-Cache-Result"))
	return result, nil
}

// Sweep asks the cache-manager to evict old artifacts from the hot cache.
func (c *Client) Sweep(ctx context.Context, req *CacheRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal sweep request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/sweep", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create sweep request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sweep cache: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("sweep cache: %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}

	return nil
}

// Healthz checks if the cache-manager is alive.
func (c *Client) Healthz(ctx context.Context) error {
	resp, err := c.httpClient.Get(c.baseURL + "/healthz")
	if err != nil {
		return fmt.Errorf("cache-manager health check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cache-manager health check: %s", resp.Status)
	}
	return nil
}
