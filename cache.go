package sturdyc

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	xxhash "github.com/cespare/xxhash/v2"
)

// FetchFn Fetch represents a function that can be used to fetch a single record from a data source.
type FetchFn[T any] func(ctx context.Context) (T, error)

// BatchFetchFn represents a function that can be used to fetch multiple records from a data source.
type BatchFetchFn[T any] func(ctx context.Context, ids []string) (map[string]T, error)

type BatchResponse[T any] map[string]T

// KeyFn is called invoked for each record that a batch fetch
// operation returns. It is used to create unique cache keys.
type KeyFn func(id string) string

// ISturdyCItem is an optional interface cache items can implement to provide
// additional information to the cache.
type ISturdyCItem interface {
	// GetCacheKey returns the primary key for the item.
	GetCacheKey() string
	// GetCacheAliasKeys returns the list of alias keys for the item.
	GetCacheAliasKeys() []string
}

// Config represents the configuration that can be applied to the cache.
type Config struct {
	clock                      Clock
	evictionInterval           time.Duration
	disableContinuousEvictions bool
	metricsRecorder            DistributedMetricsRecorder
	log                        Logger

	refreshInBackground bool
	minRefreshTime      time.Duration
	maxRefreshTime      time.Duration
	retryBaseDelay      time.Duration
	storeMissingRecords bool

	bufferRefreshes      bool
	batchMutex           sync.Mutex
	bufferSize           int
	bufferTimeout        time.Duration
	permutationBufferMap map[string]*buffer

	useRelativeTimeKeyFormat bool
	keyTruncation            time.Duration
	getSize                  func() int

	distributedStorage              DistributedStorageWithDeletions
	distributedEarlyRefreshes       bool
	distributedRefreshAfterDuration time.Duration
}

// Client represents a cache client that can be used to store and retrieve values.
type Client[T any] struct {
	*Config
	shards             []*shard[T]
	shardIndexByAlias  sync.Map
	nextShard          int
	inFlightMutex      sync.Mutex
	inFlightBatchMutex sync.Mutex
	inFlightMap        map[string]*inFlightCall[T]
	inFlightBatchMap   map[string]*inFlightCall[map[string]T]
}

// New creates a new Client instance with the specified configuration.
//
//	`capacity` defines the maximum number of entries that the cache can store.
//	`numShards` Is used to set the number of shards. Has to be greater than 0.
//	`ttl` Sets the time to live for each entry in the cache. Has to be greater than 0.
//	`evictionPercentage` Percentage of items to evict when the cache exceeds its capacity.
//	`opts` allows for additional configurations to be applied to the cache client.
func New[T any](capacity, numShards int, ttl time.Duration, evictionPercentage int, opts ...Option) *Client[T] {
	client := &Client[T]{
		inFlightMap:       make(map[string]*inFlightCall[T]),
		inFlightBatchMap:  make(map[string]*inFlightCall[map[string]T]),
		shardIndexByAlias: sync.Map{},
	}

	// Create a default configuration, and then apply the options.
	cfg := &Config{
		clock:            NewClock(),
		evictionInterval: ttl / time.Duration(numShards),
		getSize:          client.Size,
		log:              slog.Default(),
	}
	// Apply the options to the configuration.
	client.Config = cfg
	for _, opt := range opts {
		opt(cfg)
	}
	validateConfig(capacity, numShards, ttl, evictionPercentage, cfg)

	shardSize := capacity / numShards
	shards := make([]*shard[T], numShards)
	for i := 0; i < numShards; i++ {
		shards[i] = newShard[T](shardSize, ttl, evictionPercentage, cfg)
	}
	client.shards = shards
	client.nextShard = 0

	// Run evictions on the shards in a separate goroutine.
	if !cfg.disableContinuousEvictions {
		client.performContinuousEvictions()
	}

	return client
}

// performContinuousEvictions is going to be running in a separate goroutine that we're going to prevent from ever exiting.
func (c *Client[T]) performContinuousEvictions() {
	go func() {
		ticker, stop := c.clock.NewTicker(c.evictionInterval)
		defer stop()
		for range ticker {
			c.shards[c.nextShard].evictExpired()
			c.nextShard = (c.nextShard + 1) % len(c.shards)
		}
	}()
}

