// Copyright 2017-2019 Lei Ni (nilei81@gmail.com) and other Dragonboat authors.
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

// +build dragonboat_memfs_test

package logdb

import (
	"github.com/lni/dragonboat/v3/config"
	"github.com/lni/dragonboat/v3/internal/logdb/kv"
	"github.com/lni/dragonboat/v3/internal/logdb/kv/pebble"
	"github.com/lni/dragonboat/v3/internal/vfs"

	gvfs "github.com/lni/goutils/vfs"
)

const (
	// DefaultKVStoreTypeName is the type name of the default kv store
	DefaultKVStoreTypeName = "pebble"
)

func newDefaultKVStore(config config.LogDBConfig,
	dir string, wal string, fs vfs.IFS) (kv.IKVStore, error) {
	if fs == nil {
		panic("nil fs")
	}
	_, ok := fs.(*gvfs.MemFS)
	if !ok {
		panic("invalid fs")
	}
	return pebble.NewKVStore(config, dir, wal, fs)
}
