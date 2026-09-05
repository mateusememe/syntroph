package githubissues

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/storage"
)

func TestMCPClientModernDiscoveryPaginationReuseAndGracefulShutdown(t *testing.T) {
	shutdown := t.TempDir() + "/shutdown"
	t.Setenv("GO_WANT_MCP_HELPER", "modern")
	t.Setenv("MCP_HELPER_SHUTDOWN", shutdown)
	client, err := newMCPClient(helperCommand(), MCPOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Negotiate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !client.modern || client.protocolVersion != modernMCPVersion {
		t.Fatalf("negotiation = modern:%v version:%s", client.modern, client.protocolVersion)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "first" || tools[1].Name != "second" {
		t.Fatalf("tools = %#v", tools)
	}
	var got map[string]any
	if err := client.CallTool(context.Background(), "echo", map[string]any{"value": "ok"}, &got); err != nil {
		t.Fatal(err)
	}
	if got["value"] != "ok" {
		t.Fatalf("tool result = %#v", got)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shutdown); err != nil {
		t.Fatalf("helper did not observe graceful stdin EOF: %v", err)
	}
}

func TestMCPClientFallsBackToLegacyInitialize(t *testing.T) {
	t.Setenv("GO_WANT_MCP_HELPER", "legacy")
	client, err := newMCPClient(helperCommand(), MCPOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Negotiate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.modern || client.protocolVersion != legacyMCPVersion {
		t.Fatalf("negotiation = modern:%v version:%s", client.modern, client.protocolVersion)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMCPClientCancellationSendsNotification(t *testing.T) {
	cancelled := t.TempDir() + "/cancelled"
	t.Setenv("GO_WANT_MCP_HELPER", "cancel")
	t.Setenv("MCP_HELPER_CANCELLED", cancelled)
	client, _ := newMCPClient(helperCommand(), MCPOptions{Timeout: time.Second, ShutdownTimeout: time.Second})
	if err := client.Negotiate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := client.CallTool(ctx, "hang", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("cancellation error = %v", err)
	}
	_ = client.Close()
	if _, statErr := os.Stat(cancelled); statErr != nil {
		t.Fatalf("helper did not observe cancellation notification: %v", statErr)
	}
}

func TestMCPClientBoundsStderrAndReportsUnexpectedExit(t *testing.T) {
	t.Setenv("GO_WANT_MCP_HELPER", "exit")
	client, _ := newMCPClient(helperCommand(), MCPOptions{Timeout: time.Second, MaxStderrBytes: 32})
	err := client.Negotiate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stderr:") || !strings.Contains(err.Error(), "truncated") || strings.Contains(err.Error(), "mcp-secret") {
		t.Fatalf("unexpected exit error = %v", err)
	}
}

func TestMCPClientRejectsMalformedAndOversizedStdout(t *testing.T) {
	for _, scenario := range []string{"malformed", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("GO_WANT_MCP_HELPER", scenario)
			client, _ := newMCPClient(helperCommand(), MCPOptions{Timeout: time.Second, MaxMessageBytes: 256})
			err := client.Negotiate(context.Background())
			if err == nil || (!strings.Contains(err.Error(), "decode MCP stdout") && !strings.Contains(err.Error(), "token too long")) {
				t.Fatalf("%s error = %v", scenario, err)
			}
		})
	}
}

func TestMCPProviderMirrorsWithCanonicalOrderingAndRecoversLostBinding(t *testing.T) {
	root := filepath.Join(t.TempDir(), "storage")
	state, logPath := filepath.Join(t.TempDir(), "remote.json"), filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("GO_WANT_MCP_HELPER", "provider-pagination")
	t.Setenv("MCP_HELPER_STATE", state)
	t.Setenv("MCP_HELPER_LOG", logPath)
	remote, err := NewMCP("mateusememe/syntroph", helperCommand(), root, MCPOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := storage.NewManagedMirror(root, MCPProviderID, remote)
	if err != nil {
		t.Fatal(err)
	}
	result := managed.Mirror(context.Background(), testDiary())
	if result.State != storage.Mirrored || result.RemoteID != "42" || !strings.HasPrefix(result.RemoteRev, "issue:42:") {
		t.Fatalf("mirror result = %+v", result)
	}
	bindingPath, _ := managed.Bindings.Path(testDiary().Key())
	if _, err := os.Stat(bindingPath); err != nil {
		t.Fatalf("binding missing: %v", err)
	}
	logData, _ := os.ReadFile(logPath)
	logText := string(logData)
	for _, ordered := range []string{"server/discover", "tools/list", "get_label", "label_write", "issue_write:create", "issue_write:update"} {
		index := strings.Index(logText, ordered)
		if index < 0 {
			t.Fatalf("%s absent from MCP log:\n%s", ordered, logText)
		}
		logText = logText[index+len(ordered):]
	}
	if strings.Count(string(logData), "START ") != 1 {
		t.Fatalf("mirror should reuse one process:\n%s", logData)
	}
	if strings.Contains(string(logData), "list_issues") {
		t.Fatalf("normal mirror performed recovery discovery:\n%s", logData)
	}

	// Recovery search is deliberately exercised only after the operational
	// binding is removed. The exact marker must find Issue 42 without a second
	// issue_write:create.
	if err := os.Remove(bindingPath); err != nil {
		t.Fatal(err)
	}
	remote2, _ := NewMCP("mateusememe/syntroph", helperCommand(), root, MCPOptions{Timeout: time.Second})
	managed2, _ := storage.NewManagedMirror(root, MCPProviderID, remote2)
	result = managed2.Recover(context.Background(), testDiary())
	if result.State != storage.Mirrored || result.RemoteID != "42" {
		t.Fatalf("recovered result = %+v", result)
	}
	logData, _ = os.ReadFile(logPath)
	if strings.Count(string(logData), "issue_write:create") != 1 {
		t.Fatalf("marker recovery duplicated issue:\n%s", logData)
	}
	if !strings.Contains(string(logData), "list_issues:after=page-2") {
		t.Fatalf("recovery did not use the official list_issues after cursor:\n%s", logData)
	}
}

func TestMCPProviderReusesOneNegotiatedProcessForACommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), "storage")
	logPath := filepath.Join(t.TempDir(), "calls.log")
	shutdown := filepath.Join(t.TempDir(), "shutdown")
	t.Setenv("GO_WANT_MCP_HELPER", "provider")
	t.Setenv("MCP_HELPER_STATE", filepath.Join(t.TempDir(), "remote.json"))
	t.Setenv("MCP_HELPER_LOG", logPath)
	t.Setenv("MCP_HELPER_SHUTDOWN", shutdown)
	remote, err := NewMCP("mateusememe/syntroph", helperCommand(), root, MCPOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := storage.NewManagedMirror(root, MCPProviderID, remote)
	if err != nil {
		t.Fatal(err)
	}
	managed.BeginCommand()
	if result := managed.Mirror(context.Background(), testDiary()); result.State != storage.Mirrored {
		t.Fatalf("mirror = %+v", result)
	}
	if result := managed.Status(context.Background(), testDiary()); result.State != storage.Mirrored {
		t.Fatalf("status = %+v", result)
	}
	if err := managed.EndCommand(); err != nil {
		t.Fatal(err)
	}
	logData, _ := os.ReadFile(logPath)
	for _, want := range []string{"START ", "server/discover", "tools/list"} {
		if got := strings.Count(string(logData), want); got != 1 {
			t.Fatalf("%s count = %d, want 1:\n%s", want, got, logData)
		}
	}
	if _, err := os.Stat(shutdown); err != nil {
		t.Fatalf("command-scoped process did not shut down gracefully: %v", err)
	}
}

func TestMCPProviderRejectsReadOnlyCapabilitiesBeforeWrites(t *testing.T) {
	t.Setenv("GO_WANT_MCP_HELPER", "readonly")
	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("MCP_HELPER_LOG", logPath)
	provider, err := NewMCP("mateusememe/syntroph", helperCommand(), filepath.Join(t.TempDir(), "storage"), MCPOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	result := provider.Mirror(context.Background(), testDiary())
	if result.State != storage.StoragePrerequisiteMissing || !errorsIs(result.Cause, storage.ErrPrerequisiteMissing) || !strings.Contains(result.Cause.Error(), "label_write") {
		t.Fatalf("read-only result = %+v", result)
	}
	logData, _ := os.ReadFile(logPath)
	if strings.Contains(string(logData), "tools/call") {
		t.Fatalf("provider wrote or called a tool before capability validation:\n%s", logData)
	}
}

func TestMCPProviderRejectsIncompleteToolSchemaBeforeWrites(t *testing.T) {
	t.Setenv("GO_WANT_MCP_HELPER", "invalid-schema")
	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("MCP_HELPER_LOG", logPath)
	provider, err := NewMCP("mateusememe/syntroph", helperCommand(), filepath.Join(t.TempDir(), "storage"), MCPOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	result := provider.Mirror(context.Background(), testDiary())
	if result.State != storage.StoragePrerequisiteMissing || !strings.Contains(result.Cause.Error(), "state_reason") {
		t.Fatalf("invalid schema result = %+v", result)
	}
	logData, _ := os.ReadFile(logPath)
	if strings.Contains(string(logData), "tools/call") {
		t.Fatalf("provider called a tool before schema validation:\n%s", logData)
	}
}

func TestMCPProviderClassifiesPermissionToolFailureAsPrerequisite(t *testing.T) {
	t.Setenv("GO_WANT_MCP_HELPER", "provider-denied")
	t.Setenv("MCP_HELPER_STATE", filepath.Join(t.TempDir(), "remote.json"))
	provider, err := NewMCP("mateusememe/syntroph", helperCommand(), filepath.Join(t.TempDir(), "storage"), MCPOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	result := provider.Mirror(context.Background(), testDiary())
	if result.State != storage.StoragePrerequisiteMissing || !strings.Contains(strings.ToLower(result.Cause.Error()), "permission") {
		t.Fatalf("permission result = %+v", result)
	}
}

func TestMCPProviderClassifiesProtocolInitializationFailureAsPrerequisite(t *testing.T) {
	t.Setenv("GO_WANT_MCP_HELPER", "legacy-init-error")
	provider, _ := NewMCP("mateusememe/syntroph", helperCommand(), filepath.Join(t.TempDir(), "storage"), MCPOptions{Timeout: time.Second})
	result := provider.Mirror(context.Background(), testDiary())
	if result.State != storage.StoragePrerequisiteMissing || !strings.Contains(result.Cause.Error(), "initialize") {
		t.Fatalf("initialization result = %+v", result)
	}
}

func TestMCPProviderClassifiesToolTimeoutWithoutLosingLocalSuccess(t *testing.T) {
	t.Setenv("GO_WANT_MCP_HELPER", "provider-hang")
	provider, _ := NewMCP("mateusememe/syntroph", helperCommand(), filepath.Join(t.TempDir(), "storage"), MCPOptions{Timeout: 40 * time.Millisecond, ShutdownTimeout: time.Second})
	result := provider.Mirror(context.Background(), testDiary())
	if result.State != storage.StorageSyncPending || result.FailureClass != storage.FailureTransient || !strings.Contains(result.Cause.Error(), "deadline") {
		t.Fatalf("timeout result = %+v", result)
	}
}

func TestMCPProviderClassifiesMalformedToolResultAndShutdownFailure(t *testing.T) {
	t.Run("malformed tool result", func(t *testing.T) {
		t.Setenv("GO_WANT_MCP_HELPER", "provider-malformed-result")
		t.Setenv("MCP_HELPER_STATE", filepath.Join(t.TempDir(), "remote.json"))
		provider, _ := NewMCP("mateusememe/syntroph", helperCommand(), filepath.Join(t.TempDir(), "storage"), MCPOptions{Timeout: time.Second})
		result := provider.Mirror(context.Background(), testDiary())
		if result.State != storage.StorageSyncPending || !strings.Contains(result.Cause.Error(), "decode MCP structured tool result") {
			t.Fatalf("malformed result = %+v", result)
		}
	})

	t.Run("graceful shutdown timeout", func(t *testing.T) {
		t.Setenv("GO_WANT_MCP_HELPER", "provider-shutdown-hang")
		t.Setenv("MCP_HELPER_STATE", filepath.Join(t.TempDir(), "remote.json"))
		provider, _ := NewMCP("mateusememe/syntroph", helperCommand(), filepath.Join(t.TempDir(), "storage"), MCPOptions{Timeout: time.Second, ShutdownTimeout: 30 * time.Millisecond})
		result := provider.Mirror(context.Background(), testDiary())
		if result.State != storage.StorageSyncPending || !strings.Contains(result.Cause.Error(), "did not exit") {
			t.Fatalf("shutdown result = %+v", result)
		}
	})
}

func TestMCPProviderConflictUsesAppendOnlyCorrectionAndTypedBinding(t *testing.T) {
	root := filepath.Join(t.TempDir(), "storage")
	statePath := filepath.Join(t.TempDir(), "remote.json")
	t.Setenv("GO_WANT_MCP_HELPER", "provider")
	t.Setenv("MCP_HELPER_STATE", statePath)
	remote, _ := NewMCP("mateusememe/syntroph", helperCommand(), root, MCPOptions{Timeout: time.Second})
	managed, _ := storage.NewManagedMirror(root, MCPProviderID, remote)
	if result := managed.Mirror(context.Background(), testDiary()); result.State != storage.Mirrored {
		t.Fatalf("initial mirror = %+v", result)
	}
	state := readHelperState()
	state.Issue.Body = "human edit"
	state.Issue.UpdatedAt = mustTime("2026-08-31T13:00:00Z")
	writeHelperState(state)

	conflict := managed.Status(context.Background(), testDiary())
	if conflict.State != storage.StorageSyncConflict || !strings.HasPrefix(conflict.RemoteRev, "issue:42:") {
		t.Fatalf("conflict = %+v", conflict)
	}
	resolved := managed.Resolve(context.Background(), testDiary(), storage.KeepLocal, conflict.RemoteRev)
	if resolved.State != storage.Mirrored || !strings.HasPrefix(resolved.RemoteRev, "comment:77:") {
		t.Fatalf("keep-local = %+v", resolved)
	}
	state = readHelperState()
	if state.Issue.Body != "human edit" || len(state.Comments) != 1 {
		t.Fatalf("append-only correction mutated issue or duplicated comments: %+v", state)
	}
	binding, ok, err := managed.Bindings.Load(context.Background(), testDiary().Key())
	if err != nil || !ok || !strings.HasPrefix(string(binding.RemoteRevision), "comment:77:") {
		t.Fatalf("correction binding = %+v ok=%v err=%v", binding, ok, err)
	}

	// Redelivery recognizes the versioned correction and performs no second
	// comment write.
	resolved = managed.Resolve(context.Background(), testDiary(), storage.KeepLocal, string(binding.RemoteRevision))
	if resolved.State != storage.Mirrored {
		t.Fatalf("correction replay = %+v", resolved)
	}
	if state = readHelperState(); len(state.Comments) != 1 {
		t.Fatalf("correction replay duplicated comment: %+v", state.Comments)
	}
}

func helperCommand() []string {
	return []string{os.Args[0], "-test.run=^TestMCPHelperProcess$"}
}

func TestMCPHelperProcess(t *testing.T) {
	scenario := os.Getenv("GO_WANT_MCP_HELPER")
	if scenario == "" {
		return
	}
	if scenario == "exit" {
		fmt.Fprint(os.Stderr, "token=mcp-secret "+strings.Repeat("diagnostic", 20))
		os.Exit(17)
	}
	if scenario == "malformed" {
		fmt.Fprintln(os.Stdout, "not-json")
		os.Exit(0)
	}
	if scenario == "oversized" {
		fmt.Fprintln(os.Stdout, strings.Repeat("x", 1024))
		os.Exit(0)
	}
	logHelper("START " + strconv.Itoa(os.Getpid()))
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  map[string]any  `json:"params,omitempty"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(18)
		}
		logHelper(request.Method)
		if request.Method == "notifications/cancelled" {
			_ = os.WriteFile(os.Getenv("MCP_HELPER_CANCELLED"), []byte("yes"), 0o600)
			continue
		}
		if len(request.ID) == 0 {
			continue
		}
		response := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(request.ID)}
		switch request.Method {
		case "server/discover":
			if strings.HasPrefix(scenario, "legacy") {
				response["error"] = map[string]any{"code": -32601, "message": "method not found"}
			} else {
				meta, _ := request.Params["_meta"].(map[string]any)
				if meta["io.modelcontextprotocol/protocolVersion"] != modernMCPVersion {
					response["error"] = map[string]any{"code": -32602, "message": "missing modern metadata"}
				} else {
					response["result"] = map[string]any{"resultType": "complete", "supportedVersions": []string{modernMCPVersion}}
				}
			}
		case "initialize":
			if scenario == "legacy-init-error" {
				response["error"] = map[string]any{"code": -32602, "message": "unsupported client"}
			} else {
				response["result"] = map[string]any{"protocolVersion": legacyMCPVersion, "capabilities": map[string]any{}}
			}
		case "tools/list":
			if scenario != "legacy" {
				meta, _ := request.Params["_meta"].(map[string]any)
				if meta["io.modelcontextprotocol/protocolVersion"] != modernMCPVersion {
					response["error"] = map[string]any{"code": -32602, "message": "missing per-request metadata"}
					break
				}
			}
			if strings.HasPrefix(scenario, "provider") || scenario == "readonly" || scenario == "invalid-schema" {
				tools := providerTools()
				if scenario == "readonly" {
					delete(tools, "label_write")
				}
				if scenario == "invalid-schema" {
					delete(tools["issue_write"]["properties"].(map[string]any), "state_reason")
				}
				values := make([]map[string]any, 0, len(tools))
				for _, name := range []string{"get_label", "label_write", "issue_read", "issue_write", "add_issue_comment", "list_issues"} {
					if schema, ok := tools[name]; ok {
						values = append(values, map[string]any{"name": name, "inputSchema": schema})
					}
				}
				response["result"] = map[string]any{"tools": values}
			} else if request.Params["cursor"] == "page-2" {
				response["result"] = map[string]any{"tools": []map[string]any{{"name": "second", "inputSchema": objectSchema("value")}}}
			} else if scenario == "legacy" {
				response["result"] = map[string]any{"tools": []any{}}
			} else {
				response["result"] = map[string]any{"tools": []map[string]any{{"name": "first", "inputSchema": objectSchema("value")}}, "nextCursor": "page-2"}
			}
		case "tools/call":
			if scenario == "cancel" {
				continue
			}
			if scenario == "provider-hang" {
				continue
			} else if strings.HasPrefix(scenario, "provider") {
				response["result"] = providerToolCall(request.Params)
			} else {
				response["result"] = map[string]any{"structuredContent": map[string]any{"value": "ok"}}
			}
		default:
			response["error"] = map[string]any{"code": -32601, "message": "unknown"}
		}
		_ = encoder.Encode(response)
	}
	if scenario == "provider-shutdown-hang" {
		time.Sleep(10 * time.Second)
	}
	if path := os.Getenv("MCP_HELPER_SHUTDOWN"); path != "" {
		_ = os.WriteFile(path, []byte("yes"), 0o600)
	}
	os.Exit(0)
}

type helperRemoteState struct {
	Labels   map[string]label `json:"labels"`
	Issue    issue            `json:"issue"`
	Comments []comment        `json:"comments"`
}

func providerToolCall(params map[string]any) map[string]any {
	name, _ := params["name"].(string)
	arguments, _ := params["arguments"].(map[string]any)
	method, _ := arguments["method"].(string)
	logHelper(name + map[bool]string{true: ":" + method, false: ""}[method != ""])
	if os.Getenv("GO_WANT_MCP_HELPER") == "provider-malformed-result" && name == "get_label" {
		return map[string]any{"structuredContent": "not-an-object"}
	}
	state := readHelperState()
	structured := func(value any) map[string]any { return map[string]any{"structuredContent": value} }
	switch name {
	case "get_label":
		if os.Getenv("GO_WANT_MCP_HELPER") == "provider-denied" {
			return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": "forbidden: Issues write permission is required"}}}
		}
		name, _ := arguments["name"].(string)
		value, ok := state.Labels[name]
		if !ok {
			return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": "label not found"}}}
		}
		return structured(value)
	case "label_write":
		if state.Labels == nil {
			state.Labels = map[string]label{}
		}
		value := label{Name: stringArg(arguments, "name"), Color: stringArg(arguments, "color"), Description: stringArg(arguments, "description")}
		state.Labels[value.Name] = value
		writeHelperState(state)
		return structured(value)
	case "list_issues":
		if os.Getenv("GO_WANT_MCP_HELPER") == "provider-malformed-result" {
			return map[string]any{"structuredContent": "not-an-object"}
		}
		after := stringArg(arguments, "after")
		logHelper("list_issues:after=" + after)
		if os.Getenv("GO_WANT_MCP_HELPER") == "provider-pagination" && after == "" {
			page := issueListResult{}
			page.PageInfo.HasNextPage = true
			page.PageInfo.NextCursor = "page-2"
			return structured(page)
		}
		stateName := stringArg(arguments, "state")
		issues := []issue{}
		if state.Issue.Number != 0 && state.Issue.State == stateName {
			issues = append(issues, state.Issue)
		}
		return structured(issueListResult{Issues: issues})
	case "issue_write":
		if method == "create" {
			state.Issue = issue{Number: 42, HTMLURL: "https://github.test/issues/42", Body: stringArg(arguments, "body"), UpdatedAt: mustTime("2026-08-31T12:00:00Z"), State: "open"}
		} else {
			state.Issue.State, state.Issue.StateReason = "closed", "completed"
			state.Issue.UpdatedAt = mustTime("2026-08-31T12:01:00Z")
		}
		writeHelperState(state)
		return structured(state.Issue)
	case "issue_read":
		if method == "get_comments" {
			return structured(map[string]any{"comments": state.Comments})
		}
		// Exercise the official server's JSON-in-text fallback.
		data, _ := json.Marshal(state.Issue)
		return map[string]any{"content": []map[string]any{{"type": "text", "text": string(data)}}}
	case "add_issue_comment":
		value := comment{ID: int64(len(state.Comments) + 77), HTMLURL: "https://github.test/issues/42#comment", Body: stringArg(arguments, "body"), UpdatedAt: mustTime("2026-08-31T12:02:00Z")}
		state.Comments = append(state.Comments, value)
		writeHelperState(state)
		return structured(value)
	default:
		return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": "unsupported tool"}}}
	}
}

func providerTools() map[string]map[string]any {
	return map[string]map[string]any{
		"get_label":         schemaWithMethods([]string{"owner", "repo", "name"}),
		"label_write":       schemaWithMethods([]string{"owner", "repo", "method", "name", "color", "description"}, "create", "update"),
		"issue_read":        schemaWithMethods([]string{"owner", "repo", "method", "issue_number"}, "get", "get_comments"),
		"issue_write":       schemaWithMethods([]string{"owner", "repo", "method", "title", "body", "labels", "issue_number", "state", "state_reason"}, "create", "update"),
		"add_issue_comment": schemaWithMethods([]string{"owner", "repo", "issue_number", "body"}),
		"list_issues":       schemaWithMethods([]string{"owner", "repo", "state", "labels", "after"}),
	}
}

func schemaWithMethods(properties []string, methods ...string) map[string]any {
	schema := objectSchema(properties...)
	if len(methods) != 0 {
		values := make([]any, len(methods))
		for index, method := range methods {
			values[index] = method
		}
		schema["properties"].(map[string]any)["method"] = map[string]any{"type": "string", "enum": values}
	}
	return schema
}

func readHelperState() helperRemoteState {
	var state helperRemoteState
	data, _ := os.ReadFile(os.Getenv("MCP_HELPER_STATE"))
	_ = json.Unmarshal(data, &state)
	return state
}

func writeHelperState(state helperRemoteState) {
	data, _ := json.Marshal(state)
	_ = os.WriteFile(os.Getenv("MCP_HELPER_STATE"), data, 0o600)
}

func logHelper(line string) {
	path := os.Getenv("MCP_HELPER_LOG")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = fmt.Fprintln(file, line)
		_ = file.Close()
	}
}

func stringArg(arguments map[string]any, key string) string {
	value, _ := arguments[key].(string)
	return value
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		value, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = value.Unwrap()
	}
	return false
}

func objectSchema(properties ...string) map[string]any {
	values := map[string]any{}
	for _, property := range properties {
		values[property] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": values}
}
