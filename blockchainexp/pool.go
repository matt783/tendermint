package blockchainexp

import (
	"sort"

	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/types"
)

type blockPool struct {
	logger log.Logger
	// Set of peers that have sent status responses, with height bigger than pool.Height
	peers map[p2p.ID]*bpPeer
	// Set of block heights and the corresponding peers from where a block response is expected or has been received.
	blocks map[int64]p2p.ID

	plannedRequests   map[int64]struct{} // list of blocks to be assigned peers for blockRequest
	nextRequestHeight int64              // next height to be added to plannedRequests

	Height        int64 // height of next block to execute
	MaxPeerHeight int64 // maximum height of all peers
	toBcR         bcReactor
}

// NewBlockPool creates a new blockPool.
func NewBlockPool(height int64, toBcR bcReactor) *blockPool {
	return &blockPool{
		peers:             make(map[p2p.ID]*bpPeer),
		MaxPeerHeight:     0,
		blocks:            make(map[int64]p2p.ID),
		plannedRequests:   make(map[int64]struct{}),
		nextRequestHeight: height,
		Height:            height,
		toBcR:             toBcR,
	}
}

// SetLogger sets the logger of the pool.
func (pool *blockPool) SetLogger(l log.Logger) {
	pool.logger = l
}

// ReachedMaxHeight check if the pool has reached the maximum peer height.
func (pool *blockPool) ReachedMaxHeight() bool {
	return pool.Height >= pool.MaxPeerHeight
}

func (pool *blockPool) rescheduleRequest(peerID p2p.ID, height int64) {
	pool.logger.Info("reschedule requests made to peer for height ", "peerID", peerID, "height", height)
	pool.plannedRequests[height] = struct{}{}
	delete(pool.blocks, height)
	pool.peers[peerID].RemoveBlock(height)
}

// Updates the pool's max height. If no peers are left MaxPeerHeight is set to 0.
func (pool *blockPool) updateMaxPeerHeight() {
	var newMax int64
	for _, peer := range pool.peers {
		peerHeight := peer.Height
		if peerHeight > newMax {
			newMax = peerHeight
		}
	}
	pool.MaxPeerHeight = newMax
}

// UpdatePeer adds a new peer or updates an existing peer with a new height.
// If a peer is too short it is not added.
func (pool *blockPool) UpdatePeer(peerID p2p.ID, height int64) error {

	peer := pool.peers[peerID]

	if peer == nil {
		if height < pool.Height {
			pool.logger.Info("Peer height too small",
				"peer", peerID, "height", height, "fsm_height", pool.Height)
			return errPeerTooShort
		}
		// Add new peer.
		peer = NewBPPeer(peerID, height, pool.toBcR.sendPeerError, nil)
		peer.SetLogger(pool.logger.With("peer", peerID))
		pool.peers[peerID] = peer
		pool.logger.Info("added peer", "peerID", peerID, "height", height, "num_peers", len(pool.peers))
	} else {
		// Check if peer is lowering its height. This is not allowed.
		if height < peer.Height {
			pool.RemovePeer(peerID, errPeerLowersItsHeight)
			return errPeerLowersItsHeight
		}
		// Update existing peer.
		peer.Height = height
	}

	// Update the pool's MaxPeerHeight if needed.
	pool.updateMaxPeerHeight()

	return nil
}

// Cleans and deletes the peer. Recomputes the max peer height.
func (pool *blockPool) deletePeer(peer *bpPeer) {
	if peer == nil {
		return
	}
	peer.Cleanup()
	delete(pool.peers, peer.ID)

	if peer.Height == pool.MaxPeerHeight {
		pool.updateMaxPeerHeight()
	}
}

