// Package state 记录已经发送过的文件版本，避免重启后重复推送。
//
// 文件格式（与 Python 版一致）：
//
//	{"version":1,"files":{"<绝对路径>":[<mtime 纳秒>,<大小>]}}
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

const (
	maxEntries = 20000
	pruneTo    = 10000
)

type entry struct {
	MTime int64 `json:"mtime"`
	Size  int64 `json:"size"`
}

type fileFormat struct {
	Version int                `json:"version"`
	Files   map[string][]int64 `json:"files"`
}

// Store 是并发安全的发送记录。
type Store struct {
	path    string
	mu      sync.Mutex
	entries map[string]entry
	dirty   bool
}

// Open 载入状态文件；path 为空表示不持久化。
func Open(path string) (*Store, error) {
	store := &Store{path: path, entries: map[string]entry{}}
	if path == "" {
		return store, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取状态文件失败：%w", err)
	}
	var parsed fileFormat
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("解析状态文件失败：%w", err)
	}
	for path, values := range parsed.Files {
		if len(values) == 2 {
			store.entries[path] = entry{MTime: values[0], Size: values[1]}
		}
	}
	return store, nil
}

// Sent 判断该文件的这个版本是否已经发送过。
func (s *Store) Sent(path string, mtime, size int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	found, ok := s.entries[path]
	return ok && found.MTime == mtime && found.Size == size
}

// Mark 记录发送成功。
func (s *Store) Mark(path string, mtime, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, path)
	s.entries[path] = entry{MTime: mtime, Size: size}
	s.dirty = true
	if len(s.entries) > maxEntries {
		for key := range s.entries {
			delete(s.entries, key)
			if len(s.entries) <= pruneTo {
				break
			}
		}
	}
}

// Save 原子写回磁盘。
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" || !s.dirty {
		return nil
	}
	files := make(map[string][]int64, len(s.entries))
	for path, value := range s.entries {
		files[path] = []int64{value.MTime, value.Size}
	}
	payload, err := json.Marshal(fileFormat{Version: 1, Files: files})
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建状态目录失败：%w", err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return fmt.Errorf("写入状态文件失败：%w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("替换状态文件失败：%w", err)
	}
	s.dirty = false
	return nil
}

// Len 返回记录条数（测试用）。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
