// Package store 提供 StormOps 的状态持久化。
//
// 实现要点：
//  1. 读写锁保护内存状态，请求路径不加磁盘 IO；
//  2. 后台协程每 3 秒把脏状态落盘一次，落盘用 "写临时文件 + rename" 保证原子性，
//     避免进程被 kill 时留下半个 JSON 导致下次启动直接崩；
//  3. 启动时如果主文件损坏，自动回退到 .bak 备份，仍然失败则用空状态启动并记录告警。
//
// 生产环境切换到 PostgreSQL 只需要替换本文件里的 Load/Save，
// 领域模型和上层逻辑完全不用改，接口边界见 docs/decisions/ADR-001。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Ricevire/stormops/internal/model"
)

// Store 是带持久化的状态容器。
type Store struct {
	mu    sync.RWMutex
	path  string
	state model.State
	dirty bool
	stop  chan struct{}
	once  sync.Once
}

// Open 打开（或创建）一个状态文件。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	s := &Store{path: path, stop: make(chan struct{})}
	s.state = emptyState()

	if err := s.load(); err != nil {
		// 主文件坏了就试备份，都不行就从空状态起步，绝不阻塞启动。
		bak := path + ".bak"
		if _, statErr := os.Stat(bak); statErr == nil {
			if e2 := s.loadFrom(bak); e2 == nil {
				s.dirty = true
				go s.flusher()
				return s, nil
			}
		}
		s.state = emptyState()
		s.state.Meta["bootWarning"] = "状态文件损坏，已使用空状态启动：" + err.Error()
	}

	go s.flusher()
	return s, nil
}

func emptyState() model.State {
	return model.State{
		Series:   map[string][]model.Sample{},
		Capacity: map[string][]model.CapPoint{},
		Meta:     map[string]string{},
	}
}

func (s *Store) load() error {
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		s.dirty = true
		return nil
	}
	return s.loadFrom(s.path)
}

func (s *Store) loadFrom(p string) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var st model.State
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	if st.Series == nil {
		st.Series = map[string][]model.Sample{}
	}
	if st.Capacity == nil {
		st.Capacity = map[string][]model.CapPoint{}
	}
	if st.Meta == nil {
		st.Meta = map[string]string{}
	}
	s.state = st
	return nil
}

// View 在只读锁内执行 fn。fn 内不得调用 Update，否则死锁。
func (s *Store) View(fn func(*model.State)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&s.state)
}

// Update 在写锁内执行 fn，并标记为脏等待落盘。
func (s *Store) Update(fn func(*model.State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
	s.state.UpdatedAt = time.Now().Unix()
	s.dirty = true
}

func (s *Store) flusher() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.mu.Lock()
			dirty := s.dirty
			var payload []byte
			if dirty {
				payload, _ = json.Marshal(&s.state)
				s.dirty = false
			}
			s.mu.Unlock()
			if dirty {
				if err := s.writeAtomic(payload); err != nil {
					s.mu.Lock()
					s.dirty = true
					s.mu.Unlock()
					fmt.Fprintf(os.Stderr, "[store] 落盘失败: %v\n", err)
				}
			}
		case <-s.stop:
			return
		}
	}
}

func (s *Store) writeAtomic(payload []byte) error {
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	// 每次成功覆盖前，先把上一份留成 .bak
	if _, err := os.Stat(s.path); err == nil {
		_ = os.Rename(s.path, s.path+".bak")
	}
	return os.Rename(tmp, s.path)
}

// Flush 立即落盘，用于优雅退出。
func (s *Store) Flush() error {
	s.mu.Lock()
	payload, err := json.Marshal(&s.state)
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.writeAtomic(payload)
}

// Close 停止后台协程并做最后一次落盘。
func (s *Store) Close() error {
	var err error
	s.once.Do(func() {
		close(s.stop)
		err = s.Flush()
	})
	return err
}
