// Copyright (c) 2016-2018 iQIYI.com.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package objectserver

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/AndreasBriese/bbloom"
	"github.com/golang/protobuf/proto"
	rocksdb "github.com/tecbot/gorocksdb"
	"go.uber.org/zap"

	"github.com/iqiyi/auklet/common"
	"github.com/iqiyi/auklet/common/conf"
	"github.com/iqiyi/auklet/common/fs"
	"github.com/iqiyi/auklet/common/ring"
)

type KVStore struct {
	driveRoot  string
	ringPort   int
	hashPrefix string
	hashSuffix string
	wopt       *rocksdb.WriteOptions
	ropt       *rocksdb.ReadOptions
	dbs        map[string]*rocksdb.DB
	testMode   bool
	filter     bbloom.Bloom
	counter    int64

	sync.RWMutex
}

func (s *KVStore) setTestMode(mode bool) {
	s.testMode = mode
}

func (s *KVStore) asyncJobPrefix(policy int) string {
	suffix := ""
	if policy != 0 {
		suffix = fmt.Sprintf("-%d", policy)
	}

	return fmt.Sprintf("/async_pending%s", suffix)
}

func (s *KVStore) asyncJobKey(job *KVAsyncJob) string {
	if job == nil {
		return ""
	}
	hash := common.HashObjectName(
		s.hashPrefix, job.Account, job.Container, job.Object, s.hashSuffix)
	prefix := s.asyncJobPrefix(int(job.Policy))
	return fmt.Sprintf(
		"%s/%s/%s-%s", prefix, hash[29:32], hash, job.Headers[common.XTimestamp])
}

func (s *KVStore) openAsyncJobDB(device string) (*rocksdb.DB, error) {
	opts := rocksdb.NewDefaultOptions()
	opts.SetCreateIfMissing(true)
	opts.SetWalSizeLimitMb(64)

	p := filepath.Join(s.driveRoot, device, "async-jobs")

	db, err := rocksdb.OpenDb(opts, p)
	if err != nil {
		return nil, err
	}

	return db, nil
}

func (s *KVStore) getDB(device string) *rocksdb.DB {
	if s.testMode {
		glogger.Info("get db instance in test mode")
		s.Lock()
		defer s.Unlock()
		db := s.dbs[device]
		if db == nil {
			err := os.MkdirAll(filepath.Join(s.driveRoot, device), 0755)
			if err != nil {
				glogger.Error("unable to create device directory",
					zap.String("device", device), zap.Error(err))
				return nil
			}

			db, err = s.openAsyncJobDB(device)
			if err != nil {
				glogger.Error("unable to open RocksDB",
					zap.String("device", device), zap.Error(err))
				return nil
			}
			s.dbs[device] = db
		}

		return db
	}

	s.RLock()
	defer s.RUnlock()
	return s.dbs[device]
}

func (s *KVStore) listDevices() map[int][]string {
	devices := map[int][]string{}
	for _, p := range conf.LoadPolicies() {
		devs, err := ring.ListLocalDevices(
			"object", s.hashPrefix, s.hashSuffix, p.Index, s.ringPort)
		if err != nil {
			glogger.Error("unable to get local device list",
				zap.Int("port", s.ringPort), zap.Error(err))
		}

		for _, d := range devs {
			devices[p.Index] = append(devices[p.Index], d.Device)
		}
	}

	return devices
}

func (s *KVStore) initDBs() {
	devices := s.listDevices()
	for _, devs := range devices {
		for _, d := range devs {
			// During initialization, there is no need to acquire any lock.
			if db := s.dbs[d]; db != nil {
				continue
			}

			db, err := s.openAsyncJobDB(d)
			if err != nil {
				glogger.Error("unable to open RocksDB", zap.String("device", d), zap.Error(err))
				continue
			}

			s.dbs[d] = db
		}
	}
}

