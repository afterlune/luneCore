package mcp

import (
	"errors"
	"sync"

	"github.com/go-keyring/keyring"
)

// CredentialStorage abstracts persistence for OAuth tokens and credentials.
type CredentialStorage interface {
	Get(service, account string) ([]byte, error)
	Set(service, account string, data []byte) error
	Delete(service, account string) error
}

var ErrCredentialNotFound = errors.New("credential not found")

// KeyringStorage persists credentials in the operating system keyring.
type KeyringStorage struct{}

func (KeyringStorage) Get(service, account string) ([]byte, error) {
	data, err := keyring.Get(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrCredentialNotFound
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (KeyringStorage) Set(service, account string, data []byte) error {
	return keyring.Set(service, account, data)
}

func (KeyringStorage) Delete(service, account string) error {
	err := keyring.Delete(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrCredentialNotFound
	}
	return err
}

// MemoryCredentialStorage stores credentials in-memory for testing, ephemeral or headless use.
type MemoryCredentialStorage struct {
	mu    sync.RWMutex
	items map[string][]byte
}

func NewMemoryCredentialStorage() *MemoryCredentialStorage {
	return &MemoryCredentialStorage{items: make(map[string][]byte)}
}

func (m *MemoryCredentialStorage) key(service, account string) string {
	return service + "\x00" + account
}

func (m *MemoryCredentialStorage) Get(service, account string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	val, ok := m.items[m.key(service, account)]
	if !ok {
		return nil, ErrCredentialNotFound
	}
	return append([]byte(nil), val...), nil
}

func (m *MemoryCredentialStorage) Set(service, account string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.items == nil {
		m.items = make(map[string][]byte)
	}
	m.items[m.key(service, account)] = append([]byte(nil), data...)
	return nil
}

func (m *MemoryCredentialStorage) Delete(service, account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(service, account)
	if _, ok := m.items[k]; !ok {
		return ErrCredentialNotFound
	}
	delete(m.items, k)
	return nil
}
