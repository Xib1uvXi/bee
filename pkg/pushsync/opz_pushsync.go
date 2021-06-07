package pushsync

import (
	"context"
	"fmt"
	"github.com/ethersphere/bee/pkg/cac"
	"github.com/ethersphere/bee/pkg/p2p"
	"github.com/ethersphere/bee/pkg/p2p/protobuf"
	"github.com/ethersphere/bee/pkg/pushsync/pb"
	"github.com/ethersphere/bee/pkg/soc"
	"github.com/ethersphere/bee/pkg/storage"
	"github.com/ethersphere/bee/pkg/swarm"
)

// localHandler There is no need to push the chunk to other nodes,
// we store it directly after receiving the chunk, and then return to Receipt
func (ps *PushSync) localHandler(ctx context.Context, p p2p.Peer, stream p2p.Stream) (err error) {
	w, r := protobuf.NewWriterAndReader(stream)
	ctx, cancel := context.WithTimeout(ctx, defaultTTL)
	defer cancel()
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
	if chunk, err = ps.validStamp(chunk, ch.Stamp); err != nil {
		return fmt.Errorf("pushsync valid stamp: %w", err)
	}

	if cac.Valid(chunk) {
		if ps.unwrap != nil {
			go ps.unwrap(chunk)
		}
	} else if !soc.Valid(chunk) {
		return swarm.ErrInvalidChunk
	}

	price := ps.pricer.Price(chunk.Address())

	bytes := chunk.Address().Bytes()

	ctxd, canceld := context.WithTimeout(context.Background(), timeToWaitForPushsyncToNeighbor)
	defer canceld()

	_, err = ps.storer.Put(ctxd, storage.ModePutSync, chunk)
	if err != nil {
		return fmt.Errorf("chunk store: %w", err)
	}

	// log
	ps.tclogger.Info("block forwarding, save chunk to local storage")

	debit := ps.accounting.PrepareDebit(p.Address, price)
	defer debit.Cleanup()

	// return back receipt
	signature, err := ps.signer.Sign(bytes)
	if err != nil {
		return fmt.Errorf("receipt signature: %w", err)
	}
	receipt := pb.Receipt{Address: bytes, Signature: signature}
	if err := w.WriteMsgWithContext(ctxd, &receipt); err != nil {
		return fmt.Errorf("send receipt to peer %s: %w", p.Address.String(), err)
	}

	if err := debit.Apply(); err != nil {
		return err
	}

	ps.tclogger.Infof("block forwarding, save chunk to local storage, peer address: %s, price: %v", p.Address.String(), price)

	return nil
}
