// Copyright 2017-2019 Lei Ni (nilei81@gmail.com)
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

package logdb

import (
	"encoding/binary"
	"math"

	"github.com/lni/dragonboat/internal/settings"
	"github.com/lni/dragonboat/raftio"
	pb "github.com/lni/dragonboat/raftpb"
)

var (
	batchSize = settings.Hard.LogDBEntryBatchSize
)

type entryManager interface {
	binaryFormat() uint32
	record(wb IWriteBatch,
		clusterID uint64, nodeID uint64,
		ctx raftio.IContext, entries []pb.Entry) uint64
	iterate(ents []pb.Entry, maxIndex uint64,
		size uint64, clusterID uint64, nodeID uint64,
		low uint64, high uint64, maxSize uint64) ([]pb.Entry, uint64, error)
	getRange(clusterID uint64,
		nodeID uint64, lastIndex uint64, maxIndex uint64) (uint64, uint64, error)
	rangedOp(clusterID uint64,
		nodeID uint64, index uint64,
		op func(fk *PooledKey, lk *PooledKey) error) error
}

// rdb is the struct used to manage rocksdb backed persistent Log stores.
type rdb struct {
	cs      *rdbcache
	keys    *logdbKeyPool
	kvs     IKvStore
	entries entryManager
}

func openRDB(dir string, wal string, batched bool) (*rdb, error) {
	kvs, err := newKVStore(dir, wal)
	if err != nil {
		return nil, err
	}
	cs := newRDBCache()
	pool := newLogdbKeyPool()
	var em entryManager
	if batched {
		em = newBatchedEntries(cs, pool, kvs)
	} else {
		em = newPlainEntries(cs, pool, kvs)
	}
	return &rdb{
		cs:      cs,
		keys:    pool,
		kvs:     kvs,
		entries: em,
	}, nil
}

func (r *rdb) selfCheckFailed() (bool, error) {
	fk := newKey(entryKeySize, nil)
	lk := newKey(entryKeySize, nil)
	_, ok := r.entries.(*batchedEntries)
	if ok {
		fk.SetEntryBatchKey(0, 0, 0)
		lk.SetEntryBatchKey(math.MaxUint64, math.MaxUint64, math.MaxUint64)
	} else {
		fk.SetEntryBatchKey(0, 0, 0)
		lk.SetEntryBatchKey(math.MaxUint64, math.MaxUint64, math.MaxUint64)
	}
	located := false
	op := func(key []byte, data []byte) (bool, error) {
		located = true
		return false, nil
	}
	if err := r.kvs.IterateValue(fk.Key(), lk.Key(), true, op); err != nil {
		return false, err
	}
	return located, nil
}

func (r *rdb) binaryFormat() uint32 {
	return r.entries.binaryFormat()
}

func (r *rdb) close() {
	if err := r.kvs.Close(); err != nil {
		panic(err)
	}
}

func (r *rdb) getWriteBatch() IWriteBatch {
	return r.kvs.GetWriteBatch(nil)
}

func (r *rdb) listNodeInfo() ([]raftio.NodeInfo, error) {
	fk := newKey(bootstrapKeySize, nil)
	lk := newKey(bootstrapKeySize, nil)
	fk.setBootstrapKey(0, 0)
	lk.setBootstrapKey(math.MaxUint64, math.MaxUint64)
	ni := make([]raftio.NodeInfo, 0)
	op := func(key []byte, data []byte) (bool, error) {
		cid, nid := parseNodeInfoKey(key)
		ni = append(ni, raftio.GetNodeInfo(cid, nid))
		return true, nil
	}
	r.kvs.IterateValue(fk.Key(), lk.Key(), true, op)
	return ni, nil
}

func (r *rdb) readRaftState(clusterID uint64,
	nodeID uint64, lastIndex uint64) (*raftio.RaftState, error) {
	firstIndex, length, err := r.getRange(clusterID, nodeID, lastIndex)
	if err != nil {
		return nil, err
	}
	state, err := r.readState(clusterID, nodeID)
	if err != nil {
		return nil, err
	}
	rs := &raftio.RaftState{
		State:      state,
		FirstIndex: firstIndex,
		EntryCount: length,
	}
	return rs, nil
}

