package node

import (
	"github.com/Anon-HP/Aletheia/internal/consensus"
	"github.com/Anon-HP/Aletheia/internal/core"
)

// Broadcaster is the node's view of its peers. The node calls it without
// holding any of its locks, so an implementation may block (network I/O) or
// call back into this or another node.
//
// The HTTP implementation lives in the network layer; tests use an
// in-memory one.
type Broadcaster interface {
	// BroadcastBlock sends b to every peer: blocks this node mined, and
	// blocks it accepted from a peer (relay). A peer that already has b
	// ignores it, so relaying terminates.
	BroadcastBlock(b core.Block)

	// FetchBlocks sends this node's block locator to peer and returns the
	// peer's fork point (the highest locator height at which both chains
	// agree) and the peer's blocks after it, in height order. One round trip
	// (DECISIONS DR-14).
	FetchBlocks(peer string, loc []consensus.LocatorEntry) (fork uint64, blocks []core.Block, err error)
}

// noPeers is the Broadcaster of a node created without one: it sends
// nothing and has no peer to fetch from.
type noPeers struct{}

func (noPeers) BroadcastBlock(core.Block) {}

func (noPeers) FetchBlocks(string, []consensus.LocatorEntry) (uint64, []core.Block, error) {
	return 0, nil, ErrNoPeer
}
