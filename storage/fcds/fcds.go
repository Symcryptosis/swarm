// Copyright 2019 The Swarm Authors
// This file is part of the Swarm library.
//
// The Swarm library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The Swarm library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the Swarm library. If not, see <http://www.gnu.org/licenses/>.

package fcds

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ethersphere/swarm/chunk"
)

// Interface specifies methods required for FCDS implementation.
// It can be used where alternative implementations are needed to
// switch at runtime.
type Interface interface {
	Get(addr chunk.Address) (ch chunk.Chunk, err error)
	Has(addr chunk.Address) (yes bool, err error)
	Put(ch chunk.Chunk) (err error)
	Delete(addr chunk.Address) (err error)
	Count() (count int, err error)
	Iterate(func(ch chunk.Chunk) (stop bool, err error)) (err error)
	Close() (err error)
}

var _ Interface = new(Store)

// Number of files that store chunk data.
const shardCount = 32

// ErrDBClosed is returned if database is already closed.
var ErrDBClosed = errors.New("closed database")

// Store is the main FCDS implementation. It stores chunk data into
// a number of files partitioned by the last byte of the chunk address.
type Store struct {
	shards       []shard        // relations with shard id and a shard file and their mutexes
	meta         MetaStore      // stores chunk offsets
	free         []bool         // which shards have free offsets
	freeMu       sync.RWMutex   // protects free field
	freeCache    *offsetCache   // optional cache of free offset values
	wg           sync.WaitGroup // blocks Close until all other method calls are done
	maxChunkSize int            // maximal chunk data size
	quit         chan struct{}  // quit disables all operations after Close is called
	quitOnce     sync.Once      // protects quit channel from multiple Close calls
}

// New constructs a new Store with files at path, with specified max chunk size.
// Argument withCache enables in memory cache of free chunk data positions in files.
func New(path string, maxChunkSize int, metaStore MetaStore, withCache bool) (s *Store, err error) {
	if err := os.MkdirAll(path, 0777); err != nil {
		return nil, err
	}
	shards := make([]shard, shardCount)
	for i := byte(0); i < shardCount; i++ {
		shards[i].f, err = os.OpenFile(filepath.Join(path, fmt.Sprintf("chunks-%v.db", i)), os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			return nil, err
		}
		shards[i].mu = new(sync.Mutex)
	}
	var freeCache *offsetCache
	if withCache {
		freeCache = newOffsetCache(shardCount)
	}
	return &Store{
		shards:       shards,
		meta:         metaStore,
		freeCache:    freeCache,
		free:         make([]bool, shardCount),
		maxChunkSize: maxChunkSize,
		quit:         make(chan struct{}),
	}, nil
}