func (r *rdb) getRange(clusterID uint64,
	nodeID uint64, lastIndex uint64) (uint64, uint64, error) {
	maxIndex, err := r.readMaxIndex(clusterID, nodeID)
	if err == raftio.ErrNoSavedLog {
		return lastIndex, 0, nil
	}
	if err != nil {
		panic(err)
	}
	return r.entries.getRange(clusterID, nodeID, lastIndex, maxIndex)
}

func (r *rdb) saveRaftState(updates []pb.Update,
	ctx raftio.IContext) error {
	wb := r.kvs.GetWriteBatch(ctx)
	for _, ud := range updates {
		r.recordState(ud.ClusterID, ud.NodeID, ud.State, wb, ctx)
		if !pb.IsEmptySnapshot(ud.Snapshot) {
			if len(ud.EntriesToSave) > 0 {
				if ud.Snapshot.Index > ud.EntriesToSave[len(ud.EntriesToSave)-1].Index {
					plog.Panicf("max index not handled, %d, %d",
						ud.Snapshot.Index, ud.EntriesToSave[len(ud.EntriesToSave)-1].Index)
				}
			}
			r.recordSnapshot(wb, ud)
			r.setMaxIndex(wb, ud, ud.Snapshot.Index, ctx)
		}
	}
	r.saveEntries(updates, wb, ctx)
	if wb.Count() > 0 {
		return r.kvs.CommitWriteBatch(wb)
	}
	return nil
}

func (r *rdb) importSnapshot(ss pb.Snapshot, nodeID uint64) error {
	if ss.Type == pb.UnknownStateMachine {
		panic("Unknown state machine type")
	}
	snapshots, err := r.listSnapshots(ss.ClusterId, nodeID)
	if err != nil {
		return err
	}
	selectedss := make([]pb.Snapshot, 0)
	for _, curss := range snapshots {
		if curss.Index >= ss.Index {
			selectedss = append(selectedss, curss)
		}
	}
	wb := r.getWriteBatch()
	bsrec := pb.Bootstrap{Join: true, Type: ss.Type}
	state := pb.State{Term: ss.Term, Commit: ss.Index}
	r.recordRemoveNodeData(wb, selectedss, ss.ClusterId, nodeID)
	r.recordBootstrap(wb, ss.ClusterId, nodeID, bsrec)
	r.recordStateAllocs(wb, ss.ClusterId, nodeID, state)
	r.recordSnapshot(wb, pb.Update{
		ClusterID: ss.ClusterId, NodeID: nodeID, Snapshot: ss,
	})
	return r.kvs.CommitWriteBatch(wb)
}

func (r *rdb) setMaxIndex(wb IWriteBatch,
	ud pb.Update, maxIndex uint64, ctx raftio.IContext) {
	r.cs.setMaxIndex(ud.ClusterID, ud.NodeID, maxIndex)
	r.recordMaxIndex(wb, ud.ClusterID, ud.NodeID, maxIndex, ctx)
}

func (r *rdb) recordBootstrap(wb IWriteBatch,
	clusterID uint64, nodeID uint64, bsrec pb.Bootstrap) {
	bskey := newKey(maxKeySize, nil)
	bskey.setBootstrapKey(clusterID, nodeID)
	bsdata, err := bsrec.Marshal()
	if err != nil {
		panic(err)
	}
	wb.Put(bskey.Key(), bsdata)
}

func (r *rdb) recordSnapshot(wb IWriteBatch, ud pb.Update) {
	if pb.IsEmptySnapshot(ud.Snapshot) {
		return
	}
	ko := newKey(snapshotKeySize, nil)
	ko.setSnapshotKey(ud.ClusterID, ud.NodeID, ud.Snapshot.Index)
	data, err := ud.Snapshot.Marshal()
	if err != nil {
		panic(err)
	}
	wb.Put(ko.Key(), data)
}

