// Copyright 2020 The Swarm Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package pushsync provides the pushsync protocol
// implementation.
package pushsync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethersphere/bee/pkg/accounting"
	"github.com/ethersphere/bee/pkg/content"
	"github.com/ethersphere/bee/pkg/logging"
	"github.com/ethersphere/bee/pkg/p2p"
	"github.com/ethersphere/bee/pkg/p2p/protobuf"
	"github.com/ethersphere/bee/pkg/pushsync/pb"
	"github.com/ethersphere/bee/pkg/soc"
	"github.com/ethersphere/bee/pkg/storage"
	"github.com/ethersphere/bee/pkg/swarm"
	"github.com/ethersphere/bee/pkg/tags"
	"github.com/ethersphere/bee/pkg/topology"
	"github.com/ethersphere/bee/pkg/tracing"
	opentracing "github.com/opentracing/opentracing-go"
)

const (
	protocolName    = "pushsync"
	protocolVersion = "1.0.0"
	streamName      = "pushsync"
)

const (
	maxPeers = 5
)

type PushSyncer interface {
	PushChunkToClosest(ctx context.Context, ch swarm.Chunk) (*Receipt, error)
}

type Receipt struct {
	Address swarm.Address
}

type PushSync struct {
	addr          swarm.Address // this node's overlay address
	streamer      p2p.StreamerDisconnecter
	storer        storage.Putter
	peerSuggester topology.Driver
	tagger        *tags.Tags
	unwrap        func(swarm.Chunk)
	logger        logging.Logger
	accounting    accounting.Interface
	pricer        accounting.Pricer
	metrics       metrics
	tracer        *tracing.Tracer
}

var timeToWaitForReceipt = 3 * time.Second // time to wait to get a receipt for a chunk

func New(addr swarm.Address, streamer p2p.StreamerDisconnecter, storer storage.Putter, closestPeerer topology.Driver, tagger *tags.Tags, unwrap func(swarm.Chunk), logger logging.Logger, accounting accounting.Interface, pricer accounting.Pricer, tracer *tracing.Tracer) *PushSync {
	ps := &PushSync{
		addr:          addr,
		streamer:      streamer,
		storer:        storer,
		peerSuggester: closestPeerer,
		tagger:        tagger,
		unwrap:        unwrap,
		logger:        logger,
		accounting:    accounting,
		pricer:        pricer,
		metrics:       newMetrics(),
		tracer:        tracer,
	}
	return ps
}

func (s *PushSync) Protocol() p2p.ProtocolSpec {
	return p2p.ProtocolSpec{
		Name:    protocolName,
		Version: protocolVersion,
		StreamSpecs: []p2p.StreamSpec{
			{
				Name:    streamName,
				Handler: s.handler,
			},
		},
	}
}

// handler handles chunk delivery from other node and forwards to its destination node.
// If the current node is the destination, it stores in the local store and sends a receipt.
func (ps *PushSync) handler(ctx context.Context, p p2p.Peer, stream p2p.Stream) (err error) {
	w, r := protobuf.NewWriterAndReader(stream)
	defer func() {
		if err != nil {
			ps.metrics.TotalErrors.Inc()
			_ = stream.Reset()
		} else {
			_ = stream.FullClose()
		}
	}()
	var ch pb.Delivery
	if err = r.ReadMsgWithContext(ctx, &ch); err != nil {
		return fmt.Errorf("pushsync read delivery: %w", err)
	}
	ps.metrics.TotalReceived.Inc()

	chunk := swarm.NewChunk(swarm.NewAddress(ch.Address), ch.Data)

	if content.Valid(chunk) {
		if ps.unwrap != nil {
			go ps.unwrap(chunk)
		}
	} else if !soc.Valid(chunk) {
		return swarm.ErrInvalidChunk
	}

	span, _, ctx := ps.tracer.StartSpanFromContext(ctx, "pushsync-handler", ps.logger, opentracing.Tag{Key: "address", Value: chunk.Address().String()})
	defer span.Finish()

	receipt, err := ps.pushToClosest(ctx, chunk, p.Address, false)
	if err != nil {
		if errors.Is(err, topology.ErrWantSelf) {

			// store the chunk in the local store
			_, err = ps.storer.Put(ctx, storage.ModePutSync, chunk)
			if err != nil {
				return fmt.Errorf("chunk store: %w", err)
			}

			receipt := pb.Receipt{Address: chunk.Address().Bytes()}
			if err := w.WriteMsg(&receipt); err != nil {
				return fmt.Errorf("send receipt to peer %s: %w", p.Address.String(), err)
			}

			return ps.accounting.Debit(p.Address, ps.pricer.Price(chunk.Address()))
		}
		return fmt.Errorf("handler: push to closest: %w", err)
	}

	// pass back the received receipt in the previously received stream
	ctx, cancel := context.WithTimeout(ctx, timeToWaitForReceipt)
	defer cancel()
	if err := w.WriteMsgWithContext(ctx, receipt); err != nil {
		return fmt.Errorf("send receipt to peer %s: %w", p.Address.String(), err)
	}

	return ps.accounting.Debit(p.Address, ps.pricer.Price(chunk.Address()))
}

// PushChunkToClosest sends chunk to the closest peer by opening a stream. It then waits for
// a receipt from that peer and returns error or nil based on the receiving and
// the validity of the receipt.
func (ps *PushSync) PushChunkToClosest(ctx context.Context, ch swarm.Chunk) (*Receipt, error) {
	r, err := ps.pushToClosest(ctx, ch, ps.addr, true)
	if err != nil {
		return nil, err
	}
	return &Receipt{Address: swarm.NewAddress(r.Address)}, nil
}