// Get returns a chunk with data.
func (s *Store) Get(addr chunk.Address) (ch chunk.Chunk, err error) {
	done, err := s.protect()
	if err != nil {
		return nil, err
	}
	defer done()

	sh := s.shards[getShard(addr)]
	sh.mu.Lock()
	defer sh.mu.Unlock()

	m, err := s.getMeta(addr)
	if err != nil {
		return nil, err
	}
	data := make([]byte, m.Size)
	n, err := sh.f.ReadAt(data, m.Offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if n != int(m.Size) {
		return nil, fmt.Errorf("incomplete chunk data, read %v of %v", n, m.Size)
	}
	return chunk.NewChunk(addr, data), nil
}

// Has returns true if chunk is stored.
func (s *Store) Has(addr chunk.Address) (yes bool, err error) {
	done, err := s.protect()
	if err != nil {
		return false, err
	}
	defer done()

	mu := s.shards[getShard(addr)].mu
	mu.Lock()
	defer mu.Unlock()

	_, err = s.getMeta(addr)
	if err != nil {
		if err == chunk.ErrChunkNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Put stores chunk data.
func (s *Store) Put(ch chunk.Chunk) (err error) {
	done, err := s.protect()
	if err != nil {
		return err
	}
	defer done()

	addr := ch.Address()
	data := ch.Data()

	section := make([]byte, s.maxChunkSize)
	copy(section, data)

	shard := getShard(addr)
	sh := s.shards[shard]

	sh.mu.Lock()
	defer sh.mu.Unlock()

	offset, reclaimed, err := s.getOffset(shard)
	if err != nil {
		return err
	}

	if offset < 0 {
		offset, err = sh.f.Seek(0, io.SeekEnd)
	} else {
		_, err = sh.f.Seek(offset, io.SeekStart)
	}
	if err != nil {
		return err
	}

	if _, err = sh.f.Write(section); err != nil {
		return err
	}
	if reclaimed && s.freeCache != nil {
		s.freeCache.remove(shard, offset)
	}
	return s.meta.Set(addr, shard, reclaimed, &Meta{
		Size:   uint16(len(data)),
		Offset: offset,
	})
}

// getOffset returns an offset where chunk data can be written to
// and a flag if the offset is reclaimed from a previously removed chunk.
// If offset is less then 0, no free offsets are available.
func (s *Store) getOffset(shard uint8) (offset int64, reclaimed bool, err error) {
	if !s.shardHasFreeOffsets(shard) {
		// shard does not have free offset
		return -1, false, err
	}

	offset = -1 // negative offset denotes no available free offset
	if s.freeCache != nil {
		// check if local cache has an offset
		offset = s.freeCache.get(shard)
	}

	if offset < 0 {
		// free cache did not return a free offset,
		// check the meta store for one
		offset, err = s.meta.FreeOffset(shard)
		if err != nil {
			return 0, false, err
		}
	}
	if offset < 0 {
		// meta store did not return a free offset,
		// mark this shard that has no free offsets
		s.markShardWithFreeOffsets(shard, false)
		return -1, false, nil
	}

	return offset, true, nil
}

// Delete removes chunk data.
func (s *Store) Delete(addr chunk.Address) (err error) {
	done, err := s.protect()
	if err != nil {
		return err
	}
	defer done()

	shard := getShard(addr)
	s.markShardWithFreeOffsets(shard, true)

	mu := s.shards[shard].mu
	mu.Lock()
	defer mu.Unlock()

	if s.freeCache != nil {
		m, err := s.getMeta(addr)
		if err != nil {
			return err
		}
		s.freeCache.set(shard, m.Offset)
	}
	return s.meta.Remove(addr, shard)
}

// Count returns a number of stored chunks.
func (s *Store) Count() (count int, err error) {
	return s.meta.Count()
}

// Iterate iterates over stored chunks in no particular order.
func (s *Store) Iterate(fn func(chunk.Chunk) (stop bool, err error)) (err error) {
	done, err := s.protect()
	if err != nil {
		return err
	}
	defer done()

	for _, sh := range s.shards {
		sh.mu.Lock()
	}
	defer func() {
		for _, sh := range s.shards {
			sh.mu.Unlock()
		}
	}()

	return s.meta.Iterate(func(addr chunk.Address, m *Meta) (stop bool, err error) {
		data := make([]byte, m.Size)
		_, err = s.shards[getShard(addr)].f.ReadAt(data, m.Offset)
		if err != nil {
			return true, err
		}
		return fn(chunk.NewChunk(addr, data))
	})
}

// Close disables of further operations on the Store.
// Every call to its methods will return ErrDBClosed error.
// Close will wait for all running operations to finish before
// closing its MetaStore and returning.
func (s *Store) Close() (err error) {
	s.quitOnce.Do(func() {
		close(s.quit)
	})

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}

	for _, sh := range s.shards {
		if err := sh.f.Close(); err != nil {
			return err
		}
	}
	return s.meta.Close()
}

// protect protects Store from executing operations
// after the Close method is called and makes sure
// that Close method will wait for all ongoing operations
// to finish before returning. Returned function done
// must be closed to unblock the Close method call.
func (s *Store) protect() (done func(), err error) {
	select {
	case <-s.quit:
		// Store is closed.
		return nil, ErrDBClosed
	default:
	}
	s.wg.Add(1)
	return s.wg.Done, nil
}

// getMeta returns Meta information from MetaStore.
func (s *Store) getMeta(addr chunk.Address) (m *Meta, err error) {
	return s.meta.Get(addr)
}

func (s *Store) markShardWithFreeOffsets(shard uint8, has bool) {
	s.freeMu.Lock()
	s.free[shard] = has
	s.freeMu.Unlock()
}

func (s *Store) shardHasFreeOffsets(shard uint8) (has bool) {
	s.freeMu.RLock()
	has = s.free[shard]
	s.freeMu.RUnlock()
	return has
}

// getShard returns a shard number for the chunk address.
func getShard(addr chunk.Address) (shard uint8) {
	return addr[len(addr)-1] % shardCount
}

type shard struct {
	f  *os.File
	mu *sync.Mutex
}