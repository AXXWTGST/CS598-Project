package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"cs598/tagindex/internal/index"
)

type TagEvent struct {
	Ts           int64    `json:"ts"`
	Source       string   `json:"source,omitempty"`
	Op           string   `json:"op"`
	Path         string   `json:"path"`
	Tags         []string `json:"tags,omitempty"`
	IndexUpdated bool     `json:"index_updated"`
}

type ReplayResult struct {
	StartOffset int64
	EndOffset   int64
	Applied     int
	Skipped     int
}

func AppendTagEvent(logPath string, event TagEvent) error {
	if logPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	unlock, err := lockEventLog(logPath)
	if err != nil {
		return err
	}
	defer unlock()

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(event); err != nil {
		return err
	}
	return file.Sync()
}

func ReplayTagEvents(logPath, checkpointPath, indexRoot string) (*ReplayResult, error) {
	startOffset, err := ReadCheckpoint(checkpointPath)
	if err != nil {
		return nil, err
	}
	result := &ReplayResult{StartOffset: startOffset, EndOffset: startOffset}

	unlock, err := lockEventLog(logPath)
	if err != nil {
		return nil, err
	}
	defer unlock()

	data, err := os.ReadFile(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	if startOffset > int64(len(data)) {
		startOffset = 0
		result.StartOffset = 0
	}

	store := index.New(indexRoot)
	reader := bufio.NewReader(bytes.NewReader(data[startOffset:]))
	offset := startOffset
	needsRewrite := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		offset += int64(len(line))
		line = strings.TrimSpace(line)
		if line == "" {
			if err := WriteCheckpoint(checkpointPath, offset); err != nil {
				return nil, err
			}
			continue
		}

		var event TagEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("parse event at offset %d: %w", offset-int64(len(line)), err)
		}
		if event.Path == "" {
			return nil, fmt.Errorf("event at offset %d has empty path", offset-int64(len(line)))
		}

		if event.IndexUpdated {
			result.Skipped++
		} else {
			switch strings.ToLower(event.Op) {
			case "set":
				if err := store.Set(event.Path, event.Tags); err != nil {
					return nil, err
				}
			case "delete_all":
				if err := store.Set(event.Path, nil); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("unsupported event op %q at offset %d", event.Op, offset-int64(len(line)))
			}
			result.Applied++
			needsRewrite = true
		}

		result.EndOffset = offset
		if err := WriteCheckpoint(checkpointPath, offset); err != nil {
			return nil, err
		}
	}
	if needsRewrite {
		if err := markEventsUpdated(logPath, data, result.EndOffset); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func ReadCheckpoint(path string) (int64, error) {
	if path == "" {
		return 0, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return 0, nil
	}
	offset, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse checkpoint %s: %w", path, err)
	}
	return offset, nil
}

func WriteCheckpoint(path string, offset int64) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(offset, 10)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func markEventsUpdated(logPath string, data []byte, throughOffset int64) error {
	var out bytes.Buffer
	reader := bufio.NewReader(bytes.NewReader(data))
	offset := int64(0)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				if line != "" {
					out.WriteString(line)
				}
				break
			}
			return err
		}

		nextOffset := offset + int64(len(line))
		if strings.TrimSpace(line) != "" && nextOffset <= throughOffset {
			var event TagEvent
			if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &event); err != nil {
				return fmt.Errorf("parse event at offset %d while marking updated: %w", offset, err)
			}
			event.IndexUpdated = true
			encoded, err := json.Marshal(event)
			if err != nil {
				return err
			}
			out.Write(encoded)
			out.WriteByte('\n')
		} else {
			out.WriteString(line)
		}
		offset = nextOffset
	}

	tmp, err := os.CreateTemp(filepath.Dir(logPath), filepath.Base(logPath)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(out.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, logPath); err != nil {
		return err
	}
	removeTmp = false
	return nil
}

func lockEventLog(logPath string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	lockPath := logPath + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
