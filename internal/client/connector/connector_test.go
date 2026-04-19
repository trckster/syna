package connector

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"syna/internal/common/protocol"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
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
