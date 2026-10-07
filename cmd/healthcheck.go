package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// healthResponse matches the JSON schema returned by GET /healthz.
type healthResponse struct {
	Status         string `json:"status"`
	Version        string `json:"version"`
	UptimeSeconds  int    `json:"uptime_seconds"`
	LastSyncTime   string `json:"last_sync_time"`
	FilesTracked   int    `json:"files_tracked"`
	RecordsBufferd int    `json:"records_buffered"`
	ErrorsLastHour int    `json:"errors_last_hour"`
	Problem        string `json:"problem,omitempty"`
}

// RunHealthcheck hits the local /healthz endpoint and returns exit code
// 0 when the status is "ok", or 1 for any other status or connection error.
// The port parameter specifies the health server port (default 19876).
func RunHealthcheck(port int) int {
	return runHealthcheckWithClient(&http.Client{Timeout: 5 * time.Second}, port)
}

// runHealthcheckWithClient allows tests to inject a custom http.Client.
func runHealthcheckWithClient(client *http.Client, port int) int {
	return runHealthcheck(client, port, os.Stdout)
}

// runHealthcheck checks the endpoint and writes its report to out.
func runHealthcheck(client *http.Client, port int, out io.Writer) int {
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)

	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(out, "healthcheck failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(out, "healthcheck failed: could not read response: %v\n", err)
		return 1
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(out, "healthcheck failed: HTTP %d\n", resp.StatusCode)
		return 1
	}

	var hr healthResponse
	if err := json.Unmarshal(body, &hr); err != nil {
		fmt.Fprintf(out, "healthcheck failed: invalid JSON: %v\n", err)
		return 1
	}

	fmt.Fprintf(out, "status: %s, version: %s, uptime: %ds\n", hr.Status, hr.Version, hr.UptimeSeconds)
	if hr.Problem != "" {
		fmt.Fprintf(out, "problem: %s\n", hr.Problem)
	}

	if hr.Status == "ok" {
		return 0
	}
	return 1
}