// getShard returns the shard that should be used for the specified key.
func (c *Client[T]) getShard(key string) *shard[T] {
	shardIndex := c.getShardIndex(key)
	return c.shards[shardIndex]
}

// getShardIndex returns the shard index that should be used for the specified key.
// It first searches for the shard index by alias, and if the key is not a known alias,

func (c *Client[T]) getShardIndex(key string) int {
	// They key could be an alias, so let's first search for the shardIndex by alias
	shardIndex := c.findShardIndexByAlias(key)
	if shardIndex == -1 {
		// If the key is not a known alias, it may be a primary key.
		// We can use the hash to determine the shard index.
		hash := xxhash.Sum64String(key)
		shardIndex = int(hash % uint64(len(c.shards)))
	}
	c.reportShardIndex(shardIndex)
	return shardIndex
}

// findShardIndexByAlias returns the shard index that should be used for the specified key.
// It first searches for the shard index by alias, and if the key is not a known alias,
// it returns -1.
//
// Concurrency performance note: a very short read lock is required to query each shard for
// the alias.
func (c *Client[T]) findShardIndexByAlias(key string) int {
	shardIndex, ok := c.shardIndexByAlias.Load(key)
	if ok {
		return shardIndex.(int)
	}
	return -1
}

// getWithState retrieves a single value from the cache and returns additional
// information about the state of the record. The state includes whether the record
// exists, if it has been marked as missing, and if it is due for a refresh.
func (c *Client[T]) getWithState(key string) (value T, exists, markedAsMissing, refresh bool) {
	shard := c.getShard(key)
	val, exists, markedAsMissing, refresh := shard.get(key)
	c.reportCacheHits(exists, markedAsMissing, refresh)
	return val, exists, markedAsMissing, refresh
}

// Get retrieves a single value from the cache.
//
// Parameters:
//
//	key - The key to be retrieved.
//
// Returns:
//
//	The value corresponding to the key and a boolean indicating if the value was found.
func (c *Client[T]) Get(key string, opts ...GetOptions) (T, bool) {
	for _, opt := range opts {
		if opt.KeyFn != nil {
			key = opt.KeyFn(key)
		}
	}
	shard := c.getShard(key)
	val, ok, markedAsMissing, refresh := shard.get(key)
	c.reportCacheHits(ok, markedAsMissing, refresh)
	return val, ok && !markedAsMissing
}

type GetOptions struct {
	KeyFn KeyFn
}

func WithGetKeyFn(keyFn KeyFn) GetOptions {
	return GetOptions{KeyFn: keyFn}
}

// GetMany retrieves multiple values from the cache.
//
// Parameters:
//
//	keys - The list of keys to be retrieved.
//
// Returns:
//
//	A map of keys to their corresponding values.
func (c *Client[T]) GetMany(keys []string, opts ...GetOptions) map[string]T {
	records := make(map[string]T, len(keys))
	for _, key := range keys {
		if value, ok := c.Get(key, opts...); ok {
			records[key] = value
		}
	}
	return records
}

// Deprecated: Use GetMany with WithGetKeyFn instead.
// GetManyKeyFn follows the same API as GetOrFetchBatch and PassthroughBatch.
// You provide it with a slice of IDs and a keyFn, which is applied to create
// the cache key. The returned map uses the IDs as keys instead of the cache
// key. If you've used ScanKeys to retrieve the actual keys, you can retrieve
// the records using GetMany instead.
//
// Parameters:
//
//	ids - The list of IDs to be retrieved.
//	keyFn - A function that generates the cache key for each ID.
//
// Returns:
//
//	A map of IDs to their corresponding values.
func (c *Client[T]) GetManyKeyFn(ids []string, keyFn KeyFn) map[string]T {
	return c.GetMany(ids, WithGetKeyFn(keyFn))
}

// Set writes a single value to the cache.
//
// Parameters:
//
//	key - The key to be set.
//	value - The value to be associated with the key.
//
// Returns:
//
//	A boolean indicating if the set operation triggered an eviction.
func (c *Client[T]) Set(key string, value T) bool {
	key, aliases := c.set_applyCacheItemInterface(key, value)
	shardIndex := c.getShardIndex(key)
	c.set_aliasGuard(key, aliases, shardIndex)
	shard := c.shards[shardIndex]
	return shard.set(key, value, false, aliases)
}

