// Copyright (c) 2017 Cisco and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package redis

import (
	"time"

	"github.com/ligato/cn-infra/db/keyval"
	"github.com/ligato/cn-infra/logging"

	"errors"
	"reflect"
	"strings"

	"fmt"
	redigo "github.com/garyburd/redigo/redis"
)

// BytesConnectionRedis allows to store, read and watch values from Redis.
type BytesConnectionRedis struct {
	logging.Logger
	pool   ConnPool
	client Client

	// closeCh will be closed when this connection is closed -- i.e., by the Close() method.
	// It is used to give go routines a signal to stop.
	closeCh chan struct{}

	// Flag to indicate whether this connection is closed
	closed bool
}

// bytesKeyValIterator is an iterator returned by ListValues call
type bytesKeyValIterator struct {
	index  int
	values []*bytesKeyVal
}

// bytesKeyIterator is an iterator returned by ListKeys call
type bytesKeyIterator struct {
	index int
	keys  []string
}

// bytesKeyVal represents a single key-value pair
type bytesKeyVal struct {
	key   string
	value []byte
}

// NewBytesConnectionRedis creates a new instance of BytesConnectionRedis using the provided
// ConnPool
func NewBytesConnectionRedis(pool ConnPool, log logging.Logger) (*BytesConnectionRedis, error) {
	return &BytesConnectionRedis{log, pool, nil, make(chan struct{}), false}, nil
}

// NewBytesConnection creates a new instance of BytesConnectionRedis using the provided
// Client (be it node, cluster, or sentinel client)
func NewBytesConnection(client Client, log logging.Logger) (*BytesConnectionRedis, error) {
	return &BytesConnectionRedis{log, nil, client, make(chan struct{}), false}, nil
}

// Close closes the connection to redis.
func (db *BytesConnectionRedis) Close() error {
	if db.closed {
		db.Debug("Close() called on a closed connection")
		return nil
	}
	db.Debug("Close()")
	db.closed = true
	close(db.closeCh)
	if db.pool != nil {
		err := db.pool.Close()
		if err != nil {
			return fmt.Errorf("Close() encountered error: %s", err)
		}
	}
	if db.client != nil {
		err := db.client.Close()
		if err != nil {
			return fmt.Errorf("Close() encountered error: %s", err)
		}
	}
	return nil
}

// NewTxn creates new transaction.
func (db *BytesConnectionRedis) NewTxn() keyval.BytesTxn {
	if db.closed {
		db.Error("NewTxn() called on a closed connection")
		return nil
	}
	db.Debug("NewTxn()")

	return &Txn{db: db, ops: []op{}}
}

// Put sets the key/value in Redis data store. Replaces value if the key already exists.
func (db *BytesConnectionRedis) Put(key string, data []byte, opts ...keyval.PutOption) error {
	if db.closed {
		return fmt.Errorf("Put(%s) called on a closed connection", key)
	}
	db.Debugf("Put(%s)", key)

	var ttl time.Duration
	for _, o := range opts {
		if withTTL, ok := o.(*keyval.WithTTLOpt); ok && withTTL.TTL > 0 {
			ttl = withTTL.TTL
		}
	}
	if db.pool != nil {
		return redigoPut(db, key, data, ttl)
	}
	err := db.client.Set(key, data, ttl).Err()
	if err != nil {
		return fmt.Errorf("Set(%s) failed: %s", key, err)
	}
	return nil
}

// GetValue retrieves the value of the key from Redis.
func (db *BytesConnectionRedis) GetValue(key string) (data []byte, found bool, revision int64, err error) {
	if db.closed {
		return nil, false, 0, fmt.Errorf("GetValue(%s) called on a closed connection", key)
	}
	db.Debugf("GetValue(%s)", key)

	if db.pool != nil {
		return redigoGetValue(db, key)
	}
	statusCmd := db.client.Get(key)
	data, err = statusCmd.Bytes()
	if err != nil {
		if err == GoRedisNil {
			return data, false, 0, nil
		}
		return nil, false, 0, fmt.Errorf("Get(%s) failed: %s", key, err)
	}
	return data, true, 0, nil
}

