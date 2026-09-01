package githubissues

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	modernMCPVersion  = "2026-07-28"
	legacyMCPVersion  = "2025-11-25"
	defaultMCPTimeout = 15 * time.Second
	defaultMCPMessage = 32 << 20
	defaultMCPStderr  = 64 << 10
)

type MCPOptions struct {
	Timeout         time.Duration
	ShutdownTimeout time.Duration
	MaxMessageBytes int
	MaxStderrBytes  int
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolCallResult struct {
	Content           []toolContent   `json:"content,omitempty"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type boundedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return len(p), nil
	}
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		b.data = append(b.data, p[:remaining]...)
	}
	if remaining < len(p) {
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	value := strings.TrimSpace(string(b.data))
	if b.truncated {
		value += " [truncated]"
	}
	return value
}

type mcpClient struct {
	command         []string
	opts            MCPOptions
	cmd             *exec.Cmd
	stdin           io.WriteCloser
	responses       chan rpcResponse
	readErrors      chan error
	stderr          *boundedBuffer
	writeMu         sync.Mutex
	callMu          sync.Mutex
	nextID          atomic.Int64
	modern          bool
	protocolVersion string
	closed          atomic.Bool
}

func newMCPClient(command []string, opts MCPOptions) (*mcpClient, error) {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return nil, errors.New("MCP command requires an executable")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultMCPTimeout
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 2 * time.Second
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = defaultMCPMessage
	}
	if opts.MaxStderrBytes <= 0 {
		opts.MaxStderrBytes = defaultMCPStderr
	}
	return &mcpClient{command: append([]string(nil), command...), opts: opts}, nil
}

func (c *mcpClient) Start(ctx context.Context) error {
	if c.cmd != nil {
		return nil
	}
	c.cmd = exec.CommandContext(context.WithoutCancel(ctx), c.command[0], c.command[1:]...)
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open MCP stdin: %w", err)
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open MCP stdout: %w", err)
	}
	stderrPipe, err := c.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open MCP stderr: %w", err)
	}
	c.stderr = &boundedBuffer{limit: c.opts.MaxStderrBytes}
	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("start MCP process: %w", err)
	}
	c.stdin = stdin
	c.responses = make(chan rpcResponse, 16)
	c.readErrors = make(chan error, 1)
	go c.readLoop(stdout)
	go func() { _, _ = io.Copy(c.stderr, stderrPipe) }()
	return nil
}

func (c *mcpClient) Negotiate(ctx context.Context) error {
	if err := c.Start(ctx); err != nil {
		return err
	}
	var discovered struct {
		SupportedVersions []string `json:"supportedVersions"`
	}
	modernCtx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	modernErr := c.call(modernCtx, "server/discover", map[string]any{"_meta": modernMetadata()}, &discovered, false)
	cancel()
	if modernErr == nil {
		if !containsString(discovered.SupportedVersions, modernMCPVersion) {
			return &mcpProtocolError{message: fmt.Sprintf("modern MCP server does not support %s (advertised %v)", modernMCPVersion, discovered.SupportedVersions)}
		}
		c.modern, c.protocolVersion = true, modernMCPVersion
		return nil
	}
	var modernRPC *mcpRPCError
	if errors.As(modernErr, &modernRPC) && modernRPC.Code == -32022 {
		var data struct {
			Supported []string `json:"supported"`
		}
		_ = json.Unmarshal(modernRPC.Data, &data)
		if containsString(data.Supported, modernMCPVersion) {
			c.modern, c.protocolVersion = true, modernMCPVersion
			return nil
		}
		return &mcpProtocolError{message: fmt.Sprintf("modern MCP server rejected %s and advertised %v", modernMCPVersion, data.Supported)}
	}
	// A timeout may leave a response in flight. Restart before the legacy
	// handshake so request IDs and protocol eras cannot be confused.
	if errors.Is(modernErr, context.DeadlineExceeded) {
		_ = c.Close()
		c.reset()
		if err := c.Start(ctx); err != nil {
			return err
		}
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	params := map[string]any{
		"protocolVersion": legacyMCPVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "syntroph", "version": "dev"},
	}
	if err := c.call(ctx, "initialize", params, &initialized, false); err != nil {
		var rpc *mcpRPCError
		if errors.As(err, &rpc) {
			return &mcpProtocolError{message: fmt.Sprintf("legacy MCP initialize failed after modern discovery error %v: %v", modernErr, err)}
		}
		return fmt.Errorf("negotiate MCP (modern discovery failed: %v): %w", modernErr, err)
	}
	if initialized.ProtocolVersion == "" {
		return &mcpProtocolError{message: "legacy MCP initialize response omitted protocolVersion"}
	}
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		return err
	}
	c.modern, c.protocolVersion = false, initialized.ProtocolVersion
	return nil
}

func modernMetadata() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/protocolVersion":    modernMCPVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		"io.modelcontextprotocol/clientInfo":         map[string]string{"name": "syntroph", "version": "dev"},
	}
}

func (c *mcpClient) ListTools(ctx context.Context) ([]mcpTool, error) {
	var all []mcpTool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", c.params(params), &page, true); err != nil {
			return nil, err
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

func (c *mcpClient) CallTool(ctx context.Context, name string, arguments map[string]any, out any) error {
	var result toolCallResult
	params := c.params(map[string]any{"name": name, "arguments": arguments})
	if err := c.call(ctx, "tools/call", params, &result, true); err != nil {
		return err
	}
	if result.IsError {
		message := toolResultText(result)
		if message == "" {
			message = "MCP tool returned isError"
		}
		return &mcpToolError{message: message}
	}
	if out == nil {
		return nil
	}
	return decodeToolPayload(result, out)
}

func (c *mcpClient) params(params map[string]any) map[string]any {
	if c.modern {
		params["_meta"] = modernMetadata()
	}
	return params
}

func (c *mcpClient) call(ctx context.Context, method string, params any, out any, cancellable bool) error {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	if c.closed.Load() {
		return errors.New("MCP client is closed")
	}
	id := c.nextID.Add(1)
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > c.opts.Timeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opts.Timeout)
		defer cancel()
	}
	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			if cancellable {
				_ = c.notify("notifications/cancelled", map[string]any{"requestId": id, "reason": ctx.Err().Error()})
			}
			return ctx.Err()
		case err := <-c.readErrors:
			return c.processError(err)
		case response := <-c.responses:
			if response.Method != "" || response.ID == nil {
				continue
			}
			var responseID int64
			if err := json.Unmarshal(response.ID, &responseID); err != nil || responseID != id {
				continue
			}
			if response.Error != nil {
				return &mcpRPCError{Code: response.Error.Code, Message: response.Error.Message, Data: response.Error.Data}
			}
			if out != nil && len(response.Result) != 0 {
				if err := json.Unmarshal(response.Result, out); err != nil {
					return fmt.Errorf("decode MCP %s result: %w", method, err)
				}
			}
			return nil
		}
	}
}

func (c *mcpClient) notify(method string, params any) error {
	return c.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *mcpClient) write(message rpcRequest) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdin == nil {
		return errors.New("MCP process is not started")
	}
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		return c.processError(err)
	}
	return nil
}

func (c *mcpClient) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), c.opts.MaxMessageBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var response rpcResponse
		if err := json.Unmarshal(line, &response); err != nil {
			c.sendReadError(fmt.Errorf("decode MCP stdout: %w", err))
			return
		}
		c.responses <- response
	}
	if err := scanner.Err(); err != nil {
		c.sendReadError(fmt.Errorf("read MCP stdout: %w", err))
		return
	}
	c.sendReadError(io.EOF)
}

func (c *mcpClient) sendReadError(err error) {
	select {
	case c.readErrors <- err:
	default:
	}
}

func (c *mcpClient) processError(err error) error {
	detail := ""
	if c.stderr != nil {
		detail = c.stderr.String()
	}
	if detail != "" {
		return fmt.Errorf("MCP process: %w (stderr: %s)", err, detail)
	}
	return fmt.Errorf("MCP process: %w", err)
}

func (c *mcpClient) Close() error {
	if c.cmd == nil || c.closed.Swap(true) {
		return nil
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return c.processError(err)
		}
		return nil
	case <-time.After(c.opts.ShutdownTimeout):
		_ = c.cmd.Process.Kill()
		<-done
		return errors.New("MCP process did not exit after stdin closed and was killed")
	}
}

func (c *mcpClient) reset() {
	c.cmd, c.stdin, c.responses, c.readErrors, c.stderr = nil, nil, nil, nil, nil
	c.closed.Store(false)
}

type mcpRPCError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *mcpRPCError) Error() string {
	return fmt.Sprintf("MCP JSON-RPC error %d: %s", e.Code, e.Message)
}

type mcpToolError struct{ message string }

func (e *mcpToolError) Error() string { return e.message }

type mcpProtocolError struct{ message string }

func (e *mcpProtocolError) Error() string { return e.message }

func toolResultText(result toolCallResult) string {
	var values []string
	for _, content := range result.Content {
		if content.Type == "text" && strings.TrimSpace(content.Text) != "" {
			values = append(values, strings.TrimSpace(content.Text))
		}
	}
	return strings.Join(values, "; ")
}

func decodeToolPayload(result toolCallResult, out any) error {
	if len(result.StructuredContent) != 0 && string(result.StructuredContent) != "null" {
		if err := json.Unmarshal(result.StructuredContent, out); err != nil {
			return fmt.Errorf("decode MCP structured tool result: %w", err)
		}
		return nil
	}
	var candidates []string
	for _, content := range result.Content {
		if content.Type == "text" && json.Valid([]byte(strings.TrimSpace(content.Text))) {
			candidates = append(candidates, strings.TrimSpace(content.Text))
		}
	}
	if len(candidates) != 1 {
		return fmt.Errorf("MCP tool result requires exactly one JSON text payload, got %d", len(candidates))
	}
	if err := json.Unmarshal([]byte(candidates[0]), out); err != nil {
		return fmt.Errorf("decode MCP text tool result: %w", err)
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
