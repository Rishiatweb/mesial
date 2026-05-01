// Package lspclient manages a language server subprocess and provides
// typed LSP methods for symbol resolution (textDocument/definition).
package lspclient

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.uber.org/zap"
)

// Client manages a language server subprocess.
type Client struct {
	cmd    *exec.Cmd
	conn   jsonrpc2.Conn
	server protocol.Server
}

// Start launches the language server and performs LSP initialization.
func Start(ctx context.Context, serverCmd string, args []string, workspaceRoot string) (*Client, error) {
	cmd := exec.CommandContext(ctx, serverCmd, args...)
	cmd.Dir = workspaceRoot

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Discard stderr to avoid blocking
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", serverCmd, err)
	}

	rwc := &readWriteCloser{
		reader: stdout,
		writer: stdin,
	}

	stream := jsonrpc2.NewStream(rwc)
	logger := zap.NewNop()

	_, conn, server := protocol.NewClient(ctx, &noopClient{}, stream, logger)

	// Initialize
	rootURI := uri.File(workspaceRoot)
	_, err = server.Initialize(ctx, &protocol.InitializeParams{
		RootURI: rootURI,
		Capabilities: protocol.ClientCapabilities{
			TextDocument: &protocol.TextDocumentClientCapabilities{
				Definition: &protocol.DefinitionTextDocumentClientCapabilities{},
			},
		},
		WorkspaceFolders: []protocol.WorkspaceFolder{
			{
				URI:  string(rootURI),
				Name: workspaceRoot,
			},
		},
	})
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("LSP initialize: %w", err)
	}

	if err := server.Initialized(ctx, &protocol.InitializedParams{}); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("LSP initialized: %w", err)
	}

	return &Client{cmd: cmd, conn: conn, server: server}, nil
}

// DidOpen notifies the server that a document was opened.
func (c *Client) DidOpen(ctx context.Context, filePath, languageID, text string) error {
	return c.server.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri.File(filePath),
			LanguageID: protocol.LanguageIdentifier(languageID),
			Version:    1,
			Text:       text,
		},
	})
}

// Definition requests go-to-definition for a position. Returns locations.
func (c *Client) Definition(ctx context.Context, filePath string, line, col uint) ([]protocol.Location, error) {
	result, err := c.server.Definition(ctx, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{
				URI: uri.File(filePath),
			},
			Position: protocol.Position{
				Line:      uint32(line),
				Character: uint32(col),
			},
		},
	})
	if err != nil {
		return nil, err
	}
	return locationsFromResult(result), nil
}

// Shutdown sends shutdown + exit to the server and waits for the process.
func (c *Client) Shutdown(ctx context.Context) error {
	// Best-effort shutdown sequence
	_ = c.server.Shutdown(ctx)
	_ = c.server.Exit(ctx)
	c.conn.Close()
	return c.cmd.Wait()
}

// URIToPath converts an LSP document URI to a filesystem path.
func URIToPath(docURI protocol.DocumentURI) string {
	s := string(docURI)
	if strings.HasPrefix(s, "file://") {
		parsed, err := url.Parse(s)
		if err == nil {
			return parsed.Path
		}
	}
	return s
}

// locationsFromResult converts the Definition response (which may be a single
// Location or []Location) into a uniform []Location.
func locationsFromResult(result []protocol.Location) []protocol.Location {
	return result
}

// readWriteCloser combines separate reader and writer into io.ReadWriteCloser.
type readWriteCloser struct {
	reader io.ReadCloser
	writer io.WriteCloser
}

func (rwc *readWriteCloser) Read(p []byte) (int, error)  { return rwc.reader.Read(p) }
func (rwc *readWriteCloser) Write(p []byte) (int, error) { return rwc.writer.Write(p) }
func (rwc *readWriteCloser) Close() error {
	rerr := rwc.reader.Close()
	werr := rwc.writer.Close()
	if rerr != nil {
		return rerr
	}
	return werr
}

// noopClient implements protocol.Client with all no-op methods.
// The language server sends notifications (diagnostics, log messages, etc.)
// which we safely ignore.
type noopClient struct{}

func (n *noopClient) Progress(context.Context, *protocol.ProgressParams) error { return nil }
func (n *noopClient) WorkDoneProgressCreate(context.Context, *protocol.WorkDoneProgressCreateParams) error {
	return nil
}
func (n *noopClient) LogMessage(context.Context, *protocol.LogMessageParams) error { return nil }
func (n *noopClient) PublishDiagnostics(context.Context, *protocol.PublishDiagnosticsParams) error {
	return nil
}
func (n *noopClient) ShowMessage(context.Context, *protocol.ShowMessageParams) error { return nil }
func (n *noopClient) ShowMessageRequest(context.Context, *protocol.ShowMessageRequestParams) (*protocol.MessageActionItem, error) {
	return nil, nil
}
func (n *noopClient) Telemetry(context.Context, interface{}) error       { return nil }
func (n *noopClient) RegisterCapability(context.Context, *protocol.RegistrationParams) error {
	return nil
}
func (n *noopClient) UnregisterCapability(context.Context, *protocol.UnregistrationParams) error {
	return nil
}
func (n *noopClient) ApplyEdit(context.Context, *protocol.ApplyWorkspaceEditParams) (bool, error) {
	return false, nil
}
func (n *noopClient) Configuration(context.Context, *protocol.ConfigurationParams) ([]interface{}, error) {
	return nil, nil
}
func (n *noopClient) WorkspaceFolders(context.Context) ([]protocol.WorkspaceFolder, error) {
	return nil, nil
}
