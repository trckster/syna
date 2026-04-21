package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"syna/internal/common/protocol"
)

type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (c *Client) WithToken(token string) *Client {
	cp := *c
	cp.Token = token
	return &cp
}

func (c *Client) SessionStart(ctx context.Context, req protocol.SessionStartRequest) (*protocol.SessionStartResponse, error) {
	var resp protocol.SessionStartResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/session/start", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) SessionFinish(ctx context.Context, req protocol.SessionFinishRequest) (*protocol.SessionFinishResponse, error) {
	var resp protocol.SessionFinishResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/session/finish", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) Bootstrap(ctx context.Context) (*protocol.BootstrapResponse, error) {
	var resp protocol.BootstrapResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/bootstrap", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) FetchEvents(ctx context.Context, afterSeq int64, limit int) (*protocol.EventFetchResponse, *protocol.ErrorResponse, error) {
	var resp protocol.EventFetchResponse
	var apiErr protocol.ErrorResponse
	err := c.doJSONRaw(ctx, http.MethodGet, fmt.Sprintf("/v1/events?after_seq=%d&limit=%d", afterSeq, limit), nil, &resp, &apiErr)
	if err != nil {
		return nil, &apiErr, err
	}
	return &resp, nil, nil
}

func (c *Client) SubmitEvent(ctx context.Context, req protocol.EventSubmitRequest) (*protocol.EventSubmitResponse, *protocol.ErrorResponse, error) {
	var resp protocol.EventSubmitResponse
	var apiErr protocol.ErrorResponse
	err := c.doJSONRaw(ctx, http.MethodPost, "/v1/events", req, &resp, &apiErr)
	if err != nil {
		return nil, &apiErr, err
	}
	return &resp, nil, nil
}

func (c *Client) SubmitSnapshot(ctx context.Context, req protocol.SnapshotSubmitRequest) (*protocol.SnapshotSubmitResponse, error) {
	var resp protocol.SnapshotSubmitResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/snapshots", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) UploadObject(ctx context.Context, objectID, kind string, plainSize int64, blob []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+"/v1/objects/"+objectID, bytes.NewReader(blob))
	if err != nil {
		return err
	}
	req.Header.Set(protocol.VersionHeader, "1")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Syna-Object-Kind", kind)
	req.Header.Set("X-Syna-Plain-Size", fmt.Sprintf("%d", plainSize))
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return responseError("upload object", resp)
	}
	return nil
}

func (c *Client) DownloadObject(ctx context.Context, objectID string, maxBytes int64) ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	if _, err := c.DownloadObjectTo(ctx, objectID, maxBytes, buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (c *Client) DownloadObjectTo(ctx context.Context, objectID string, maxBytes int64, dst io.Writer) (int64, error) {
	if maxBytes <= 0 {
		return 0, fmt.Errorf("invalid maximum download size %d", maxBytes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/objects/"+objectID, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set(protocol.VersionHeader, "1")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, responseError("download object", resp)
	}
	if resp.ContentLength > maxBytes {
		return 0, fmt.Errorf("object exceeds %d bytes", maxBytes)
	}
	limited := &io.LimitedReader{R: resp.Body, N: maxBytes + 1}
	n, err := io.Copy(dst, limited)
	if err != nil {
		return n, err
	}
	if n > maxBytes {
		return n, fmt.Errorf("object exceeds %d bytes", maxBytes)
	}
	return n, nil
}

func (c *Client) DialWS(ctx context.Context) (*websocket.Conn, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("unsupported websocket URL scheme %q", u.Scheme)
	}
	u.Path = "/v1/ws"
	header := http.Header{}
	header.Set(protocol.VersionHeader, "1")
	header.Set("Authorization", "Bearer "+c.Token)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), header)
	return conn, err
}

func (c *Client) doJSON(ctx context.Context, method, path string, reqBody any, respBody any) error {
	var apiErr protocol.ErrorResponse
	return c.doJSONRaw(ctx, method, path, reqBody, respBody, &apiErr)
}

func (c *Client) doJSONRaw(ctx context.Context, method, p string, reqBody any, respBody any, apiErr *protocol.ErrorResponse) error {
	var body io.Reader
	if reqBody != nil {
		buf := bytes.NewBuffer(nil)
		if err := json.NewEncoder(buf).Encode(reqBody); err != nil {
			return err
		}
		body = buf
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+p, body)
	if err != nil {
		return err
	}
	req.Header.Set(protocol.VersionHeader, "1")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		if apiErr != nil {
			_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(apiErr)
			if apiErr.Message != "" {
				return fmt.Errorf("%s", apiErr.Message)
			}
			if apiErr.Code == "" {
				return fmt.Errorf("http %d", resp.StatusCode)
			}
			return fmt.Errorf("%s", apiErr.Code)
		}
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	if respBody != nil {
		return json.NewDecoder(resp.Body).Decode(respBody)
	}
	return nil
}

func responseError(operation string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	message := strings.TrimSpace(string(body))
	if message == "" {
		return fmt.Errorf("%s: http %d", operation, resp.StatusCode)
	}
	return fmt.Errorf("%s: http %d: %s", operation, resp.StatusCode, message)
}
