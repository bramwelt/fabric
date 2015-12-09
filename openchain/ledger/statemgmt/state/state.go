/*
Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements.  See the NOTICE file
distributed with this work for additional information
regarding copyright ownership.  The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License.  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied.  See the License for the
specific language governing permissions and limitations
under the License.
*/

package state

import (
	"encoding/binary"
	"fmt"

	"github.com/op/go-logging"
	"github.com/openblockchain/obc-peer/openchain/db"
	"github.com/openblockchain/obc-peer/openchain/ledger/statemgmt"
	"github.com/openblockchain/obc-peer/openchain/ledger/statemgmt/buckettree"
	"github.com/tecbot/gorocksdb"
)

var logger = logging.MustGetLogger("state")

// these be configurable in yaml?
var historyStateDeltaSize = uint64(500)
var stateImpl = buckettree.NewStateImpl()

// State structure for maintaining world state.
// This encapsulates a particular implementation for managing the state persistence
// This is not thread safe
type State struct {
	stateImpl           statemgmt.HashableState
	stateDelta          *statemgmt.StateDelta
	currentTxStateDelta *statemgmt.StateDelta
	currentTxUUID       string
	txStateDeltaHash    map[string][]byte
	updateStateImpl     bool
}

// NewState constructs a new State. This Initializes encapsulated state implementation
func NewState() *State {
	err := stateImpl.Initialize()
	if err != nil {
		panic(fmt.Errorf("Error during initialization of state implementation: %s", err))
	}
	return &State{stateImpl, statemgmt.NewStateDelta(), statemgmt.NewStateDelta(), "", make(map[string][]byte), false}

}

// TxBegin marks begin of a new tx. If a tx is already in progress, this call panics
func (state *State) TxBegin(txUUID string) {
	logger.Debug("txBegin() for txUuid [%s]", txUUID)
	if state.txInProgress() {
		panic(fmt.Errorf("A tx [%s] is already in progress. Received call for begin of another tx [%s]", state.currentTxUUID, txUUID))
	}
	state.currentTxUUID = txUUID
}

// TxFinish marks the completion of on-going tx. If txUUID is not same as of the on-going tx, this call panics
func (state *State) TxFinish(txUUID string, txSuccessful bool) {
	logger.Debug("txFinish() for txUuid [%s], txSuccessful=[%t]", txUUID, txSuccessful)
	if state.currentTxUUID != txUUID {
		panic(fmt.Errorf("Different Uuid in tx-begin [%s] and tx-finish [%s]", state.currentTxUUID, txUUID))
	}
	if txSuccessful {
		if !state.currentTxStateDelta.IsEmpty() {
			logger.Debug("txFinish() for txUuid [%s] merging state changes", txUUID)
			state.stateDelta.ApplyChanges(state.currentTxStateDelta)
			state.txStateDeltaHash[txUUID] = state.currentTxStateDelta.ComputeCryptoHash()
			state.updateStateImpl = true
		} else {
			state.txStateDeltaHash[txUUID] = nil
		}
	}
	state.currentTxStateDelta = statemgmt.NewStateDelta()
	state.currentTxUUID = ""
}

func (state *State) txInProgress() bool {
	return state.currentTxUUID != ""
}

// Get returns state for chaincodeID and key. If committed is false, this first looks in memory and if missing,
// pulls from db. If committed is true, this pulls from the db only.
func (state *State) Get(chaincodeID string, key string, committed bool) ([]byte, error) {
	if !committed {
		valueHolder := state.currentTxStateDelta.Get(chaincodeID, key)
		if valueHolder != nil {
			return valueHolder.GetValue(), nil
		}
		valueHolder = state.stateDelta.Get(chaincodeID, key)
		if valueHolder != nil {
			return valueHolder.GetValue(), nil
		}
	}
	return state.stateImpl.Get(chaincodeID, key)
}

// Set sets state to given value for chaincodeID and key. Does not immideatly writes to DB
func (state *State) Set(chaincodeID string, key string, value []byte) error {
	logger.Debug("set() chaincodeID=[%s], key=[%s], value=[%#v]", chaincodeID, key, value)
	if !state.txInProgress() {
		panic("State can be changed only in context of a tx.")
	}
	state.currentTxStateDelta.Set(chaincodeID, key, value)
	return nil
}

// Delete tracks the deletion of state for chaincodeID and key. Does not immideatly writes to DB
func (state *State) Delete(chaincodeID string, key string) error {
	logger.Debug("delete() chaincodeID=[%s], key=[%s]", chaincodeID, key)
	if !state.txInProgress() {
		panic("State can be changed only in context of a tx.")
	}
	state.currentTxStateDelta.Delete(chaincodeID, key)
	return nil
}

