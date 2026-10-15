package client

import (
	"context"
	"fmt"
	"io"

	"github.com/mark3labs/mcp-go/client/transport"
)

// NewStdioMCPClient creates a new stdio-based MCP client that communicates with a subprocess.
// It launches the specified command with given arguments and sets up stdin/stdout pipes for communication.
// Returns an error if the subprocess cannot be started or the pipes cannot be created.
//
// NOTICE: NewStdioMCPClient starts the client, so the returned client is
// ready to Initialize and receives the server's notifications. This is for
// backward compatibility.
func NewStdioMCPClient(
	command string,
	env []string,
	args ...string,
) (*Client, error) {
	return NewStdioMCPClientWithOptions(command, env, args)
}

// NewStdioMCPClientWithOptions creates a new stdio-based MCP client that communicates with a subprocess.
// It launches the specified command with given arguments and sets up stdin/stdout pipes for communication.
// Optional configuration functions can be provided to customize the transport before it starts,
// such as setting a custom command function.
//
// NOTICE: NewStdioMCPClientWithOptions starts the client, so the returned
// client is ready to Initialize and receives the server's notifications.
// This is for backward compatibility.
func NewStdioMCPClientWithOptions(
	command string,
	env []string,
	args []string,
	opts ...transport.StdioOption,
) (*Client, error) {
	stdioTransport := transport.NewStdioWithOptions(command, env, args, opts...)

	// The client, not the transport alone, is started: Start also installs
	// the notification and request handlers, without which nothing the
	// server sends reaches the handlers registered with OnNotification.
	c := NewClient(stdioTransport)
	if err := c.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to start stdio transport: %w", err)
	}

	return c, nil
}

// GetStderr returns a reader for the stderr output of the subprocess.
// This can be used to capture error messages or logs from the subprocess.
//
// The stdio transport drains the subprocess's stderr into a bounded buffer so
// the OS pipe never fills up and deadlocks the child. The returned reader
// yields the most recent output; older output is dropped once the buffer is
// full. To capture the full, real-time stream, use
// transport.WithCommandStderrWriter instead.
//
// It works with any transport that exposes a Stderr() io.Reader method,
// including *transport.Stdio and *transport.CommandTransport.
func GetStderr(c *Client) (io.Reader, bool) {
	t := c.GetTransport()

	type stderrer interface {
		Stderr() io.Reader
	}
	if s, ok := t.(stderrer); ok {
		return s.Stderr(), true
	}
	return nil, false
}
