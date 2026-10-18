package mpv

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Dialer establishes a fresh Conn per exchange. One connection per command
// mirrors plaza-player's request() model; mpv handles the churn fine.
type Dialer interface {
	Dial(ctx context.Context) (Conn, error)
}

// Client exercises an IPC endpoint with a fresh connection per command.
type Client struct {
	dialer Dialer
}

// NewClient binds a one-shot IPC client to a dialer.
func NewClient(dialer Dialer) *Client {
	return &Client{dialer: dialer}
}

// Call issues a command with the default command timeout.
func (c *Client) Call(ctx context.Context, command []any) (json.RawMessage, error) {
	return c.CallTimeout(ctx, command, DefaultCommandTimeout)
}

// CallTimeout issues a command with an explicit response deadline.
func (c *Client) CallTimeout(ctx context.Context, command []any, timeout time.Duration) (json.RawMessage, error) {
	conn, err := c.dialer.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	return conn.Call(ctx, command, timeout)
}

// GetProperty reads one property and returns its raw JSON payload.
func (c *Client) GetProperty(ctx context.Context, name string) (json.RawMessage, error) {
	return c.Call(ctx, []any{"get_property", name})
}

// SetProperty writes one property.
func (c *Client) SetProperty(ctx context.Context, name string, value any) error {
	_, err := c.Call(ctx, []any{"set_property", name, value})
	return err
}

// PropBool decodes a boolean property; false with no error is also the
// caller-side convention for "absent/invalid" via probe helpers.
func PropBool(data json.RawMessage) (bool, error) {
	var b bool
	if err := json.Unmarshal(data, &b); err != nil {
		return false, err
	}
	return b, nil
}

// PropFloat decodes a numeric property.
func PropFloat(data json.RawMessage) (float64, error) {
	var f float64
	if err := json.Unmarshal(data, &f); err != nil {
		return 0, err
	}
	return f, nil
}

// PropInt rounds a numeric property; mpv JSON numbers arrive as float64.
func PropInt(data json.RawMessage) (int, error) {
	f, err := PropFloat(data)
	if err != nil {
		return 0, err
	}
	return int(math.Round(f)), nil
}

// PropString decodes a string property.
func PropString(data json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return "", err
	}
	return s, nil
}

// Device is one entry of mpv's audio-device-list property.
// "auto" is always present and means system-default output.
type Device struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PropDevices decodes the audio-device-list payload.
func PropDevices(data json.RawMessage) ([]Device, error) {
	var devices []Device
	if err := json.Unmarshal(data, &devices); err != nil {
		return nil, err
	}
	return devices, nil
}
