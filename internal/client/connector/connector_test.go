package connector

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"syna/internal/common/protocol"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestDialWSRejectsUnsupportedScheme(t *testing.T) {
	client := New("ftp://example.test")
	_, err := client.DialWS(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unsupported websocket URL scheme") {
		t.Fatalf("expected unsupported scheme error, got %v", err)
	}
}

func TestJSONErrorFallsBackToHTTPStatus(t *testing.T) {
	client := &Client{
		BaseURL: "https://example.test",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(strings.NewReader("not json")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
	}

	err := client.doJSON(context.Background(), http.MethodGet, "/v1/bootstrap", nil, nil)
	if err == nil || err.Error() != "http 502" {
		t.Fatalf("error = %v want http 502", err)
	}
}

func TestJSONErrorPreservesStatusAndCode(t *testing.T) {
	client := &Client{
		BaseURL: "https://example.test",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader(`{"code":"workspace_not_found","message":"workspace is gone"}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
	}

	err := client.doJSON(context.Background(), http.MethodGet, "/v1/bootstrap", nil, nil)
	if !IsHTTPError(err, http.StatusNotFound, "workspace_not_found") {
		t.Fatalf("error = %#v, expected typed workspace_not_found", err)
	}
	if err.Error() != "workspace is gone" {
		t.Fatalf("error text = %q", err.Error())
	}
}

func TestResponseErrorPreservesOperationForEmptyBody(t *testing.T) {
	err := responseError("open websocket", &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader("")),
	})
	if err.Error() != "open websocket: http 401" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestAuthenticatedHTTP401NotifiesSessionOwner(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Client) error
	}{
		{name: "json", run: func(client *Client) error {
			_, err := client.Bootstrap(context.Background())
			return err
		}},
		{name: "upload", run: func(client *Client) error {
			return client.UploadObject(context.Background(), "object-1", "file_chunk", 1, []byte("x"))
		}},
		{name: "download", run: func(client *Client) error {
			_, err := client.DownloadObject(context.Background(), "object-1", 16)
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var rejectedToken string
			client := (&Client{
				BaseURL: "https://example.test",
				Token:   "expired-token",
				HTTPClient: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusUnauthorized,
						Body:       io.NopCloser(strings.NewReader(`{"code":"unauthorized"}`)),
						Header:     make(http.Header),
						Request:    req,
					}, nil
				})},
			}).WithUnauthorizedHandler(func(token string) {
				rejectedToken = token
			})

			if err := tc.run(client); !IsHTTPError(err, http.StatusUnauthorized, "unauthorized") {
				t.Fatalf("error = %#v, expected typed unauthorized response", err)
			}
			if rejectedToken != "expired-token" {
				t.Fatalf("rejected token = %q want expired-token", rejectedToken)
			}
		})
	}
}

func TestAuthenticatedWebSocket401NotifiesSessionOwner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized"}`))
	}))
	defer server.Close()

	var rejectedToken string
	client := New(server.URL).
		WithToken("expired-token").
		WithUnauthorizedHandler(func(token string) {
			rejectedToken = token
		})
	if _, err := client.DialWS(context.Background()); !IsHTTPError(err, http.StatusUnauthorized, "unauthorized") {
		t.Fatalf("DialWS error = %#v, expected typed unauthorized response", err)
	}
	if rejectedToken != "expired-token" {
		t.Fatalf("rejected token = %q want expired-token", rejectedToken)
	}
}

func TestDownloadObjectToRejectsOversizedBody(t *testing.T) {
	body := []byte("abcdef")
	client := &Client{
		BaseURL: "https://example.test",
		Token:   "token",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get(protocol.VersionHeader); got != "1" {
				t.Fatalf("protocol header = %q want 1", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(body)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
	}

	var dst bytes.Buffer
	n, err := client.DownloadObjectTo(context.Background(), "object-1", 5, &dst)
	if err == nil {
		t.Fatalf("expected oversized body rejection")
	}
	if n != 6 {
		t.Fatalf("bytes copied = %d want 6", n)
	}
	if dst.Len() != 6 {
		t.Fatalf("destination length = %d want 6", dst.Len())
	}
}
