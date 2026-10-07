// Package credentials provides API key storage via the OS keyring with
// fallback to config file and environment variable.
//
// Priority chain (first available wins):
//  1. go-keyring (macOS Keychain, Windows Credential Manager, Linux Secret Service)
//  2. Config file api_key value
//  3. QUANTIFAI_API_KEY / AI_OPS_API_KEY environment variable
//
// The API key is never logged, even at debug level.
package credentials

import (
	"errors"
	"os"
)

const (
	// defaultServiceName is the default keyring service identifier.
	defaultServiceName = "quantifai-sync"
	// legacyServiceName is the old keyring service identifier for migration.
	legacyServiceName = "ai-ops-shipper"
	// accountName is the keyring account identifier.
	accountName = "api-key"
)

// serviceName returns the keyring service name, checking QUANTIFAI_KEYRING_SERVICE
// first, then AI_OPS_KEYRING_SERVICE for backwards compatibility.
func serviceName() string {
	if v := os.Getenv("QUANTIFAI_KEYRING_SERVICE"); v != "" {
		return v
	}
	if v := os.Getenv("AI_OPS_KEYRING_SERVICE"); v != "" {
		return v
	}
	return defaultServiceName
}

// ErrNoAPIKey is returned when no API key can be found in any source.
var ErrNoAPIKey = errors.New("credentials: no API key found in keyring, config, or environment")

// KeyringProvider abstracts OS keyring operations so the credential
// manager can be tested without a real keyring.
type KeyringProvider interface {
	Get(service, account string) (string, error)
	Set(service, account, password string) error
	Delete(service, account string) error
}

// Manager resolves and stores API keys using the priority chain.
type Manager struct {
	keyring      KeyringProvider
	configAPIKey string // api_key from config file (already resolved by config loader)
}

// NewManager creates a credential Manager.  Pass nil for keyring when
// no OS keyring is available (headless/CI environments).  configAPIKey
// is the api_key value loaded from the TOML config file.
func NewManager(keyring KeyringProvider, configAPIKey string) *Manager {
	return &Manager{
		keyring:      keyring,
		configAPIKey: configAPIKey,
	}
}

// Key sources reported by ResolveAPIKey.
const (
	SourceKeyring = "keyring"
	SourceConfig  = "config"
	SourceEnv     = "environment"
)

// RetrieveAPIKey returns the API key using the priority chain:
// keyring -> config file -> environment variable.
// Returns ErrNoAPIKey if no key is available from any source.
func (m *Manager) RetrieveAPIKey() (string, error) {
	key, _, err := m.ResolveAPIKey()
	return key, err
}

// ResolveAPIKey is RetrieveAPIKey plus the name of the source the key came
// from, so callers can log where a key was found without logging the key.
// The config loader already folds QUANTIFAI_API_KEY into the config value,
// so SourceConfig can also mean "set by the environment".
func (m *Manager) ResolveAPIKey() (key, source string, err error) {
	// Priority 1: OS keyring
	if m.keyring != nil {
		key, err := m.keyring.Get(serviceName(), accountName)
		if err == nil && key != "" {
			return key, SourceKeyring, nil
		}
	}

	// Priority 2: config file value
	if m.configAPIKey != "" {
		return m.configAPIKey, SourceConfig, nil
	}

	// Priority 3: environment variable (new name first, legacy fallback)
	if key := os.Getenv("QUANTIFAI_API_KEY"); key != "" {
		return key, SourceEnv, nil
	}
	if key := os.Getenv("AI_OPS_API_KEY"); key != "" {
		return key, SourceEnv, nil
	}

	return "", "", ErrNoAPIKey
}

// StoreAPIKey persists the API key in the OS keyring.  Returns an error
// if no keyring provider is available.
func (m *Manager) StoreAPIKey(key string) error {
	if m.keyring == nil {
		return errors.New("credentials: no keyring provider available")
	}
	return m.keyring.Set(serviceName(), accountName, key)
}

// DeleteAPIKey removes the API key from the OS keyring.
func (m *Manager) DeleteAPIKey() error {
	if m.keyring == nil {
		return errors.New("credentials: no keyring provider available")
	}
	return m.keyring.Delete(serviceName(), accountName)
}

// MigrateKeyring copies the API key from the legacy keyring service name
// to the new one, then deletes the old entry.  This is a best-effort
// operation: errors are returned but callers may choose to ignore them
// (e.g. when the old key simply doesn't exist).
func (m *Manager) MigrateKeyring() error {
	if m.keyring == nil {
		return nil
	}
	key, err := m.keyring.Get(legacyServiceName, accountName)
	if err != nil || key == "" {
		return nil // nothing to migrate
	}
	if err := m.keyring.Set(serviceName(), accountName, key); err != nil {
		return err
	}
	_ = m.keyring.Delete(legacyServiceName, accountName) // best-effort cleanup
	return nil
}