func (r *rdb) recordMaxIndex(wb IWriteBatch,
	clusterID uint64, nodeID uint64, index uint64, ctx raftio.IContext) {
	data := ctx.GetValueBuffer(8)
	binary.BigEndian.PutUint64(data, index)
	data = data[:8]
	ko := ctx.GetKey()
	ko.SetMaxIndexKey(clusterID, nodeID)
	wb.Put(ko.Key(), data)
}

func (r *rdb) recordStateAllocs(wb IWriteBatch,
	clusterID uint64, nodeID uint64, st pb.State) {
	data, err := st.Marshal()
	if err != nil {
		panic(err)
	}
	key := newKey(snapshotKeySize, nil)
	key.SetStateKey(clusterID, nodeID)
	wb.Put(key.Key(), data)
}

func (r *rdb) recordState(clusterID uint64,
	nodeID uint64, st pb.State, wb IWriteBatch, ctx raftio.IContext) {
	if pb.IsEmptyState(st) {
		return
	}
	if !r.cs.setState(clusterID, nodeID, st) {
		return
	}
	data := ctx.GetValueBuffer(uint64(st.Size()))
	ms, err := st.MarshalTo(data)
	if err != nil {
		panic(err)
	}
	data = data[:ms]
	ko := ctx.GetKey()
	ko.SetStateKey(clusterID, nodeID)
	wb.Put(ko.Key(), data)
}

func (r *rdb) saveBootstrapInfo(clusterID uint64,
	nodeID uint64, bootstrap pb.Bootstrap) error {
	wb := r.getWriteBatch()
	r.recordBootstrap(wb, clusterID, nodeID, bootstrap)
	return r.kvs.CommitWriteBatch(wb)
}