func (ps *PushSync) pushToClosest(ctx context.Context, ch swarm.Chunk, source swarm.Address, allowUpstream bool) (rr *pb.Receipt, reterr error) {
	span, logger, ctx := ps.tracer.StartSpanFromContext(ctx, "push-closest", ps.logger, opentracing.Tag{Key: "address", Value: ch.Address().String()})
	defer span.Finish()
	var (
		skipPeers = []swarm.Address{source}
		lastErr   error
	)

	deferFuncs := make([]func(), 0)
	defersFn := func() {
		if len(deferFuncs) > 0 {
			for _, deferFn := range deferFuncs {
				deferFn()
			}
			deferFuncs = deferFuncs[:0]
		}
	}
	defer defersFn()

	for i := 0; i < maxPeers; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		defersFn()

		deferFuncs = append(deferFuncs, func() {
			if lastErr != nil {
				ps.metrics.TotalErrors.Inc()
				logger.Errorf("pushsync: %v", lastErr)
			}
		})

		// find next closest peer
		peer, err := ps.closestPeer(ch.Address(), skipPeers, allowUpstream)
		if err != nil {
			// ClosestPeer can return ErrNotFound in case we are not connected to any peers
			// in which case we should return immediately.
			// if ErrWantSelf is returned, it means we are the closest peer.
			return nil, fmt.Errorf("closest peer: %w", err)
		}

		// save found peer (to be skipped if there is some error with him)
		skipPeers = append(skipPeers, peer)

		// compute the price we pay for this receipt and reserve it for the rest of this function
		receiptPrice := ps.pricer.PeerPrice(peer, ch.Address())
		err = ps.accounting.Reserve(ctx, peer, receiptPrice)
		if err != nil {
			return nil, fmt.Errorf("reserve balance for peer %s: %w", peer.String(), err)
		}
		deferFuncs = append(deferFuncs, func() { ps.accounting.Release(peer, receiptPrice) })

		streamer, err := ps.streamer.NewStream(ctx, peer, nil, protocolName, protocolVersion, streamName)
		if err != nil {
			lastErr = fmt.Errorf("new stream for peer %s: %w", peer.String(), err)
			continue
		}
		deferFuncs = append(deferFuncs, func() { go streamer.FullClose() })

		w, r := protobuf.NewWriterAndReader(streamer)
		ctx, cancel := context.WithTimeout(ctx, timeToWaitForReceipt)
		defer cancel()
		if err := w.WriteMsgWithContext(ctx, &pb.Delivery{
			Address: ch.Address().Bytes(),
			Data:    ch.Data(),
		}); err != nil {
			_ = streamer.Reset()
			lastErr = fmt.Errorf("chunk %s deliver to peer %s: %w", ch.Address().String(), peer.String(), err)
			continue
		}

		ps.metrics.TotalSent.Inc()

		// if you manage to get a tag, just increment the respective counter
		t, err := ps.tagger.Get(ch.TagID())
		if err == nil && t != nil {
			err = t.Inc(tags.StateSent)
			if err != nil {
				lastErr = fmt.Errorf("tag %d increment: %v", ch.TagID(), err)
				err = lastErr
				return nil, err
			}
		}

		var receipt pb.Receipt
		cctx, cancel := context.WithTimeout(ctx, timeToWaitForReceipt)
		defer cancel()
		if err := r.ReadMsgWithContext(cctx, &receipt); err != nil {
			_ = streamer.Reset()
			lastErr = fmt.Errorf("chunk %s receive receipt from peer %s: %w", ch.Address().String(), peer.String(), err)
			continue
		}

		if !ch.Address().Equal(swarm.NewAddress(receipt.Address)) {
			// if the receipt is invalid, try to push to the next peer
			lastErr = fmt.Errorf("invalid receipt. chunk %s, peer %s", ch.Address().String(), peer.String())
			continue
		}

		err = ps.accounting.Credit(peer, receiptPrice)
		if err != nil {
			return nil, err
		}

		return &receipt, nil
	}

	logger.Tracef("pushsync: chunk %s: reached %v peers", ch.Address(), maxPeers)

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, topology.ErrNotFound
}

func (ps *PushSync) closestPeer(addr swarm.Address, skipPeers []swarm.Address, allowUpstream bool) (swarm.Address, error) {
	closest := swarm.Address{}
	err := ps.peerSuggester.EachPeerRev(func(peer swarm.Address, po uint8) (bool, bool, error) {
		for _, a := range skipPeers {
			if a.Equal(peer) {
				return false, false, nil
			}
		}
		if closest.IsZero() {
			closest = peer
			return false, false, nil
		}
		dcmp, err := swarm.DistanceCmp(addr.Bytes(), closest.Bytes(), peer.Bytes())
		if err != nil {
			return false, false, fmt.Errorf("distance compare error. addr %s closest %s peer %s: %w", addr.String(), closest.String(), peer.String(), err)
		}
		switch dcmp {
		case 0:
			// do nothing
		case -1:
			// current peer is closer
			closest = peer
		case 1:
			// closest is already closer to chunk
			// do nothing
		}
		return false, false, nil
	})
	if err != nil {
		return swarm.Address{}, err
	}

	// check if found
	if closest.IsZero() {
		return swarm.Address{}, topology.ErrNotFound
	}
	if allowUpstream {
		return closest, nil
	}

	dcmp, err := swarm.DistanceCmp(addr.Bytes(), closest.Bytes(), ps.addr.Bytes())
	if err != nil {
		return swarm.Address{}, fmt.Errorf("distance compare addr %s closest %s base address %s: %w", addr.String(), closest.String(), ps.addr.String(), err)
	}
	if dcmp != 1 {
		return swarm.Address{}, topology.ErrNotFound
	}

	return closest, nil
}