// ListValues lists values for all the keys that start with the given match string.
func (db *BytesConnectionRedis) ListValues(match string) (keyval.BytesKeyValIterator, error) {
	if db.closed {
		return nil, fmt.Errorf("ListValues(%s) called on a closed connection", match)
	}
	db.Debugf("ListValues(%s)", match)

	keys, err := scanKeys(db, match)
	if err != nil {
		return nil, err
	}

	values, err := listValues(db, keys)
	if err != nil {
		return nil, err
	}

	kvs := make([]*bytesKeyVal, len(values))
	for i, val := range values {
		kvs[i] = &bytesKeyVal{keys[i], val}
	}

	return &bytesKeyValIterator{values: kvs}, nil
}

// ListKeys returns an iterator used to traverse keys that start with the given match string.
func (db *BytesConnectionRedis) ListKeys(match string) (keyval.BytesKeyIterator, error) {
	if db.closed {
		return nil, fmt.Errorf("ListKeys(%s) called on a closed connection", match)
	}
	db.Debugf("ListKeys(%s)", match)

	keys, err := scanKeys(db, match)
	if err != nil {
		return nil, err
	}
	return &bytesKeyIterator{keys: keys}, nil
}

// ListValuesRange returns an iterator used to traverse values stored under the provided key.
// TODO: Not in BytesBroker interface
/*
func (db *BytesConnectionRedis) ListValuesRange(fromPrefix string, toPrefix string) (keyval.BytesKeyValIterator, error) {
	db.Panic("Not implemented")
	return nil, nil
}
*/

func listValues(db *BytesConnectionRedis, keys []string) (values [][]byte, err error) {
	db.Debugf("listValues(%v)", keys)

	if len(keys) == 0 {
		return [][]byte{}, nil
	}

	if db.pool != nil {
		return redigoListValues(db, keys)
	}

	sliceCmd := db.client.MGet(keys...)
	if sliceCmd.Err() != nil {
		return nil, fmt.Errorf("MGet(%v) failed: %s", keys, sliceCmd.Err())
	}
	vals := sliceCmd.Val()
	values = make([][]byte, len(vals))
	for i, v := range vals {
		switch o := v.(type) {
		case string:
			values[i] = []byte(o)
		case []byte:
			values[i] = o
		case nil:
			values[i] = nil
		}
	}
	return values, nil
}

func scanKeys(db *BytesConnectionRedis, match string) (keys []string, err error) {
	pattern := wildcard(match)
	db.Debugf("scanKeys(%s): pattern %s", match, pattern)

	if db.pool != nil {
		return redigoScanKeys(db, pattern)
	}
	// TODO: goredis.ClusterClient.Scan() doesn't return any key (bug?)
	keys = []string{}
	var cursor uint64
	for {
		page, next, err := db.client.Scan(cursor, pattern, 10).Result()
		if err != nil {
			return nil, fmt.Errorf("Scan(%s) failed: %s", pattern, err)
		}
		if db.GetLevel() == logging.DebugLevel {
			db.Debugf("Scan(%s): got %d keys @ cursor %d (next cursor %d)", pattern, len(page), cursor, next)
		}
		keys = append(keys, page...)
		if next == 0 {
			if db.GetLevel() == logging.DebugLevel {
				db.Debugf("Scan(%s): got total %d keys", pattern, len(keys))
			}
			break
		}
		cursor = next
	}
	return keys, nil
}

const redisWildcardChars = "*?[]"

func wildcard(match string) string {
	containsWildcard := strings.ContainsAny(match, redisWildcardChars)
	if !containsWildcard {
		return match + "*" //prefix
	}
	return match
}

// Delete deletes all the keys that start with the given match string.
func (db *BytesConnectionRedis) Delete(match string, opts ...keyval.DelOption) (found bool, err error) {
	//TODO: process delete opts
	if db.closed {
		return false, fmt.Errorf("Delete(%s) called on a closed connection", match)
	}
	db.Debugf("Delete(%s)", match)

	keysToDelete, err := scanKeys(db, match)
	if err != nil {
		return false, err
	}

	if len(keysToDelete) == 0 {
		return false, nil
	}

	db.Debugf("Delete(%s): deleting %v", match, keysToDelete)

	if db.pool != nil {
		return redigoDelete(db, keysToDelete)
	}

	intCmd := db.client.Del(keysToDelete...)
	if intCmd.Err() != nil {
		return false, fmt.Errorf("Delete(%s) failed: %s", match, intCmd.Err())
	}
	return (intCmd.Val() != 0), nil
}