func (r *rdb) getBootstrapInfo(clusterID uint64,
	nodeID uint64) (*pb.Bootstrap, error) {
	ko := newKey(maxKeySize, nil)
	ko.setBootstrapKey(clusterID, nodeID)
	bootstrap := &pb.Bootstrap{}
	if err := r.kvs.GetValue(ko.Key(), func(data []byte) error {
		if len(data) == 0 {
			return raftio.ErrNoBootstrapInfo
		}
		if err := bootstrap.Unmarshal(data); err != nil {
			panic(err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return bootstrap, nil
}

func (r *rdb) saveSnapshots(updates []pb.Update) error {
	wb := r.kvs.GetWriteBatch(nil)
	defer wb.Destroy()
	toSave := false
	for _, ud := range updates {
		if ud.Snapshot.Index > 0 {
			r.recordSnapshot(wb, ud)
			toSave = true
		}
	}
	if toSave {
		return r.kvs.CommitWriteBatch(wb)
	}
	return nil
}

func (r *rdb) deleteSnapshot(clusterID uint64,
	nodeID uint64, snapshotIndex uint64) error {
	ko := r.keys.get()
	defer ko.Release()
	ko.setSnapshotKey(clusterID, nodeID, snapshotIndex)
	return r.kvs.DeleteValue(ko.Key())
}

func (r *rdb) listSnapshots(clusterID uint64,
	nodeID uint64) ([]pb.Snapshot, error) {
	fk := r.keys.get()
	lk := r.keys.get()
	defer fk.Release()
	defer lk.Release()
	fk.setSnapshotKey(clusterID, nodeID, 0)
	lk.setSnapshotKey(clusterID, nodeID, math.MaxUint64)
	snapshots := make([]pb.Snapshot, 0)
	op := func(key []byte, data []byte) (bool, error) {
		var ss pb.Snapshot
		if err := ss.Unmarshal(data); err != nil {
			panic(err)
		}
		snapshots = append(snapshots, ss)
		return true, nil
	}
	r.kvs.IterateValue(fk.Key(), lk.Key(), true, op)
	return snapshots, nil
}

func (r *rdb) readMaxIndex(clusterID uint64, nodeID uint64) (uint64, error) {
	if v, ok := r.cs.getMaxIndex(clusterID, nodeID); ok {
		return v, nil
	}
	ko := r.keys.get()
	defer ko.Release()
	ko.SetMaxIndexKey(clusterID, nodeID)
	maxIndex := uint64(0)
	if err := r.kvs.GetValue(ko.Key(), func(data []byte) error {
		if len(data) == 0 {
			return raftio.ErrNoSavedLog
		}
		maxIndex = binary.BigEndian.Uint64(data)
		return nil
	}); err != nil {
		return 0, err
	}
	return maxIndex, nil
}

func (r *rdb) readState(clusterID uint64,
	nodeID uint64) (*pb.State, error) {
	ko := r.keys.get()
	defer ko.Release()
	ko.SetStateKey(clusterID, nodeID)
	hs := &pb.State{}
	if err := r.kvs.GetValue(ko.Key(), func(data []byte) error {
		if len(data) == 0 {
			return raftio.ErrNoSavedLog
		}
		if err := hs.Unmarshal(data); err != nil {
			panic(err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return hs, nil
}

func (r *rdb) removeEntriesTo(clusterID uint64,
	nodeID uint64, index uint64) error {
	op := func(fk *PooledKey, lk *PooledKey) error {
		return r.kvs.RemoveEntries(fk.Key(), lk.Key())
	}
	return r.entries.rangedOp(clusterID, nodeID, index, op)
}

func (r *rdb) removeNodeData(clusterID uint64, nodeID uint64) error {
	wb := r.getWriteBatch()
	defer wb.Clear()
	snapshots, err := r.listSnapshots(clusterID, nodeID)
	if err != nil {
		return err
	}
	r.recordRemoveNodeData(wb, snapshots, clusterID, nodeID)
	if err := r.kvs.CommitDeleteBatch(wb); err != nil {
		return err
	}
	if err := r.removeEntriesTo(clusterID, nodeID, math.MaxUint64); err != nil {
		return err
	}
	return r.compaction(clusterID, nodeID, math.MaxUint64)
}

func (r *rdb) recordRemoveNodeData(wb IWriteBatch,
	snapshots []pb.Snapshot, clusterID uint64, nodeID uint64) {
	stateKey := newKey(maxKeySize, nil)
	stateKey.SetStateKey(clusterID, nodeID)
	wb.Delete(stateKey.Key())
	bootstrapKey := newKey(maxKeySize, nil)
	bootstrapKey.setBootstrapKey(clusterID, nodeID)
	wb.Delete(bootstrapKey.Key())
	maxIndexKey := newKey(maxKeySize, nil)
	maxIndexKey.SetMaxIndexKey(clusterID, nodeID)
	wb.Delete(maxIndexKey.Key())
	for _, ss := range snapshots {
		key := newKey(maxKeySize, nil)
		key.setSnapshotKey(clusterID, nodeID, ss.Index)
		wb.Delete(key.Key())
	}
}

func (r *rdb) compaction(clusterID uint64, nodeID uint64, index uint64) error {
	op := func(fk *PooledKey, lk *PooledKey) error {
		return r.kvs.Compaction(fk.Key(), lk.Key())
	}
	return r.entries.rangedOp(clusterID, nodeID, index, op)
}

func (r *rdb) saveEntries(updates []pb.Update,
	wb IWriteBatch, ctx raftio.IContext) {
	if len(updates) == 0 {
		return
	}
	for _, ud := range updates {
		clusterID := ud.ClusterID
		nodeID := ud.NodeID
		if len(ud.EntriesToSave) > 0 {
			mi := r.entries.record(wb, clusterID, nodeID, ctx, ud.EntriesToSave)
			if mi > 0 {
				r.setMaxIndex(wb, ud, mi, ctx)
			}
		}
	}
}

func (r *rdb) iterateEntries(ents []pb.Entry,
	size uint64, clusterID uint64, nodeID uint64, low uint64, high uint64,
	maxSize uint64) ([]pb.Entry, uint64, error) {
	maxIndex, err := r.readMaxIndex(clusterID, nodeID)
	if err == raftio.ErrNoSavedLog {
		return ents, size, nil
	}
	if err != nil {
		panic(err)
	}
	return r.entries.iterate(ents,
		maxIndex, size, clusterID, nodeID, low, high, maxSize)
}
