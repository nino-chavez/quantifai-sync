package credentials

import (
	"errors"
	"testing"
)

// fakeKeyring is an in-memory keyring for testing.
type fakeKeyring struct {
	store map[string]string
}

func newFakeKeyring() *fakeKeyring {
	return &fakeKeyring{store: make(map[string]string)}
}

func (f *fakeKeyring) Get(service, account string) (string, error) {
	key := service + "/" + account
	if v, ok := f.store[key]; ok {
		return v, nil
	}
	return "", errors.New("not found")
}

func (f *fakeKeyring) Set(service, account, password string) error {
	f.store[service+"/"+account] = password
	return nil
}

func (f *fakeKeyring) Delete(service, account string) error {
	delete(f.store, service+"/"+account)
	return nil
}

// failingKeyring always fails, simulating a headless environment.
type failingKeyring struct{}

func (f *failingKeyring) Get(_, _ string) (string, error)        { return "", errors.New("no keyring") }
func (f *failingKeyring) Set(_, _, _ string) error                { return errors.New("no keyring") }
func (f *failingKeyring) Delete(_, _ string) error                { return errors.New("no keyring") }

// TestCredentialPriorityChain verifies the priority: keyring -> config file -> env var.
func TestCredentialPriorityChain(t *testing.T) {
	t.Run("keyring wins over config and env", func(t *testing.T) {
		kr := newFakeKeyring()
		kr.Set(serviceName(), accountName, "keyring-key")

		t.Setenv("AI_OPS_API_KEY", "env-key")
		m := NewManager(kr, "config-key")

		got, err := m.RetrieveAPIKey()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "keyring-key" {
			t.Errorf("got %q, want %q", got, "keyring-key")
		}
	})

	t.Run("config wins over env when keyring fails", func(t *testing.T) {
		t.Setenv("AI_OPS_API_KEY", "env-key")
		m := NewManager(&failingKeyring{}, "config-key")

		got, err := m.RetrieveAPIKey()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "config-key" {
			t.Errorf("got %q, want %q", got, "config-key")
		}
	})

	t.Run("env used when keyring and config unavailable", func(t *testing.T) {
		t.Setenv("AI_OPS_API_KEY", "env-key")
		m := NewManager(nil, "")

		got, err := m.RetrieveAPIKey()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "env-key" {
			t.Errorf("got %q, want %q", got, "env-key")
		}
	})

	t.Run("no key anywhere returns error", func(t *testing.T) {
		t.Setenv("AI_OPS_API_KEY", "")
		m := NewManager(nil, "")

		_, err := m.RetrieveAPIKey()
		if !errors.Is(err, ErrNoAPIKey) {
			t.Errorf("expected ErrNoAPIKey, got %v", err)
		}
	})
}

// TestResolveAPIKeyReportsSource covers the agent's startup lookup: the
// source name it logs must match where the key actually came from.
func TestResolveAPIKeyReportsSource(t *testing.T) {
	t.Setenv("QUANTIFAI_API_KEY", "")
	t.Setenv("AI_OPS_API_KEY", "")

	kr := newFakeKeyring()
	kr.Set(serviceName(), accountName, "keyring-key")
	if key, src, err := NewManager(kr, "config-key").ResolveAPIKey(); err != nil || key != "keyring-key" || src != SourceKeyring {
		t.Errorf("keyring: got (%q, %q, %v)", key, src, err)
	}
	if key, src, err := NewManager(&failingKeyring{}, "config-key").ResolveAPIKey(); err != nil || key != "config-key" || src != SourceConfig {
		t.Errorf("config: got (%q, %q, %v)", key, src, err)
	}
	t.Setenv("QUANTIFAI_API_KEY", "env-key")
	if key, src, err := NewManager(nil, "").ResolveAPIKey(); err != nil || key != "env-key" || src != SourceEnv {
		t.Errorf("env: got (%q, %q, %v)", key, src, err)
	}
	t.Setenv("QUANTIFAI_API_KEY", "")
	if _, _, err := NewManager(&failingKeyring{}, "").ResolveAPIKey(); !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("none: got %v", err)
	}
}