// GetNext returns the next item from the iterator.
// If the iterator has reached the last item previously, lastReceived set to true.
func (ctx *bytesKeyValIterator) GetNext() (kv keyval.BytesKeyVal, lastReceived bool) {
	if ctx.index >= len(ctx.values) {
		return nil, true
	}

	kv = ctx.values[ctx.index]
	ctx.index++
	return kv, false
}

// GetNext returns the next item from the iterator.
// If the iterator has reached the last item previously, lastReceived is set to true.
func (ctx *bytesKeyIterator) GetNext() (key string, rev int64, lastReceived bool) {

	if ctx.index >= len(ctx.keys) {
		return "", 0, true
	}

	key = ctx.keys[ctx.index]
	ctx.index++
	return key, 0, false
}

// GetValue returns the value of the pair
func (kv *bytesKeyVal) GetValue() []byte {
	return kv.value
}

// GetKey returns the key of the pair
func (kv *bytesKeyVal) GetKey() string {
	return kv.key
}

// GetRevision returns the revision associated with the pair
func (kv *bytesKeyVal) GetRevision() int64 {
	return 0
}

// BytesBrokerWatcherRedis uses BytesConnectionRedis to access the datastore.
// The connection can be shared among multiple BytesBrokerWatcherRedis.
// BytesBrokerWatcherRedis allows to define a keyPrefix that is prepended to
// all keys in its methods in order to shorten keys used in arguments.
type BytesBrokerWatcherRedis struct {
	logging.Logger
	prefix   string
	delegate *BytesConnectionRedis

	// closeCh is a channel closed when Close method of data broker is closed.
	// It is used for giving go routines a signal to stop.
	closeCh chan struct{}
}

// NewBrokerWatcher creates a new CRUD + Watcher proxy instance to redis using through BytesConnectionRedis.
// The given prefix will be prepended to key argument in all calls.
// Specify empty string ("") if not wanting to use prefix.
func (db *BytesConnectionRedis) NewBrokerWatcher(prefix string) *BytesBrokerWatcherRedis {
	return &BytesBrokerWatcherRedis{db.Logger, prefix, db, db.closeCh}
}

// NewBroker creates a new CRUD proxy instance to redis using through BytesConnectionRedis.
// The given prefix will be prepended to key argument in all calls.
// Specify empty string ("") if not wanting to use prefix.
func (db *BytesConnectionRedis) NewBroker(prefix string) keyval.BytesBroker {
	return db.NewBrokerWatcher(prefix)
}

// NewWatcher creates a new Watcher proxy instance to redis using through BytesConnectionRedis.
// The given prefix will be prepended to key argument in all calls.
// Specify empty string ("") if not wanting to use prefix.
func (db *BytesConnectionRedis) NewWatcher(prefix string) keyval.BytesWatcher {
	return db.NewBrokerWatcher(prefix)
}

func (pdb *BytesBrokerWatcherRedis) addPrefix(key string) string {
	return pdb.prefix + key
}

func (pdb *BytesBrokerWatcherRedis) trimPrefix(key string) string {
	return strings.TrimPrefix(key, pdb.prefix)
}

// GetPrefix returns the prefix associated with this BytesBrokerWatcherRedis.
func (pdb *BytesBrokerWatcherRedis) GetPrefix() string {
	return pdb.prefix
}

// Put calls Put function of BytesConnectionRedis. Prefix will be prepended to key argument.
func (pdb *BytesBrokerWatcherRedis) Put(key string, data []byte, opts ...keyval.PutOption) error {
	if pdb.delegate.closed {
		return fmt.Errorf("Put(%s) called on a closed connection", key)
	}
	pdb.Debugf("Put(%s)", key)

	return pdb.delegate.Put(pdb.addPrefix(key), data, opts...)
}

// NewTxn creates new transaction. Prefix will be prepended to key argument.
func (pdb *BytesBrokerWatcherRedis) NewTxn() keyval.BytesTxn {
	if pdb.delegate.closed {
		pdb.Error("NewTxn() called on a closed connection")
		return nil
	}
	pdb.Debug("NewTxn()")

	return &Txn{db: pdb.delegate, ops: []op{}, prefix: pdb.prefix}
}

// GetValue call GetValue function of BytesConnectionRedis.
// Prefix will be prepended to key argument when searching.
func (pdb *BytesBrokerWatcherRedis) GetValue(key string) (data []byte, found bool, revision int64, err error) {
	if pdb.delegate.closed {
		return nil, false, 0, fmt.Errorf("GetValue(%s) called on a closed connection", key)
	}
	pdb.Debugf("GetValue(%s)", key)

	return pdb.delegate.GetValue(pdb.addPrefix(key))
}

