package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Request bodies, as defined in src/firecracker/swagger/firecracker.yaml of
// the pinned Firecracker release. Only the fields Forkbox sets are listed.

type machineConfig struct {
	VCPUCount       int  `json:"vcpu_count"`
	MemSizeMiB      int  `json:"mem_size_mib"`
	SMT             bool `json:"smt"`
	TrackDirtyPages bool `json:"track_dirty_pages"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type vsockDev struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type netIface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

type action struct {
	ActionType string `json:"action_type"`
}

type vmState struct {
	State string `json:"state"` // Paused or Resumed
}

type snapshotCreate struct {
	SnapshotType string `json:"snapshot_type"` // Full or Diff
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
	// SyncSnapshotFiles (default true) fsyncs the files before answering.
	SyncSnapshotFiles *bool `json:"sync_snapshot_files,omitempty"`
}

type memBackend struct {
	BackendType string `json:"backend_type"` // File or Uffd
	BackendPath string `json:"backend_path"`
}

type snapshotLoad struct {
	SnapshotPath    string     `json:"snapshot_path"`
	MemBackend      memBackend `json:"mem_backend"`
	TrackDirtyPages bool       `json:"track_dirty_pages"`
	ResumeVM        bool       `json:"resume_vm"`
}

type instanceInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"` // Not started, Running or Paused
	VMMVersion string `json:"vmm_version"`
}

type apiError struct {
	FaultMessage string `json:"fault_message"`
}

// APIError is a non-2xx answer from the Firecracker API.
type APIError struct {
	Method, Path string
	Status       int
	Fault        string // Firecracker's fault_message
}

func (e *APIError) Error() string {
	return fmt.Sprintf("firecracker %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Fault)
}

// ErrAPITimeout means a Firecracker API call did not answer in time.
var ErrAPITimeout = errors.New("firecracker API call timed out")

// client talks to one Firecracker process over its API socket. Every call
// has a deadline.
type client struct {
	http *http.Client
}

func newClient(socketPath string) *client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		DisableKeepAlives: true,
	}
	return &client{http: &http.Client{Transport: tr}}
}

func (c *client) close() { c.http.CloseIdleConnections() }

// do sends one request and decodes a JSON answer into out (when non-nil).
func (c *client) do(ctx context.Context, timeout time.Duration, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %s %s after %s", ErrAPITimeout, method, path, timeout)
		}
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %s %s after %s", ErrAPITimeout, method, path, timeout)
		}
		return fmt.Errorf("firecracker %s %s: reading answer: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var ae apiError
		_ = json.Unmarshal(data, &ae)
		if ae.FaultMessage == "" {
			ae.FaultMessage = string(bytes.TrimSpace(data))
		}
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Fault: ae.FaultMessage}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("firecracker %s %s: decoding answer: %w", method, path, err)
		}
	}
	return nil
}