// RemovePeer removes the blocks and requests from the peer, reschedules them and deletes the peer.
func (pool *blockPool) RemovePeer(peerID p2p.ID, err error) {
	peer := pool.peers[peerID]
	if peer == nil {
		return
	}
	pool.logger.Info("removing peer", "peerID", peerID, "error", err)

	// Reschedule the block requests made to the peer, or received and not processed yet.
	// Note that some of the requests may be removed further down.
	for h := range pool.peers[peerID].blocks {
		pool.rescheduleRequest(peerID, h)
	}

	oldMaxPeerHeight := pool.MaxPeerHeight
	// Delete the peer. This operation may result in the pool's MaxPeerHeight being lowered.
	pool.deletePeer(peer)

	// Check if the pool's MaxPeerHeight has been lowered.
	// This may happen if the tallest peer has been removed.
	if oldMaxPeerHeight > pool.MaxPeerHeight {
		// Remove any planned requests for heights over the new MaxPeerHeight.
		for h := range pool.plannedRequests {
			if h > pool.MaxPeerHeight {
				delete(pool.plannedRequests, h)
			}
		}
		// Adjust the nextRequestHeight to the new max plus one.
		if pool.nextRequestHeight > pool.MaxPeerHeight {
			pool.nextRequestHeight = pool.MaxPeerHeight + 1
		}
	}
}

func (pool *blockPool) removeShortPeers() {
	for _, peer := range pool.peers {
		if peer.Height < pool.Height {
			pool.RemovePeer(peer.ID, nil)
		}
	}
}

func (pool *blockPool) removeBadPeers() {
	pool.removeShortPeers()
	for _, peer := range pool.peers {
		if err := peer.CheckRate(); err != nil {
			pool.RemovePeer(peer.ID, err)
			pool.toBcR.sendPeerError(err, peer.ID)
		}
	}
}

// MakeNextRequests creates more requests if the block pool is running low.
func (pool *blockPool) MakeNextRequests(maxNumRequests int) {
	heights := pool.makeRequestBatch(maxNumRequests)
	pool.logger.Info("makeNextRequests will make following requests", "number", len(heights), "heights", heights)

	for _, height := range heights {
		h := int64(height)
		if !pool.sendRequest(h) {
			// If a good peer was not found for sending the request at height h then return,
			// as it shouldn't be possible to find a peer for h+1.
			return
		}
		delete(pool.plannedRequests, h)
	}
}

// Makes a batch of requests sorted by height such that the block pool has up to maxNumRequests entries.
func (pool *blockPool) makeRequestBatch(maxNumRequests int) []int {
	pool.removeBadPeers()
	// At this point pool.requests may include heights for requests to be redone due to removal of peers:
	// - peers timed out or were removed by switch
	// - FSM timed out on waiting to advance the block execution due to missing blocks at h or h+1
	// Determine the number of requests needed by subtracting the number of requests already made from the maximum
	// allowed
	numNeeded := int(maxNumRequests) - len(pool.blocks)
	for len(pool.plannedRequests) < numNeeded {
		if pool.nextRequestHeight > pool.MaxPeerHeight {
			break
		}
		pool.plannedRequests[pool.nextRequestHeight] = struct{}{}
		pool.nextRequestHeight++
	}

	heights := make([]int, 0, len(pool.plannedRequests))
	for k := range pool.plannedRequests {
		heights = append(heights, int(k))
	}
	sort.Ints(heights)
	return heights
}

func (pool *blockPool) sendRequest(height int64) bool {
	for _, peer := range pool.peers {
		if peer.NumPendingBlockRequests >= maxRequestsPerPeer {
			continue
		}
		if peer.Height < height {
			continue
		}

		err := pool.toBcR.sendBlockRequest(peer.ID, height)
		if err == errNilPeerForBlockRequest {
			// Switch does not have this peer, remove it and continue to look for another peer.
			pool.logger.Error("switch does not have peer..removing peer selected for height", "peer", peer.ID, "height", height)
			pool.RemovePeer(peer.ID, err)
			continue
		}

		if err == errSendQueueFull {
			pool.logger.Error("peer queue is full", "peer", peer.ID, "height", height)
			continue
		}

		pool.logger.Info("assigned request to peer", "peer", peer.ID, "height", height)

		pool.blocks[height] = peer.ID
		peer.RequestSent(height)

		return true
	}
	pool.logger.Error("could not find peer to send request for block at height", "height", height)
	return false
}