// ListValues calls ListValues function of BytesConnectionRedis.
// Prefix will be prepended to key argument when searching.
// The returned keys, however, will have the prefix trimmed.
func (pdb *BytesBrokerWatcherRedis) ListValues(match string) (keyval.BytesKeyValIterator, error) {
	if pdb.delegate.closed {
		return nil, fmt.Errorf("ListValues(%s) called on a closed connection", match)
	}
	pdb.Debugf("ListValues(%s)", match)

	keys, err := scanKeys(pdb.delegate, pdb.addPrefix(match))
	if err != nil {
		return nil, err
	}

	values, err := listValues(pdb.delegate, keys)
	if err != nil {
		return nil, errors.New(err.Error() + " for " + match)
	}

	kvs := make([]*bytesKeyVal, len(values))
	for i, val := range values {
		kvs[i] = &bytesKeyVal{pdb.trimPrefix(keys[i]), val}
	}

	return &bytesKeyValIterator{values: kvs}, err
}

// ListKeys calls ListKeys function of BytesConnectionRedis.
// Prefix will be prepended to key argument when searching.
// The returned keys, however, will have the prefix trimmed.
func (pdb *BytesBrokerWatcherRedis) ListKeys(match string) (keyval.BytesKeyIterator, error) {
	if pdb.delegate.closed {
		return nil, fmt.Errorf("ListKeys(%s) called on a closed connection", match)
	}
	pdb.Debugf("ListKeys(%s)", match)

	keys, err := scanKeys(pdb.delegate, pdb.addPrefix(match))
	if err != nil {
		return nil, err
	}

	for i, key := range keys {
		keys[i] = pdb.trimPrefix(key)
	}

	return &bytesKeyIterator{keys: keys}, err
}

// Delete calls Delete function of BytesConnectionRedis.
// Prefix will be prepended to key argument when searching.
func (pdb *BytesBrokerWatcherRedis) Delete(match string, opts ...keyval.DelOption) (bool, error) {
	if pdb.delegate.closed {
		return false, fmt.Errorf("Delete(%s) called on a closed connection", match)
	}
	pdb.Debugf("Delete(%s)", match)

	//TODO: process delete opts
	return pdb.delegate.Delete(pdb.addPrefix(match))
}

// ListValuesRange calls ListValuesRange function of BytesConnectionRedis.
// Prefix will be prepended to key argument when searching.
// TODO: Not in BytesBroker interface
/*
func (pdb *BytesBrokerWatcherRedis) ListValuesRange(fromPrefix string, toPrefix string) (keyval.BytesKeyValIterator, error) {
	return pdb.delegate.ListValuesRange(pdb.addPrefix(fromPrefix), pdb.addPrefix(toPrefix))
}
*/