// GetHash computes new state hash if the stateDelta is to be applied.
// Recomputes only if stateDelta has changed after most recent call to this function
func (state *State) GetHash() ([]byte, error) {
	logger.Debug("Enter - GetHash()")
	if state.updateStateImpl {
		logger.Debug("updating stateImpl with working-set")
		state.stateImpl.PrepareWorkingSet(state.stateDelta)
		state.updateStateImpl = false
	}
	hash, err := state.stateImpl.ComputeCryptoHash()
	if err != nil {
		return nil, err
	}
	logger.Debug("Exit - GetHash()")
	return hash, nil
}

// GetTxStateDeltaHash return the hash of the StateDelta
func (state *State) GetTxStateDeltaHash() map[string][]byte {
	return state.txStateDeltaHash
}

// ClearInMemoryChanges remove from memory all the changes to state
func (state *State) ClearInMemoryChanges() {
	state.stateDelta = statemgmt.NewStateDelta()
	state.txStateDeltaHash = make(map[string][]byte)
	state.stateImpl.ClearWorkingSet()
}

// getStateDelta get changes in state after most recent call to method clearInMemoryChanges
func (state *State) getStateDelta() *statemgmt.StateDelta {
	return state.stateDelta
}

// GetSnapshot returns a snapshot of the global state for the current block. stateSnapshot.Release()
// must be called once you are done.
func (state *State) GetSnapshot(blockNumber uint64, dbSnapshot *gorocksdb.Snapshot) (*StateSnapshot, error) {
	return newStateSnapshot(blockNumber, dbSnapshot)
}

// FetchStateDeltaFromDB fetches the StateDelta corrsponding to given blockNumber
func (state *State) FetchStateDeltaFromDB(blockNumber uint64) (*statemgmt.StateDelta, error) {
	stateDeltaBytes, err := db.GetDBHandle().GetFromStateDeltaCF(encodeStateDeltaKey(blockNumber))
	if err != nil {
		return nil, err
	}
	if stateDeltaBytes == nil {
		return nil, nil
	}
	stateDelta := statemgmt.NewStateDelta()
	stateDelta.Unmarshal(stateDeltaBytes)
	return stateDelta, nil
}

// AddChangesForPersistence adds key-value pairs to writeBatch
func (state *State) AddChangesForPersistence(blockNumber uint64, writeBatch *gorocksdb.WriteBatch) {
	logger.Debug("state.addChangesForPersistence()...start")
	if state.updateStateImpl {
		state.stateImpl.PrepareWorkingSet(state.stateDelta)
		state.updateStateImpl = false
	}
	state.stateImpl.AddChangesForPersistence(writeBatch)

	serializedStateDelta := state.stateDelta.Marshal()
	cf := db.GetDBHandle().StateDeltaCF
	logger.Debug("Adding state-delta corresponding to block number[%d]", blockNumber)
	writeBatch.PutCF(cf, encodeStateDeltaKey(blockNumber), serializedStateDelta)
	if blockNumber >= historyStateDeltaSize {
		blockNumberToDelete := blockNumber - historyStateDeltaSize
		logger.Debug("Deleting state-delta corresponding to block number[%d]", blockNumberToDelete)
		writeBatch.DeleteCF(cf, encodeStateDeltaKey(blockNumberToDelete))
	} else {
		logger.Debug("Not deleting previous state-delta. Block number [%d] is smaller than historyStateDeltaSize [%d]",
			blockNumber, historyStateDeltaSize)
	}
	logger.Debug("state.addChangesForPersistence()...finished")
}

// ApplyStateDelta applies already prepared stateDelta to the existing state
// This method is to be used in state transfer
func (state *State) ApplyStateDelta(delta *statemgmt.StateDelta) error {
	state.stateImpl.PrepareWorkingSet(delta)
	writeBatch := gorocksdb.NewWriteBatch()
	state.stateImpl.AddChangesForPersistence(writeBatch)
	opt := gorocksdb.NewDefaultWriteOptions()
	err := db.GetDBHandle().DB.Write(opt, writeBatch)
	if err != nil {
		return err
	}
	return nil
}

// DeleteState deletes ALL state keys/values from the DB. This is generally
// only used during state synchronization when creating a new state from
// a snapshot.
func (state *State) DeleteState() error {
	state.ClearInMemoryChanges()
	err := db.GetDBHandle().DeleteState()
	if err != nil {
		logger.Error("Error deleting state", err)
	}
	return err
}

func encodeStateDeltaKey(blockNumber uint64) []byte {
	return encodeUint64(blockNumber)
}

func decodeStateDeltaKey(dbkey []byte) uint64 {
	return decodeToUint64(dbkey)
}

func encodeUint64(number uint64) []byte {
	bytes := make([]byte, 8)
	binary.BigEndian.PutUint64(bytes, number)
	return bytes
}

func decodeToUint64(bytes []byte) uint64 {
	return binary.BigEndian.Uint64(bytes)
}