// Validates that the block comes from the peer it was expected from and stores it in the 'blocks' map.
func (pool *blockPool) AddBlock(peerID p2p.ID, block *types.Block, blockSize int) error {
	peer, ok := pool.peers[peerID]
	if !ok {
		pool.logger.Error("block from unknown peer", "height", block.Height, "peer", peerID)
		return errBadDataFromPeer
	}
	if wantPeerID, ok := pool.blocks[block.Height]; ok && wantPeerID != peerID {
		pool.logger.Error("block received from wrong peer", "height", block.Height,
			"peer", peerID, "expected_peer", wantPeerID)
		return errBadDataFromPeer
	}

	return peer.AddBlock(block, blockSize)
}

type blockData struct {
	block *types.Block
	peer  *bpPeer
}

// BlockAndPeerAtHeight retrieves the block and delivery peer at specified height.
// Returns errMissingBlock if a block was not found
func (pool *blockPool) BlockAndPeerAtHeight(height int64) (bData *blockData, err error) {
	peerID := pool.blocks[height]
	peer := pool.peers[peerID]
	if peer == nil {
		return nil, errMissingBlock
	}

	block, err := peer.BlockAtHeight(height)
	if err != nil {
		return nil, err
	}

	return &blockData{peer: peer, block: block}, nil

}

// FirstTwoBlocksAndPeers returns the blocks and the delivery peers at pool's height H and H+1.
func (pool *blockPool) FirstTwoBlocksAndPeers() (first, second *blockData, err error) {
	first, err = pool.BlockAndPeerAtHeight(pool.Height)
	second, err2 := pool.BlockAndPeerAtHeight(pool.Height + 1)
	if err == nil {
		err = err2
	}
	return
}

// InvalidateFirstTwoBlocks removes the peers that sent us the first two blocks, blocks are removed by RemovePeer().
func (pool *blockPool) InvalidateFirstTwoBlocks(err error) {
	first, err1 := pool.BlockAndPeerAtHeight(pool.Height)
	second, err2 := pool.BlockAndPeerAtHeight(pool.Height + 1)

	if err1 == nil {
		pool.RemovePeer(first.peer.ID, err)
	}
	if err2 == nil {
		pool.RemovePeer(second.peer.ID, err)
	}
}

// ProcessedCurrentHeightBlock performs cleanup after a block is processed. It removes block at pool height and
// the peers that are now short.
func (pool *blockPool) ProcessedCurrentHeightBlock() {
	peerID, peerOk := pool.blocks[pool.Height]
	if peerOk {
		pool.peers[peerID].RemoveBlock(pool.Height)
	}
	delete(pool.blocks, pool.Height)
	pool.logger.Debug("removed block at height", "height", pool.Height)
	pool.Height++
	pool.removeShortPeers()
}

// RemovePeerAtCurrentHeights checks if a block at pool's height H exists and if not, it removes the
// delivery peer and returns. If a block at height H exists then the check and peer removal is done for H+1.
// This function is called when the FSM is not able to make progress for some time.
// This happens if either the block H or H+1 have not been delivered.
func (pool *blockPool) RemovePeerAtCurrentHeights(err error) {
	peerID := pool.blocks[pool.Height]
	peer, ok := pool.peers[peerID]
	if ok {
		if _, err := peer.BlockAtHeight(pool.Height); err != nil {
			pool.logger.Info("remove peer that hasn't sent block at pool.Height",
				"peer", peerID, "height", pool.Height)
			pool.RemovePeer(peerID, err)
			return
		}
	}
	peerID = pool.blocks[pool.Height+1]
	peer, ok = pool.peers[peerID]
	if ok {
		if _, err := peer.BlockAtHeight(pool.Height + 1); err != nil {
			pool.logger.Info("remove peer that hasn't sent block at pool.Height+1",
				"peer", peerID, "height", pool.Height+1)
			pool.RemovePeer(peerID, err)
			return
		}
	}
}

// Cleanup performs pool and peer cleanup
func (pool *blockPool) Cleanup() {
	for _, peer := range pool.peers {
		peer.Cleanup()
	}
}

// NumPeers returns the number of peers in the pool
func (pool *blockPool) NumPeers() int {
	return len(pool.peers)
}

func (pool *blockPool) NeedsBlocks() bool {
	return len(pool.blocks) < maxNumRequests
}