package api

import (
	"context"
	"fmt"
	"sync"

	"github.com/Anon-HP/Aletheia/internal/consensus"
	"github.com/Anon-HP/Aletheia/internal/core"
	"github.com/Anon-HP/Aletheia/internal/node"
	"github.com/Anon-HP/Aletheia/internal/routing"
)

// PeerTransport is the HTTP implementation of node.Broadcaster (DOMAINS §3.5,
// DR-10 §6): it relays blocks to the node's peers with POST /block and
// fetches a peer's blocks after a locator with GET /blocks?locator=… (DR-14).
//
// BroadcastBlock does not wait: each peer gets its own goroutine, so a slow
// or unreachable peer delays nobody, and a relay chain A → B → C never nests
// one request inside another's timeout. A peer that receives blocks out of
// order syncs by locator, so delivery order does not matter.
type PeerTransport struct {
	c     *Client
	self  string   // this node's listen address, sent as "from"
	peers []string // host:port of every peer
	logf  func(format string, args ...any)

	wg sync.WaitGroup // broadcasts in flight
}

var _ node.Broadcaster = (*PeerTransport)(nil)

// NewPeerTransport creates the transport of the node listening at self
// (host:port, reachable by the peers). An entry of peers equal to self is
// ignored. logf, if set, receives one line per failed relay.
func NewPeerTransport(c *Client, self string, peers []string, logf func(format string, args ...any)) *PeerTransport {
	var ps []string
	for _, p := range peers {
		if p != self {
			ps = append(ps, p)
		}
	}
	return &PeerTransport{c: c, self: self, peers: ps, logf: logf}
}

// Peers returns the peer addresses.
func (t *PeerTransport) Peers() []string { return append([]string(nil), t.peers...) }

// BroadcastBlock sends b to every peer in the background, each POST limited
// by routing.RequestTimeout. Failures are logged, not retried: a peer that
// missed a block catches up through the next one by locator sync.
func (t *PeerTransport) BroadcastBlock(b core.Block) {
	for _, p := range t.peers {
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), routing.RequestTimeout)
			defer cancel()
			if _, err := t.c.PostBlock(ctx, p, b, t.self); err != nil && t.logf != nil {
				t.logf("relay of block %s to %s failed: %v", core.HeaderHash(b.Header), p, err)
			}
		}()
	}
}

// Wait blocks until every broadcast started so far has finished.
func (t *PeerTransport) Wait() { t.wg.Wait() }

// FetchBlocks asks peer for its fork point and blocks after loc (one round
// trip, routing.RequestTimeout). Every error wraps ErrFetch.
func (t *PeerTransport) FetchBlocks(peer string, loc []consensus.LocatorEntry) (uint64, []core.Block, error) {
	ctx, cancel := context.WithTimeout(context.Background(), routing.RequestTimeout)
	defer cancel()
	fork, bs, err := t.c.BlocksAfter(ctx, peer, loc)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %s: %w", ErrFetch, peer, err)
	}
	return fork, bs, nil
}
