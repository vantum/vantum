// Copyright 2018 The Vantum Authors
// This file is part of the vantum library.
//
// The vantum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The vantum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the vantum library. If not, see <http://www.gnu.org/licenses/>.

package masternode

import (
	"bytes"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vantum/vantum/common"
	"github.com/vantum/vantum/contracts/masternode/contract"
	"github.com/vantum/vantum/core"
	"github.com/vantum/vantum/core/types"
	"github.com/vantum/vantum/core/types/devotedb"
	"github.com/vantum/vantum/core/types/masternode"
	"github.com/vantum/vantum/event"
	"github.com/vantum/vantum/log"
	"github.com/vantum/vantum/p2p"
	"github.com/vantum/vantum/params"
)

var (
	statsReportInterval = 10 * time.Second // Time interval to report vote pool stats
)

type MasternodeManager struct {
	beats map[common.Hash]time.Time // Last heartbeat from each known vote

	devoteDB *devotedb.DevoteDB
	active   *masternode.ActiveMasternode
	mu       sync.Mutex
	// channels for fetcher, syncer, txsyncLoop
	newPeerCh    chan *peer
	IsMasternode uint32
	srvr         *p2p.Server
	contract     *contract.Contract
	blockchain   *core.BlockChain
	scope        event.SubscriptionScope

	currentCycle uint64        // Current vote of the block chain
	Lifetime     time.Duration // Maximum amount of time vote are queued

	txPool *core.TxPool
}

func NewMasternodeManager(dp *devotedb.DevoteDB, blockchain *core.BlockChain, contract *contract.Contract, txPool *core.TxPool) *MasternodeManager {

	// Create the masternode manager with its initial settings
	manager := &MasternodeManager{
		devoteDB:   dp,
		blockchain: blockchain,
		beats:      make(map[common.Hash]time.Time),
		Lifetime:   30 * time.Second,
		contract:   contract,
		txPool:     txPool,
	}
	return manager
}

func (self *MasternodeManager) Clear() {
	self.mu.Lock()
	defer self.mu.Unlock()

}

func (self *MasternodeManager) Start(srvr *p2p.Server, peers *peerSet) {
	self.srvr = srvr
	log.Trace("MasternodeManqager start ")
	self.active = masternode.NewActiveMasternode(srvr)
	go self.masternodeLoop()
}

func (self *MasternodeManager) Stop() {

}

