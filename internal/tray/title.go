//go:build darwin

package tray

import "fmt"

// statusTitle is the tray's status line for a health response. An agent
// waiting on a config problem reports it, so the tray names it rather
// than a bare "Error".
func statusTitle(h *healthResponse) string {
	if h.Problem != "" {
		p := h.Problem
		if r := []rune(p); len(r) > 60 {
			p = string(r[:60]) + "…"
		}
		return "Quantifai Sync — Waiting: " + p
	}
	label := "Running"
	switch h.Status {
	case "degraded":
		label = "Degraded"
	case "error":
		label = "Error"
	}
	return fmt.Sprintf("Quantifai Sync — %s", label)
}
