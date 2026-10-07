//go:build darwin

package tray

import (
	"strings"
	"testing"
)

func TestStatusTitle(t *testing.T) {
	long := "no API key: run `quantifai-sync install --api-key <key>`, or set api_key in ~/.config/quantifai/config.toml"
	tests := []struct {
		h    healthResponse
		want string
	}{
		{healthResponse{Status: "ok"}, "Quantifai Sync — Running"},
		{healthResponse{Status: "degraded"}, "Quantifai Sync — Degraded"},
		{healthResponse{Status: "error"}, "Quantifai Sync — Error"},
		{healthResponse{Status: "error", Problem: "sync_enabled is false"}, "Quantifai Sync — Waiting: sync_enabled is false"},
	}
	for _, tt := range tests {
		if got := statusTitle(&tt.h); got != tt.want {
			t.Errorf("statusTitle(%+v) = %q, want %q", tt.h, got, tt.want)
		}
	}
	got := statusTitle(&healthResponse{Status: "error", Problem: long})
	if !strings.HasPrefix(got, "Quantifai Sync — Waiting: no API key") || !strings.HasSuffix(got, "…") || len([]rune(got)) > 90 {
		t.Errorf("long problem title %q, want it shortened with …", got)
	}
}