///////////////////////////////////////////////////////////////////////////////
// redigo legacy
//  - Legacy code that uses http://godoc.org/github.com/garyburd/redigo
//  - Only node client is supported
func redigoPut(db *BytesConnectionRedis, key string, data []byte, ttl time.Duration) error {
	db.Debugf("redigoPut(%s)", key)

	conn := db.pool.Get()
	defer conn.Close()
	var err error
	if ttl == 0 {
		_, err = conn.Do("SET", key, string(data))
	} else {
		_, err = conn.Do("SET", key, string(data), "PX", int64(ttl/time.Millisecond))
	}
	if err != nil {
		return fmt.Errorf("Do(SET) failed: %s", err)
	}
	return nil
}
func redigoGetValue(db *BytesConnectionRedis, key string) (data []byte, found bool, revision int64, err error) {
	db.Debugf("redigoGetValue(%s)", key)

	conn := db.pool.Get()
	defer conn.Close()
	reply, err := conn.Do("GET", key)
	if err != nil {
		return nil, false, 0, fmt.Errorf("Do(GET) failed: %s", err)
	}
	db.Debug("GET reply ", reply)

	switch reply := reply.(type) {
	case []byte:
		return reply, true, 0, nil
	case string:
		return []byte(reply), true, 0, nil
	case nil:
		return nil, false, 0, nil
	case redigo.Error:
		return nil, false, 0, reply
	default:
		return nil, false, 0,
			fmt.Errorf("Unknown type %s for %s", reflect.TypeOf(reply).String(), key)
	}
}
func redigoListValues(db *BytesConnectionRedis, keys []string) (values [][]byte, err error) {
	db.Debugf("redigoListValues(%v)", keys)

	conn := db.pool.Get()
	defer conn.Close()
	keysIntf := make([]interface{}, len(keys))
	for i, k := range keys {
		keysIntf[i] = k
	}
	reply, err := conn.Do("MGET", keysIntf...)
	if err != nil {
		return nil, fmt.Errorf("Do(MGET) failed: %s", err)
	}

	switch reply := reply.(type) {
	case []interface{}:
		values := make([][]byte, len(keys))

		l := 0
		for i := range reply {
			r := reply[i]

			switch r := r.(type) {
			case nil:
				values[i] = nil
			case []byte:
				values[i] = r
			case string:
				values[i] = []byte(r)
			}
			l++
		}

		db.WithField("length", l).Debugf("listValues(%v)", keys)

		if len(keys) != len(values) {
			return nil, fmt.Errorf("Unexpeted %d != %d", len(keys), len(values))
		}

		return values, nil
	case redigo.Error:
		return nil, reply
	}

	return [][]byte{}, nil
}
func redigoScanKeys(db *BytesConnectionRedis, pattern string) (keys []string, err error) {
	db.Debugf("redigoScanKeys(%s)", pattern)

	conn := db.pool.Get()
	defer conn.Close()
	cursor := "0"
	keys = []string{}
	for {
		reply, err := conn.Do("SCAN", cursor, "MATCH", pattern)
		if err != nil {
			return nil, fmt.Errorf("Do(SCAN) failed: %s", err)
		}
		db.Debugf("SCAN returned %v", reply)
		switch r := reply.(type) {
		case []interface{}:
			cursor = string(r[0].([]byte))
			db.Debugf("cursor = %s", cursor)
			for _, k := range r[1].([]interface{}) {
				if k == nil {
					continue
				}
				switch k := k.(type) {
				case []byte:
					keys = append(keys, string(k))
				case string:
					keys = append(keys, k)
				}
			}
			if cursor == "0" {
				return keys, nil
			}
		case redigo.Error:
			return nil, r
		default:
			if reply == nil {
				return nil, errors.New("Do(SCAN) returned nil")
			}
			return nil, fmt.Errorf("Do(SCAN) returned unexpected type %T", reply)
		}
	}
}
func redigoDelete(db *BytesConnectionRedis, keysToDelete []string) (found bool, err error) {
	db.Debugf("redigoDelete(%v)", keysToDelete)

	args := make([]interface{}, len(keysToDelete))
	for i, s := range keysToDelete {
		args[i] = s
	}
	conn := db.pool.Get()
	defer conn.Close()
	reply, err := conn.Do("DEL", args...)
	if err != nil {
		return false, fmt.Errorf("Do(DEL) failed: %s", err)
	}
	db.Debugf("DEL replied %v (type: %s)", reply, reflect.TypeOf(reply).String())

	if err, ok := reply.(redigo.Error); ok {
		return false, err
	}
	if deleted, ok := reply.(int64); ok {
		if deleted == 0 {
			return false, nil
		}

		if deleted < int64(len(keysToDelete)) {
			db.Debugf("Deleted %d of %d", deleted, len(keysToDelete))
		}
	}
	return true, nil
}

/* replace by redigoScanKeys
func redigoListKeys(db *BytesConnectionRedis, match string) (keys []string, err error) {
	if db.closed {
		return nil, fmt.Errorf("redigoListKeys(%s) called on a closed connection", match)
	}
	pattern := wildcard(match)
	db.Debugf("redigoListKeys(%s): pattern %s", match, pattern)

	conn := db.pool.Get()
	defer conn.Close()
	reply, err := conn.Do("KEYS", pattern)
	if err != nil {
		return nil, fmt.Errorf("Do(KEYS) failed: %s", err)
	}

	switch reply := reply.(type) {
	case []interface{}:
		keys := make([]string, len(reply))
		length := 0
		for i := range reply {
			r := reply[i]
			if r == nil {
				continue
			}

			switch r := r.(type) {
			case []byte:
				keys[length] = string(r)
			case string:
				keys[length] = r
			}
			length++
		}

		db.WithFields(map[string]interface{}{"length": length, "match": match, "keys": keys}).Debugf("listKeys: pattern %s", pattern)

		if length == 0 {
			return []string{}, err
		}
		return keys[0:length], nil
	case redigo.Error:
		return nil, reply
	}

	return nil, err
}
*/
