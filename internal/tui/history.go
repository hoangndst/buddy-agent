package tui

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const inputHistoryLimit = 500

// HistoryEntry is one persisted prompt history record.
type HistoryEntry struct {
	Prompt    string `json:"prompt"`
	Timestamp int64  `json:"timestamp"`
}

// InputHistoryRing stores and navigates recent prompts.
type InputHistoryRing struct {
	path    string
	entries []HistoryEntry
	index   int
	draft   string
}

// NewInputHistoryRing loads prompt history from path.
func NewInputHistoryRing(path string) (*InputHistoryRing, error) {
	if path == "" {
		return nil, errors.New("prompt history path is required")
	}
	h := &InputHistoryRing{path: path}
	entries, err := readHistory(path)
	if err != nil {
		return nil, err
	}
	h.entries = entries
	h.index = len(h.entries)
	return h, nil
}

func readHistory(path string) ([]HistoryEntry, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []HistoryEntry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 2*1024*1024)
	for scanner.Scan() {
		var entry HistoryEntry
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.Prompt != "" {
			entries = append(entries, entry)
			if len(entries) > inputHistoryLimit {
				entries = entries[1:]
			}
		}
	}
	scanErr := scanner.Err()
	closeErr := file.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return entries, nil
}

// Add appends a prompt to the persistent history.
func (h *InputHistoryRing) Add(prompt string) error {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(h.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := acquireHistoryFileLock(lock); err != nil {
		_ = lock.Close()
		return err
	}
	defer func() {
		_ = releaseHistoryFileLock(lock)
		_ = lock.Close()
	}()
	entries, err := readHistory(h.path)
	if err != nil {
		return err
	}
	h.entries = entries
	if len(h.entries) > 0 && h.entries[len(h.entries)-1].Prompt == prompt {
		return nil
	}
	entry := HistoryEntry{Prompt: prompt, Timestamp: time.Now().Unix()}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if len(h.entries) == inputHistoryLimit {
		file, err := os.CreateTemp(filepath.Dir(h.path), ".history-*.jsonl")
		if err != nil {
			return err
		}
		for _, old := range h.entries[1:] {
			line, err := json.Marshal(old)
			if err == nil {
				_, err = file.Write(append(line, '\n'))
			}
			if err != nil {
				_ = file.Close()
				_ = os.Remove(file.Name())
				return err
			}
		}
		if _, err = file.Write(append(data, '\n')); err != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
			return err
		}
		if err = file.Sync(); err != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
			return err
		}
		if err = file.Close(); err != nil {
			_ = os.Remove(file.Name())
			return err
		}
		if err = os.Rename(file.Name(), h.path); err != nil {
			_ = os.Remove(file.Name())
			return err
		}
	} else {
		file, err := os.OpenFile(h.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(append(data, '\n'))
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	h.entries = append(h.entries, entry)
	if len(h.entries) > inputHistoryLimit {
		h.entries = h.entries[1:]
	}
	h.index = len(h.entries)
	return nil
}

// Previous moves to the previous prompt and saves the current draft.
func (h *InputHistoryRing) Previous(current string) (string, bool) {
	if len(h.entries) == 0 || h.index == 0 {
		return "", false
	}
	if h.index == len(h.entries) {
		h.draft = current
	}
	h.index--
	return h.entries[h.index].Prompt, true
}

// Next moves forward through history, restoring the saved draft at the end.
func (h *InputHistoryRing) Next() (string, bool) {
	if h.index >= len(h.entries) {
		return "", false
	}
	h.index++
	if h.index == len(h.entries) {
		return h.draft, true
	}
	return h.entries[h.index].Prompt, true
}

// Reset returns navigation to the end of history.
func (h *InputHistoryRing) Reset() {
	h.index = len(h.entries)
	h.draft = ""
}

// AtEnd reports whether navigation is positioned after the newest entry.
func (h *InputHistoryRing) AtEnd() bool {
	return h.index == len(h.entries)
}
