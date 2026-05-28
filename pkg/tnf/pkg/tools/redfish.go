package tools

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type redfishUUIDGetter func(ctx context.Context, address, username, password string, insecure bool) (string, error)

var defaultRedfishUUIDGetter redfishUUIDGetter = getRedfishSystemUUID

type redfishSystemResponse struct {
	UUID string `json:"UUID"`
}

func getRedfishSystemUUID(ctx context.Context, address, username, password string, insecure bool) (string, error) {
	transport := &http.Transport{}
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request for %s: %w", address, err)
	}
	req.SetBasicAuth(username, password)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to query Redfish at %s: %w", address, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Redfish at %s returned status %d", address, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read Redfish response from %s: %w", address, err)
	}

	var system redfishSystemResponse
	if err := json.Unmarshal(body, &system); err != nil {
		return "", fmt.Errorf("failed to parse Redfish response from %s: %w", address, err)
	}

	if system.UUID == "" {
		return "", fmt.Errorf("Redfish at %s returned empty UUID", address)
	}

	return system.UUID, nil
}

// parseRedfishURL strips the "redfish+" prefix from a fencing secret address
// and returns the clean URL for direct HTTP access.
func parseRedfishURL(address string) (string, error) {
	idx := strings.Index(address, "://")
	if idx == -1 {
		return "", fmt.Errorf("invalid Redfish address %q: no scheme found", address)
	}
	scheme := address[:idx]
	rest := address[idx:]

	if strings.HasPrefix(scheme, "redfish+") {
		return strings.TrimPrefix(scheme, "redfish+") + rest, nil
	}
	if scheme == "https" || scheme == "http" {
		return address, nil
	}
	return "", fmt.Errorf("unsupported scheme in Redfish address %q", address)
}
