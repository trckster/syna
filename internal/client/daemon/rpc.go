package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"

	"syna/internal/client/agentrpc"
)

func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	var req agentrpc.Request
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(agentrpc.Response{OK: false, Error: err.Error()})
		return
	}
	resp, err := d.dispatchRPC(req)
	out := agentrpc.Response{OK: err == nil}
	if err != nil {
		out.Error = err.Error()
	} else {
		out.Result = agentrpc.EncodeResult(resp)
	}
	_ = json.NewEncoder(conn).Encode(out)
}

func (d *Daemon) dispatchRPC(req agentrpc.Request) (any, error) {
	switch req.Method {
	case "connect":
		var args ConnectRequest
		if err := json.Unmarshal(req.Params, &args); err != nil {
			return nil, err
		}
		return d.Connect(context.Background(), args)
	case "disconnect":
		return nil, d.Disconnect(context.Background())
	case "add":
		var args AddRequest
		if err := json.Unmarshal(req.Params, &args); err != nil {
			return nil, err
		}
		return nil, d.AddRoot(context.Background(), args.Path)
	case "rm":
		var args RemoveRequest
		if err := json.Unmarshal(req.Params, &args); err != nil {
			return nil, err
		}
		return nil, d.RemoveRoot(context.Background(), args.Path)
	case "status":
		return d.Status()
	default:
		return nil, fmt.Errorf("unknown method %s", req.Method)
	}
}