func (s *KVStore) mountListener() {
	glogger.Debug("disk mount event detected")
	devices := s.listDevices()
	for _, devs := range devices {
		for _, dev := range devs {
			p := filepath.Join(s.driveRoot, dev)
			mounted, err := fs.IsMount(p)
			if err != nil {
				glogger.Error("unable to check if disk is mounted",
					zap.String("path", p), zap.Error(err))
				continue
			}

			if !mounted && s.dbs[dev] != nil {
				glogger.Info(
					"disk umounted, remove db instance", zap.String("device", dev))
				s.Lock()
				delete(s.dbs, dev)
				s.Unlock()
			}

			if mounted && s.dbs[dev] == nil {
				glogger.Info(
					"disk mounted, init db instance", zap.String("device", dev))
				db, err := s.openAsyncJobDB(dev)
				if err != nil {
					glogger.Error("unable to open RocksDB",
						zap.String("device", dev), zap.Error(err))
					continue
				}

				s.Lock()
				s.dbs[dev] = db
				s.Unlock()
			}
		}
	}
}

func (s *KVStore) SaveAsyncJob(job *KVAsyncJob) error {
	db := s.getDB(job.Device)
	if db == nil {
		return ErrAsyncJobDBNotFound
	}

	key := []byte(s.asyncJobKey(job))
	val, err := proto.Marshal(job)
	if err != nil {
		glogger.Error("unable to marshal async job", zap.Error(err))
		return err
	}

	return db.Put(s.wopt, key, val)
}

func (s *KVStore) ListAsyncJobs(
	device string, policy int, num int) ([]*KVAsyncJob, error) {
	db := s.getDB(device)
	if db == nil {
		return nil, ErrAsyncJobDBNotFound
	}

	var jobs []*KVAsyncJob
	iter := db.NewIterator(s.ropt)
	defer iter.Close()

	p := s.asyncJobPrefix(policy)
	for iter.Seek([]byte(p)); iter.Valid() && num > 0; iter.Next() {
		key := string(iter.Key().Data())
		bfk := []byte(filepath.Base(key))
		if s.filter.Has(bfk) {
			glogger.Debug("ignore listed entry", zap.String("entry", string(bfk)))
			continue
		}
		s.filter.AddTS(bfk)
		atomic.AddInt64(&s.counter, 1)
		cnt := atomic.LoadInt64(&s.counter)
		if cnt > int64(BLOOMFILTER_RESET_THREASHHOLD) {
			glogger.Info("reset bloom filter", zap.Int64("elements", cnt))
			s.filter.Clear()
		}

		glogger.Debug("add unlisted entry",
			zap.String("entry", string(bfk)), zap.Int64("elements", cnt))

		job := new(KVAsyncJob)
		if err := proto.Unmarshal(iter.Value().Data(), job); err != nil {
			glogger.Error("unable to unmarshal async pending job",
				zap.String("entry", string(key)), zap.Error(err))
			continue
		}

		jobs = append(jobs, job)
		num--
	}

	return jobs, nil
}

func (s *KVStore) CleanAsyncJob(job *KVAsyncJob) error {
	db := s.getDB(job.Device)
	if db == nil {
		return ErrAsyncJobDBNotFound
	}
	key := []byte(s.asyncJobKey(job))

	return db.Delete(s.wopt, key)
}

func NewKVStore(driveRoot string, ringPort int) *KVStore {
	s := &KVStore{
		driveRoot: driveRoot,
		dbs:       make(map[string]*rocksdb.DB),
		wopt:      rocksdb.NewDefaultWriteOptions(),
		ropt:      rocksdb.NewDefaultReadOptions(),
		ringPort:  ringPort,
		filter:    bbloom.New(BLOOMFILTER_ENTRIES, BLOOMFILTER_FP_RATIO),
	}

	var err error
	s.hashPrefix, s.hashSuffix, err = conf.GetHashPrefixAndSuffix()
	if err != nil {
		glogger.Error("unable to find hash prefix/suffix", zap.Error(err))
		return nil
	}

	s.initDBs()
	return s
}
