package robinhood

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var requiredTools = []string{
	"get_accounts",
	"get_portfolio",
	"get_equity_positions",
	"get_equity_quotes",
	"get_equity_orders",
	"get_equity_tradability",
	"review_equity_order",
	"place_equity_order",
}

type toolCaller interface {
	Call(ctx context.Context, name string, arguments map[string]any) (json.RawMessage, error)
	InputSchema(name string) map[string]any
	Close() error
}

type mcpCaller struct {
	session *mcp.ClientSession
	schemas map[string]map[string]any
}

type mcpClientOptions struct {
	Endpoint    string
	StateFile   string
	RedirectURL string
	Fetcher     auth.AuthorizationCodeFetcher
	Writer      SecretWriter
}

func validateMCPEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("Robinhood MCP endpoint must be an HTTPS URL without user information")
	}
	return nil
}

func newMCPCaller(ctx context.Context, opts mcpClientOptions) (*mcpCaller, error) {
	httpClient := &http.Client{Timeout: 45 * time.Second}
	oauthOpts := oauthOptions{
		StateFile: opts.StateFile, RedirectURL: opts.RedirectURL,
		Writer: opts.Writer, HTTPClient: httpClient, AuthorizationCodeFetcher: opts.Fetcher,
	}
	oauthHandler, err := newOAuthHandler(oauthOpts)
	if err != nil {
		return nil, fmt.Errorf("Robinhood OAuth: %w", err)
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint: opts.Endpoint, HTTPClient: httpClient, OAuthHandler: oauthHandler,
		DisableStandaloneSSE: true, MaxRetries: -1,
	}
	discardLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := mcp.NewClient(&mcp.Implementation{Name: "quant-stocks", Version: "1.0.0"}, &mcp.ClientOptions{
		Logger: discardLogger, Capabilities: &mcp.ClientCapabilities{},
	})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect Robinhood MCP: %w", err)
	}
	caller := &mcpCaller{session: session, schemas: make(map[string]map[string]any)}
	if err := caller.discoverTools(ctx); err != nil {
		session.Close()
		return nil, err
	}
	return caller, nil
}

func (c *mcpCaller) discoverTools(ctx context.Context) error {
	cursor := ""
	for {
		params := &mcp.ListToolsParams{Cursor: cursor}
		result, err := c.session.ListTools(ctx, params)
		if err != nil {
			return fmt.Errorf("discover Robinhood tools: %w", err)
		}
		for _, tool := range result.Tools {
			schema := make(map[string]any)
			data, err := json.Marshal(tool.InputSchema)
			if err != nil {
				return fmt.Errorf("encode schema for %s: %w", tool.Name, err)
			}
			if err := json.Unmarshal(data, &schema); err != nil {
				return fmt.Errorf("decode schema for %s: %w", tool.Name, err)
			}
			c.schemas[tool.Name] = schema
		}
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	for _, name := range requiredTools {
		if _, ok := c.schemas[name]; !ok {
			return fmt.Errorf("Robinhood MCP is missing required tool %q", name)
		}
	}
	return nil
}

func (c *mcpCaller) Call(ctx context.Context, name string, arguments map[string]any) (json.RawMessage, error) {
	if _, ok := c.schemas[name]; !ok {
		return nil, fmt.Errorf("Robinhood MCP tool %q was not discovered", name)
	}
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return nil, err
	}
	if result.IsError {
		return nil, fmt.Errorf("tool %s reported an error", name)
	}
	if result.StructuredContent != nil {
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	for _, content := range result.Content {
		text, ok := content.(*mcp.TextContent)
		if !ok {
			continue
		}
		if data := extractJSON(text.Text); data != nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("tool %s returned no structured JSON", name)
}

func (c *mcpCaller) InputSchema(name string) map[string]any {
	return c.schemas[name]
}

func (c *mcpCaller) Close() error {
	return c.session.Close()
}

func extractJSON(value string) json.RawMessage {
	value = strings.TrimSpace(value)
	if json.Valid([]byte(value)) {
		return json.RawMessage(value)
	}
	for i, r := range value {
		if r != '{' && r != '[' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(value[i:]))
		var decoded any
		if err := decoder.Decode(&decoded); err == nil {
			data, _ := json.Marshal(decoded)
			return data
		}
	}
	return nil
}

// BootstrapOAuth performs the one-time desktop authorization and verifies the
// live tool inventory. The resulting state file is always mode 0600.
func BootstrapOAuth(ctx context.Context, endpoint, stateFile string, callbackPort int, output io.Writer) error {
	if endpoint == "" || stateFile == "" {
		return errors.New("endpoint and state file are required")
	}
	if err := validateMCPEndpoint(endpoint); err != nil {
		return err
	}
	receiver, err := newCodeReceiver(callbackPort)
	if err != nil {
		return fmt.Errorf("start OAuth callback listener: %w", err)
	}
	defer receiver.close(context.Background())
	fetcher := receiver.fetcher(output)
	caller, err := newMCPCaller(ctx, mcpClientOptions{
		Endpoint: endpoint, StateFile: stateFile, RedirectURL: receiver.redirectURL(),
		Fetcher: fetcher,
	})
	if err != nil {
		return err
	}
	defer caller.Close()
	info, err := os.Stat(stateFile)
	if err != nil {
		return fmt.Errorf("OAuth state was not persisted: %w", err)
	}
	if info.Mode().Perm() != 0600 {
		return fmt.Errorf("OAuth state permissions are %o, want 600", info.Mode().Perm())
	}
	fmt.Fprintln(output, "Robinhood OAuth state saved and Trading MCP tools verified.")
	return nil
}