func (mm *MasternodeManager) masternodeLoop() {
	var id [8]byte
	copy(id[:], mm.srvr.Self().ID[0:8])
	has, err := mm.contract.Has(nil, id)
	if err != nil {
		log.Error("contract.Has", "error", err)
	}
	if has {
		fmt.Println("### It's already been a masternode! ")
		atomic.StoreUint32(&mm.IsMasternode, 1)
		mm.updateActiveMasternode(true)
	} else if mm.srvr.IsMasternode {
		mm.updateActiveMasternode(false)
		data := "0x2f926732" + common.Bytes2Hex(mm.srvr.Self().ID[:])
		fmt.Printf("### Masternode Transaction Data: %s\n", data)
	}

	joinCh := make(chan *contract.ContractJoin, 32)
	quitCh := make(chan *contract.ContractQuit, 32)
	joinSub, err1 := mm.contract.WatchJoin(nil, joinCh)
	if err1 != nil {
		// TODO: exit
		return
	}
	quitSub, err2 := mm.contract.WatchQuit(nil, quitCh)
	if err2 != nil {
		// TODO: exit
		return
	}

	ping := time.NewTimer(masternode.MASTERNODE_PING_INTERVAL)
	defer ping.Stop()
	minPower := big.NewInt(20e+14)

	report := time.NewTicker(statsReportInterval)
	defer report.Stop()

	for {
		select {
		case join := <-joinCh:
			if bytes.Equal(join.Id[:], mm.srvr.Self().ID[0:8]) {
				atomic.StoreUint32(&mm.IsMasternode, 1)
				mm.updateActiveMasternode(true)
				fmt.Println("### It's already been a masternode! ")
			}
		case quit := <-quitCh:
			if bytes.Equal(quit.Id[:], mm.srvr.Self().ID[0:8]) {
				atomic.StoreUint32(&mm.IsMasternode, 0)
				mm.updateActiveMasternode(false)
				fmt.Println("### Remove masternode! ")
			}
		case err := <-joinSub.Err():
			joinSub.Unsubscribe()
			fmt.Println("eventJoin err", err.Error())
		case err := <-quitSub.Err():
			quitSub.Unsubscribe()
			fmt.Println("eventQuit err", err.Error())

		case <-ping.C:
			ping.Reset(masternode.MASTERNODE_PING_INTERVAL)
			if mm.active.State() != masternode.ACTIVE_MASTERNODE_STARTED {
				break
			}

			address := mm.active.NodeAccount
			stateDB, _ := mm.blockchain.State()
			if stateDB.GetBalance(address).Cmp(big.NewInt(1e+16)) < 0 {
				fmt.Println("Failed to deposit 0.01 etz to ", address.String())
				break
			}
			if stateDB.GetPower(address, mm.blockchain.CurrentBlock().Number()).Cmp(minPower) < 0 {
				fmt.Println("Insufficient power for ping transaction.", address.Hex(), mm.blockchain.CurrentBlock().Number().String(), stateDB.GetPower(address, mm.blockchain.CurrentBlock().Number()).String())
				break
			}
			tx := types.NewTransaction(
				mm.txPool.State().GetNonce(address),
				params.MasterndeContractAddress,
				big.NewInt(0),
				90000,
				big.NewInt(20e+9),
				nil,
			)
			signed, err := types.SignTx(tx, types.NewEIP155Signer(mm.blockchain.Config().ChainID), mm.active.PrivateKey)
			if err != nil {
				fmt.Println("SignTx error:", err)
				break
			}

			if err := mm.txPool.AddLocal(signed); err != nil {
				fmt.Println("send ping to txpool error:", err)
				break
			}
			fmt.Println("Send ping message ...")
		}
	}
}

//func (mm *MasternodeManager) ProcessPingMsg(pm *masternode.PingMsg) error {
//	if mm.masternodes == nil {
//		return nil
//	}
//	var b [8]byte
//	binary.BigEndian.PutUint64(b[:], pm.Time)
//	key, err := secp256k1.RecoverPubkey(crypto.Keccak256(b[:]), pm.Sig)
//	if err != nil || len(key) != 65 {
//		return err
//	}
//	id := fmt.Sprintf("%x", key[1:9])
//	node := mm.masternodes.Node(id)
//	if node == nil {
//		return fmt.Errorf("error id %s", id)
//	}
//
//	if node.LastPingTime > pm.Time {
//		return fmt.Errorf("error ping time: %d > %d", node.LastPingTime, pm.Time)
//	}
//
//	// mark the ping message
//	for _, v := range mm.peers.peers { //
//		v.markPingMsg(id, pm.Time)
//	}
//	mm.masternodes.RecvPingMsg(id, pm.Time)
//	return nil
//}

func (mm *MasternodeManager) updateActiveMasternode(isMasternode bool) {
	var state int
	if isMasternode {
		state = masternode.ACTIVE_MASTERNODE_STARTED
	} else {
		state = masternode.ACTIVE_MASTERNODE_NOT_CAPABLE

	}
	mm.active.SetState(state)
}

func (self *MasternodeManager) MasternodeList(number *big.Int) ([]string, error) {
	return masternode.GetIdsByBlockNumber(self.contract, number)
}

func (self *MasternodeManager) GetGovernanceContractAddress(number *big.Int) (common.Address, error) {
	return masternode.GetGovernanceAddress(self.contract, number)
}
