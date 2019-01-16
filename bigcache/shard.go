package bigcache

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/emirpasic/gods/sets/hashset"
	"github.com/nggenius/ngbigcache/bigcache/queue"
)

//NO_EXPIRY means data placed into a shard will never expire
const NO_EXPIRY uint64 = 0

type cacheShard struct {
	sharedNum   uint64
	hashmap     map[uint64]uint32
	entries     queue.BytesQueue
	lock        sync.RWMutex
	entryBuffer []byte
	onRemove    func(wrappedEntry []byte)

	isVerbose  bool
	logger     Logger
	clock      clock
	lifeWindow uint64

	stats    Stats
	ttlTable *ttlManager
}

type onRemoveCallback func(wrappedEntry []byte)

func (s *cacheShard) get(key string, hashedKey uint64) ([]byte, error) {
	s.lock.RLock()
	itemIndex := s.hashmap[hashedKey]

	if itemIndex == 0 {
		s.lock.RUnlock()
		s.miss()
		return nil, notFound(key)
	}

	wrappedEntry, err := s.entries.Get(int(itemIndex))
	if err != nil {
		s.lock.RUnlock()
		s.miss()
		return nil, err
	}
	if entryKey := readKeyFromEntry(wrappedEntry); key != entryKey {
		if s.isVerbose {
			s.logger.Printf("Collision detected. Both %q and %q have the same hash %x", key, entryKey, hashedKey)
		}
		s.lock.RUnlock()
		s.collision()
		return nil, notFound(key)
	}
	s.lock.RUnlock()
	s.hit()
	return readEntry(wrappedEntry), nil
}

func (s *cacheShard) set(key string, hashedKey uint64, entry []byte, duration time.Duration) (uint64, error) {
	expiryTimestamp := uint64(s.clock.epoch())
	if duration != time.Duration(NO_EXPIRY) {
		expiryTimestamp += uint64(duration.Seconds())
	} else {
		expiryTimestamp = NO_EXPIRY
	}

	s.lock.Lock()

	if previousIndex := s.hashmap[hashedKey]; previousIndex != 0 {
		if previousEntry, err := s.entries.Get(int(previousIndex)); err == nil {
			timestamp := readTimestampFromEntry(previousEntry)
			s.ttlTable.remove(timestamp, key)
			resetKeyFromEntry(previousEntry)
			s.delete(previousIndex)
		}
	}

	w := wrapEntry(expiryTimestamp, hashedKey, key, entry, &s.entryBuffer)

	var err error
	if index, err := s.entries.Push(w); err == nil {
		s.hashmap[hashedKey] = uint32(index)
		s.lock.Unlock()
		if duration != time.Duration(NO_EXPIRY) {
			s.ttlTable.put(expiryTimestamp, key)
		}
		return expiryTimestamp, nil
	}

	return 0, err
}

func (s *cacheShard) evictDel(timeStamp uint64, set *hashset.Set) error {
	s.lock.Lock()

	for _, v := range set.Values() { // remove all data in the hashset for this eviction
		s.stats.EvictCount++
		keyHash := s.ttlTable.ShardHasher.Sum64(v.(string))
		itemIndex := s.hashmap[keyHash]

		if itemIndex == 0 {
			s.delmiss()
			continue
		}

		wrappedEntry, err := s.entries.Get(int(itemIndex))
		if err != nil {
			s.delmiss()
			continue
		}

		storedTimestamp := readTimestampFromEntry(wrappedEntry)
		if storedTimestamp == timeStamp {
			delete(s.hashmap, keyHash)
			s.onRemove(wrappedEntry)
			resetKeyFromEntry(wrappedEntry)
			s.delete(itemIndex)
			s.delhit()
		}

	}

	s.lock.Unlock()

	return nil
}

func (s *cacheShard) del(key string, hashedKey uint64) error {
	s.lock.Lock()
	itemIndex := s.hashmap[hashedKey]

	if itemIndex == 0 {
		s.lock.Unlock()
		s.delmiss()
		return notFound(key)
	}

	wrappedEntry, err := s.entries.Get(int(itemIndex))
	if err != nil {
		s.lock.Unlock()
		s.delmiss()
		return err
	}

	delete(s.hashmap, hashedKey)
	s.onRemove(wrappedEntry)
	resetKeyFromEntry(wrappedEntry)
	s.delete(itemIndex)
	s.lock.Unlock()

	timestamp := readTimestampFromEntry(wrappedEntry)
	s.ttlTable.remove(timestamp, key)

	s.delhit()
	return nil
}

