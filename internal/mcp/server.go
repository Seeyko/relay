// Package mcp serves Relay's tools over the Model Context Protocol (Streamable HTTP, JSON responses).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"

	"github.com/Seeyko/relay/internal/store"
)

var supportedVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type Server struct {
	Name    string
	Version string
	// ExtraInstructions is appended to the instructions sent to agents (e.g. the owner's timezone).
	ExtraInstructions string
	tools             []Tool
	byName            map[string]Tool
}

func NewServer(name, version string, tools []Tool) *Server {
	s := &Server{Name: name, Version: version, tools: tools, byName: map[string]Tool{}}
	for _, t := range tools {
		s.byName[t.Name] = t
	}
	return s
}

func (s *Server) Tools() []Tool { return s.tools }

func (s *Server) instructions() string {
	txt := "Relay manages social accounts and websites without a UI. Start with list_accounts. " +
		"Times are RFC3339 with an explicit UTC offset. Never publish without the user's go-ahead: prefer scheduled_at='draft' when unsure."
	if s.ExtraInstructions != "" {
		txt += " " + s.ExtraInstructions
	}
	return txt
}

// Call runs a tool directly (also used by the REST API).
func (s *Server) Call(ctx context.Context, name string, args json.RawMessage) (any, error) {
	t, ok := s.byName[name]
	if !ok {
		return nil, ErrUnknownTool
	}
	res, err := t.handler(ctx, args)
	if isNil(res) {
		res = nil
	}
	return res, err
}

// isNil catches typed nils (e.g. a nil slice) stored in an interface.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

var ErrUnknownTool = errors.New("unknown tool")

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// No server-initiated stream: clients fall back to plain request/response.
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var batch []rpcReq
	single := true
	if len(body) > 0 && body[0] == '[' {
		single = false
		if err := json.Unmarshal(body, &batch); err != nil {
			writeJSON(w, rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32700, "parse error"}})
			return
		}
	} else {
		var one rpcReq
		if err := json.Unmarshal(body, &one); err != nil {
			writeJSON(w, rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32700, "parse error"}})
			return
		}
		batch = []rpcReq{one}
	}
	var out []rpcResp
	for _, req := range batch {
		if len(req.ID) == 0 { // notification
			continue
		}
		out = append(out, s.handle(r.Context(), req))
	}
	if len(out) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if single {
		writeJSON(w, out[0])
		return
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handle(ctx context.Context, req rpcReq) rpcResp {
	resp := rpcResp{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := supportedVersions[0]
		for _, sv := range supportedVersions {
			if sv == p.ProtocolVersion {
				v = sv
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
			"instructions":    s.instructions(),
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": s.tools}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcErr{-32602, "invalid params"}
			return resp
		}
		res, err := s.Call(ctx, p.Name, p.Arguments)
		if errors.Is(err, ErrUnknownTool) {
			resp.Error = &rpcErr{-32602, "unknown tool " + p.Name}
			return resp
		}
		if err != nil {
			msg := err.Error()
			if errors.Is(err, store.ErrNotFound) {
				msg = "not found"
			}
			// Partial results (e.g. a thread stopped half-way) are still returned with the error.
			text := msg
			if res != nil {
				b, _ := json.Marshal(res)
				text = msg + "\n" + string(b)
			}
			resp.Result = map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": text}}}
			return resp
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}
	default:
		resp.Error = &rpcErr{-32601, "method not found: " + req.Method}
	}
	return resp
}
