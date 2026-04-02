package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type State struct {
	LastConfigVersion int64 `json:"last_config_version"`
}

type FileStore struct {
	path string
	mu   sync.Mutex
}

func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

func (s *FileStore) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var state State
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("read state file: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("parse state file: %w", err)
	}
	return state, nil
}

func (s *FileStore) Save(state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state file: %w", err)
	}
	return writeAtomically(s.path, data, 0o644)
}

func writeAtomically(path string, data []byte, mode os.FileMode) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Chmod(mode); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