func (s *cacheShard) onEvict(oldestEntry []byte, currentTimestamp uint64, evict func() error) bool {
	oldestTimestamp := readTimestampFromEntry(oldestEntry)
	if currentTimestamp-oldestTimestamp > s.lifeWindow {
		evict()
		return true
	}
	return false
}

//func (s *cacheShard) cleanUp(currentTimestamp uint64) {
//	s.lock.Lock()
//	for {
//		if oldestEntry, err := s.entries.Peek(); err != nil {
//			break
//		} else if evicted := s.onEvict(oldestEntry, currentTimestamp, s.removeOldestEntry); !evicted {
//			break
//		}
//	}
//	s.lock.Unlock()
//}

func (s *cacheShard) getOldestEntry() ([]byte, error) {
	return s.entries.Peek()
}

func (s *cacheShard) getEntry(index int) ([]byte, error) {
	return s.entries.Get(index)
}

func (s *cacheShard) copyKeys() (keys []uint32, next int) {
	keys = make([]uint32, len(s.hashmap))

	s.lock.RLock()

	for _, index := range s.hashmap {
		keys[next] = index
		next++
	}

	s.lock.RUnlock()
	return keys, next
}

func (s *cacheShard) removeOldestEntry() error {
	oldest, err := s.entries.Pop()
	if err == nil {
		hash := readHashFromEntry(oldest)
		delete(s.hashmap, hash)
		s.onRemove(oldest)
		return nil
	}
	return err
}

func (s *cacheShard) reset(config Config) {
	s.lock.Lock()
	s.hashmap = make(map[uint64]uint32, config.initialShardSize())
	s.entryBuffer = make([]byte, config.MaxEntrySize+headersSizeInBytes)
	s.entries.Reset()
	s.ttlTable.reset()
	s.lock.Unlock()
}

func (s *cacheShard) len() int {
	s.lock.RLock()
	res := len(s.hashmap)
	s.lock.RUnlock()
	return res
}

func (s *cacheShard) getStats() Stats {
	return s.stats
}

func (s *cacheShard) hit() {
	atomic.AddInt64(&s.stats.Hits, 1)
}

func (s *cacheShard) miss() {
	atomic.AddInt64(&s.stats.Misses, 1)
}

func (s *cacheShard) delhit() {
	atomic.AddInt64(&s.stats.DelHits, 1)
}

func (s *cacheShard) delmiss() {
	atomic.AddInt64(&s.stats.DelMisses, 1)
}

func (s *cacheShard) collision() {
	atomic.AddInt64(&s.stats.Collisions, 1)
}

func (s *cacheShard) delete(index uint32) {
	s.entries.Delete(int(index))
}

//func (s *cacheShard) Delete(index uint32) {
//	diff, _ := s.entries.Delete(int(index))
//
//	for i,_ := range s.hashmap {
//		idx := s.hashmap[i]
//		if idx > index {
//			s.hashmap[i] = idx - uint32(diff)
//		}
//	}
//
//	s.fList.adjustIndexes(index, diff)
//}
//func (s *cacheShard) doCompact() {
//	for range time.NewTicker(time.Second * 10).C {
//		s.lock.Lock()
//		for x := 0; x < 512; x++ {
//			index, err := s.fList.Pop()
//			if err != nil {
//				break
//			}
//			s.Delete(uint32(index))
//		}
//		s.lock.Unlock()
//	}
//}

func initNewShard(config Config, callback onRemoveCallback, clock clock, num uint64) *cacheShard {
	shard := &cacheShard{
		hashmap:     make(map[uint64]uint32, config.initialShardSize()),
		entries:     *queue.NewBytesQueue(config.initialShardSize()*config.MaxEntrySize, config.maximumShardSize(), config.Verbose),
		entryBuffer: make([]byte, config.MaxEntrySize+headersSizeInBytes),
		onRemove:    callback,

		isVerbose:  config.Verbose,
		logger:     newLogger(config.Logger),
		clock:      clock,
		lifeWindow: uint64(config.LifeWindow.Seconds()),
		sharedNum:  num,
	}

	shard.ttlTable = newTtlManager(shard, config.Hasher)

	//go shard.doCompact()

	return shard
}
