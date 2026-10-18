// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/instance"
)

// Pushed is a server's answer to an upload.
type Pushed struct {
	ID string `json:"id"`
	// Path is where the instance can be downloaded, on the server.
	Path string `json:"path"`
	// URL is Path on the store's address, filled in by the client.
	URL string `json:"url,omitempty"`
	// Created is false when the store had the instance already.
	Created bool `json:"created"`
}

// Client talks to a store server.
type Client struct {
	// URL is the server's address, such as http://127.0.0.1:7979.
	URL   string
	Token string
	// HTTP is the client to use; nil means one with a 10-minute timeout.
	HTTP *http.Client
	// MaxBytes limits what the client reads from the store; zero means
	// DefaultMaxBytes. Set it to the store's -max-size to pull bigger files.
	MaxBytes int64
}

func (c *Client) http() *http.Client {
	hc := &http.Client{Timeout: 10 * time.Minute}
	if c.HTTP != nil {
		copied := *c.HTTP
		hc = &copied
	}
	// A redirect could carry the token somewhere else, or from https to
	// http, so none are followed.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("the store answered with a redirect; give its final address")
	}
	return hc
}

func (c *Client) endpoint(path string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(c.URL, "/"))
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return "", errors.New("the store address isn't an http or https URL")
	}
	return u.String() + path, nil
}

func (c *Client) do(method, path string, body []byte) ([]byte, int, error) {
	target, err := c.endpoint(path)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	max := c.MaxBytes
	if max <= 0 {
		max = DefaultMaxBytes
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if int64(len(data)) > max {
		return nil, resp.StatusCode, fmt.Errorf("the store's answer is over %d bytes (pull -max-size raises the limit)", max)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, resp.StatusCode, fmt.Errorf("the store said %d: %s", resp.StatusCode, codeinstances.SafeText(e.Error))
		}
		return nil, resp.StatusCode, fmt.Errorf("the store said %d", resp.StatusCode)
	}
	return data, resp.StatusCode, nil
}

// Push uploads an instance file's bytes.
func (c *Client) Push(data []byte) (*Pushed, error) {
	inst, err := instance.Parse(data)
	if err != nil {
		return nil, err
	}
	out, _, err := c.do(http.MethodPost, "/v1/instances", data)
	if err != nil {
		return nil, err
	}
	var p Pushed
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("the store's answer: %w", err)
	}
	if p.ID != inst.ID {
		return nil, errors.New("the store answered with another id")
	}
	p.Path = "/v1/instances/" + inst.ID
	p.URL, _ = c.endpoint(p.Path)
	return &p, nil
}

// Pull downloads an instance by id or unique prefix, and checks that it is
// intact and is the one asked for.
func (c *Client) Pull(ref string) ([]byte, *instance.Instance, error) {
	hex, err := cleanRef(ref)
	if err != nil {
		return nil, nil, err
	}
	data, _, err := c.do(http.MethodGet, "/v1/instances/"+hex, nil)
	if err != nil {
		return nil, nil, err
	}
	inst, err := instance.Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("the store sent a file that isn't intact: %w", err)
	}
	if !strings.HasPrefix(strings.TrimPrefix(inst.ID, "sha256:"), hex) {
		return nil, nil, fmt.Errorf("the store sent %s, not %s", inst.ID, ref)
	}
	return data, inst, nil
}

// Replay asks the store to replay an instance it keeps, by id or unique
// prefix, and returns how the run went.
func (c *Client) Replay(ref string, req ReplayRequest) (*RunResult, error) {
	return c.runOn(ref, "replay", req, false)
}

// Fork asks the store to fork an instance it keeps offline at a step, and
// returns how the fork ran and the id the store keeps it under.
func (c *Client) Fork(ref string, req ForkRequest) (*RunResult, error) {
	return c.runOn(ref, "fork", req, req.Keep)
}

var fullID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (c *Client) runOn(ref, kind string, req any, keep bool) (*RunResult, error) {
	hex, err := cleanRef(ref)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	out, _, err := c.do(http.MethodPost, "/v1/instances/"+hex+"/"+kind, body)
	if err != nil {
		return nil, err
	}
	var res RunResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("the store's answer: %w", err)
	}
	if !strings.HasPrefix(strings.TrimPrefix(res.Instance, "sha256:"), hex) || res.Kind != kind {
		return nil, errors.New("the store answered about another run")
	}
	if keep && !fullID.MatchString(res.Fork) || !keep && res.Fork != "" {
		return nil, errors.New("the store's answer doesn't say where it kept the fork, or says it kept one it wasn't asked to")
	}
	return &res, nil
}
