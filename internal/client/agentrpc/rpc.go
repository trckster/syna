package agentrpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
)

type Request struct {
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params,omitempty"`
	Progress bool            `json:"progress,omitempty"`
}

type Response struct {
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Progress *Progress       `json:"progress,omitempty"`
}

type Progress struct {
	Stage        string `json:"stage"`
	Message      string `json:"message,omitempty"`
	Path         string `json:"path,omitempty"`
	DoneBytes    int64  `json:"done_bytes,omitempty"`
	TotalBytes   int64  `json:"total_bytes,omitempty"`
	DoneFiles    int    `json:"done_files,omitempty"`
	TotalFiles   int    `json:"total_files,omitempty"`
	DoneEntries  int    `json:"done_entries,omitempty"`
	TotalEntries int    `json:"total_entries,omitempty"`
}

func Call(socketPath, method string, req any, resp any) error {
	return call(socketPath, method, req, resp, false, nil)
}

func CallWithProgress(socketPath, method string, req any, resp any, onProgress func(Progress)) error {
	return call(socketPath, method, req, resp, true, onProgress)
}

func call(socketPath, method string, req any, resp any, streamProgress bool, onProgress func(Progress)) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	var params json.RawMessage
	if req != nil {
		b, err := json.Marshal(req)
		if err != nil {
			return err
		}
		params = b
	}
	if err := json.NewEncoder(conn).Encode(Request{Method: method, Params: params, Progress: streamProgress}); err != nil {
		return err
	}
	dec := json.NewDecoder(bufio.NewReader(conn))
	for {
		var out Response
		if err := dec.Decode(&out); err != nil {
			return err
		}
		if out.Progress != nil {
			if onProgress != nil {
				onProgress(*out.Progress)
			}
			continue
		}
		if !out.OK {
			if out.Error == "" {
				out.Error = "rpc failed"
			}
			return errors.New(out.Error)
		}
		if resp != nil && len(out.Result) > 0 {
			return json.Unmarshal(out.Result, resp)
		}
		return nil
	}
}

func EncodeResult(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}
