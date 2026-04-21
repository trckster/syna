package connector

import (
	"bytes"
	"context"
	"io"
	"net/http"
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