// If any of the aliases exist on a different shard, this is an error
func (c *Client[T]) set_aliasGuard(key string, aliases []string, shardIndex int) {
	for _, alias := range aliases {
		aliasShardIndex, ok := c.shardIndexByAlias.Load(alias)
		// slog.Info("aliasGuard", "alias", alias, "shardIndex", shardIndex, "key", key, "ok", ok)
		if ok {
			// slog.Info("aliasGuard", "alias", alias, "shardIndex", shardIndex, "key", key, "entryKeysByAlias", c.shards[shardIndex].entryKeysByAlias)
			if aliasShardIndex.(int) != shardIndex {
				panic(fmt.Sprintf("alias '%s' already exists in a different shard", alias))
			}
			// slog.Info("alias already exists", "alias", alias, "shardIndex", shardIndex, "key", key, "entryKeysByAlias", c.shards[shardIndex].entryKeysByAlias)
			if c.shards[shardIndex].entryKeysByAlias[alias] != key {
				panic(fmt.Sprintf("alias '%s' already exists for a different key", alias))
			}
		} else {
			c.shardIndexByAlias.Store(alias, shardIndex)
		}
	}
}

// Iterate through SetOptions and apply
func (c *Client[T]) set_applyCacheItemInterface(key string, value T) (string, []string) {
	item, ok := any(value).(ISturdyCItem)
	if ok {
		key := item.GetCacheKey()
		aliases := item.GetCacheAliasKeys()
		return key, aliases
	}

	return key, nil
}

// StoreMissingRecord writes a single value to the cache. Returns true if it triggered an eviction.
func (c *Client[T]) StoreMissingRecord(key string) bool {
	shard := c.getShard(key)
	var zero T
	return shard.set(key, zero, true, nil)
}

// SetMany writes a map of key-value pairs to the cache.
//
// Parameters:
//
//	records - A map of keys to values to be set in the cache.
//
// Returns:
//
//	A boolean indicating if any of the set operations triggered an eviction.
func (c *Client[T]) SetMany(records map[string]T) bool {
	var triggeredEviction bool
	for key, value := range records {
		evicted := c.Set(key, value)
		if evicted {
			triggeredEviction = true
		}
	}
	return triggeredEviction
}

// SetManyKeyFn follows the same API as GetOrFetchBatch and PassthroughBatch.
// It takes a map of records where the keyFn is applied to each key in the map
// before it's stored in the cache.
//
// Parameters:
//
//	records - A map of IDs to values to be set in the cache.
//	cacheKeyFn - A function that generates the cache key for each ID.
//
// Returns:
//
//	A boolean indicating if any of the set operations triggered an eviction.
//
// Note: Alternatively, you may implement ISturdyCItem on your cache item type
// to provide the cache key and alias keys.
func (c *Client[T]) SetManyKeyFn(records map[string]T, cacheKeyFn KeyFn) bool {
	var triggeredEviction bool
	for id, value := range records {
		evicted := c.Set(cacheKeyFn(id), value)
		if evicted {
			triggeredEviction = true
		}
	}
	return triggeredEviction
}

// ScanKeys returns a list of all keys in the cache.
//
// Returns:
//
//	A slice of strings representing all the keys in the cache.
func (c *Client[T]) ScanKeys() []string {
	keys := make([]string, 0, c.Size())
	for _, shard := range c.shards {
		keys = append(keys, shard.keys()...)
	}
	return keys
}

// Size returns the number of entries in the cache.
//
// Returns:
//
//	An integer representing the total number of entries in the cache.
func (c *Client[T]) Size() int {
	var sum int
	for _, shard := range c.shards {
		sum += shard.size()
	}
	return sum
}

// Delete removes a single entry from the cache.
//
// Parameters:
//
//	key: The key of the entry to be removed.
func (c *Client[T]) Delete(key string) {
	shard := c.getShard(key)
	shard.delete(key)
}

// NumKeysInflight returns the number of keys that are currently being fetched.
//
// Returns:
//
//	An integer representing the total number of keys that are currently being fetched.
func (c *Client[T]) NumKeysInflight() int {
	c.inFlightMutex.Lock()
	defer c.inFlightMutex.Unlock()
	c.inFlightBatchMutex.Lock()
	defer c.inFlightBatchMutex.Unlock()
	return len(c.inFlightMap) + len(c.inFlightBatchMap)
}
