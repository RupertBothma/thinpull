// Package tui provides Terminal User Interface components for the Fly.io Image Manager.
package tui

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	fsmv1 "github.com/superfly/fsm/gen/fsm/v1"
	"github.com/superfly/fsm/gen/fsm/v1/fsmv1connect"
)

// AdminClient provides access to the FSM admin interface via Unix socket.
type AdminClient struct {
	client     fsmv1connect.FSMServiceClient
	socketPath string
}

// NewAdminClient creates a new admin client connected to the FSM Unix socket.
// The socket is located at <fsmDBPath>/fsm.sock.
func NewAdminClient(fsmDBPath string) (*AdminClient, error) {
	socketPath := filepath.Join(fsmDBPath, "fsm.sock")

	// Create HTTP client that connects via Unix socket
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}

	// Create Connect client - use dummy URL since we're using Unix socket
	client := fsmv1connect.NewFSMServiceClient(httpClient, "http://localhost")

	return &AdminClient{
		client:     client,
		socketPath: socketPath,
	}, nil
}

// SocketPath returns the path to the Unix socket.
func (c *AdminClient) SocketPath() string {
	return c.socketPath
}

// ListRegistered returns all registered FSM definitions.
func (c *AdminClient) ListRegistered(ctx context.Context) ([]*fsmv1.FSM, error) {
	resp, err := c.client.ListRegistered(ctx, connect.NewRequest(&fsmv1.ListRegisteredRequest{}))
	if err != nil {
		return nil, fmt.Errorf("failed to list registered FSMs: %w", err)
	}
	return resp.Msg.GetFsms(), nil
}

// ListActive returns all active (non-complete) FSM runs.
func (c *AdminClient) ListActive(ctx context.Context) ([]*fsmv1.ActiveFSM, error) {
	resp, err := c.client.ListActive(ctx, connect.NewRequest(&fsmv1.ListActiveRequest{}))
	if err != nil {
		return nil, fmt.Errorf("failed to list active FSMs: %w", err)
	}
	return resp.Msg.GetActive(), nil
}

// GetHistoryEvent retrieves the history event for a specific run version.
func (c *AdminClient) GetHistoryEvent(ctx context.Context, runVersion string) (*fsmv1.HistoryEvent, error) {
	resp, err := c.client.GetHistoryEvent(ctx, connect.NewRequest(&fsmv1.GetHistoryEventRequest{
		RunVersion: runVersion,
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to get history event: %w", err)
	}
	return resp.Msg, nil
}

// IsAvailable checks if the FSM admin socket is available.
func (c *AdminClient) IsAvailable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, err := c.ListRegistered(ctx)
	return err == nil
}

// ActiveFSMToRun converts a protobuf ActiveFSM to a TUI FSMRun.
func ActiveFSMToRun(active *fsmv1.ActiveFSM) FSMRun {
	// Map RunState to string
	var state string
	switch active.GetRunState() {
	case fsmv1.RunState_RUN_STATE_PENDING:
		state = "pending"
	case fsmv1.RunState_RUN_STATE_RUNNING:
		state = "running"
	case fsmv1.RunState_RUN_STATE_COMPLETE:
		state = "completed"
	default:
		state = "unknown"
	}

	// Extract image ID from the FSM ID (format: img_<hash>)
	imageID := active.GetId()

	return FSMRun{
		ID:          active.GetId(),
		Type:        active.GetAction(),
		ImageID:     imageID,
		State:       state,
		CurrentStep: active.GetCurrentState(),
		Error:       active.GetError(),
	}
}
