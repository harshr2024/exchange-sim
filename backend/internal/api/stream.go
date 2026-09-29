package api

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/feed"
)

// handleStream serves one market's data over a WebSocket.
//
// The interesting part is the order of the first three steps, which closes the gap between
// a snapshot and the live stream.
//
// The naive sequence is to fetch a snapshot and then subscribe. That loses every update
// that lands in between: the client believes it is current as of the snapshot, but the
// book has already moved and nothing will ever tell it so. The gap is small and entirely
// silent, which is the worst combination.
//
// So instead:
//
//  1. Subscribe first. The hub starts queueing immediately, so nothing can be missed from
//     this point on.
//  2. Take a snapshot. It is produced on the market's goroutine, so it reflects exactly
//     the commands up to some sequence S, and every update for those commands is already
//     sitting in the queue from step 1.
//  3. Send the snapshot, then forward queued messages, discarding any with sequence <= S
//     because the snapshot already accounts for them.
//
// Step 3 needs no separate catch-up phase. Messages leave the hub in sequence order, so
// once one passes the sequence > S test every later one does too; the filter is simply
// applied to everything and stops mattering on its own.
//
// If the subscriber is evicted at any point, including while the snapshot is being taken,
// messages were lost and the client's view would have a hole in it. That is reported as a
// resync rather than continued from.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	symbol := r.PathValue("symbol")

	actor, err := s.exchange.Market(symbol)
	if err != nil {
		writeError(w, err)
		return
	}
	hub, err := s.Hub(symbol)
	if err != nil {
		writeError(w, err)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.cfg.AllowedOrigins,
	})
	if err != nil {
		// Accept has already written a response.
		return
	}
	defer conn.CloseNow()

	// Nothing is expected from the client. CloseRead discards incoming frames, answers
	// pings and gives back a context that is canceled when the peer goes away, which is
	// how the writer learns to stop.
	ctx := conn.CloseRead(r.Context())

	// Step 1: subscribe before reading any state.
	sub := hub.Subscribe(s.cfg.SubscriberQueue)
	defer hub.Unsubscribe(sub)

	// Step 2: read the book and this client's own open orders together, in one query, so
	// both describe the same sequence S. Reading them separately would let a fill land
	// between the two, leaving the client with a pair of views that were never
	// simultaneously true and no safe sequence to discard from.
	state, err := actor.SyncState(ctx, s.cfg.Participant)
	if err != nil {
		s.closeStream(ctx, conn, sub, "market is unavailable", false)
		return
	}
	if !sub.Alive() {
		// The queue overflowed while the state was being read, so updates after it are
		// already lost. Continuing would produce a view with a silent gap.
		s.closeStream(ctx, conn, sub, sub.Reason(), true)
		return
	}

	if err := s.write(ctx, conn, feed.SnapshotMessage(state)); err != nil {
		return
	}

	// Step 3: forward everything newer than the synchronised state.
	s.pump(ctx, conn, sub, state.Sequence)
}

// pump forwards messages until the client leaves, the subscription dies, or a write fails.
func (s *Server) pump(ctx context.Context, conn *websocket.Conn, sub *feed.Subscriber, snapshotSeq uint64) {
	for {
		// Death is checked before anything else each time round. If the subscription has
		// been evicted, whatever is still queued is on the far side of a gap, so
		// delivering it would be worse than stopping.
		select {
		case <-sub.Dead():
			s.closeStream(ctx, conn, sub, sub.Reason(), true)
			return
		default:
		}

		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "")
			return

		case <-sub.Dead():
			s.closeStream(ctx, conn, sub, sub.Reason(), true)
			return

		case msg := <-sub.Messages():
			// Already represented by the snapshot the client was given.
			if msg.Sequence <= snapshotSeq {
				continue
			}
			if err := s.write(ctx, conn, msg); err != nil {
				return
			}
		}
	}
}

// write sends one message under a deadline.
//
// The deadline matters even though the hub never blocks: a client whose socket has stalled
// would otherwise pin this goroutine indefinitely. With a deadline the write fails, the
// handler returns, and the subscription is cleaned up.
func (s *Server) write(ctx context.Context, conn *websocket.Conn, msg feed.Message) error {
	writeCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
	defer cancel()
	return wsjson.Write(writeCtx, conn, msg)
}

// closeStream makes a best effort to tell the client why the stream ended before closing
// it. Both steps can fail if the client has already gone, which is fine.
func (s *Server) closeStream(ctx context.Context, conn *websocket.Conn, sub *feed.Subscriber, reason string, resync bool) {
	if reason == "" {
		reason = "market data stream ended"
	}

	// A short deadline: this is a courtesy to a client that may well be the reason the
	// stream is ending, so it must not delay cleanup.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = wsjson.Write(writeCtx, conn, feed.ErrorMessage("", reason, resync))

	status := websocket.StatusNormalClosure
	if resync {
		// Try Again Later: the client is expected to refetch a snapshot and reconnect.
		status = websocket.StatusTryAgainLater
	}
	_ = conn.Close(status, truncateCloseReason(reason))
}

// truncateCloseReason keeps a close reason inside the 123-byte limit the protocol allows.
func truncateCloseReason(reason string) string {
	const limit = 120
	if len(reason) <= limit {
		return reason
	}
	return reason[:limit]
}
