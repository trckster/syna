package agentrpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
)

type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

func Call(socketPath, method string, req any, resp any) error {
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
	if err := json.NewEncoder(conn).Encode(Request{Method: method, Params: params}); err != nil {
		return err
	}
	var out Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&out); err != nil {
		return err
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

func EncodeResult(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}
