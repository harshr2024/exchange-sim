package engine

// node is an element of a price level's FIFO queue. Holding the node in the
// order index is what makes cancellation O(1) instead of a book scan.
type node struct {
	order      *Order
	prev, next *node
}

// PriceLevel holds every resting order at one price, oldest first.
//
// TotalQty is maintained as an exact running sum of the RemainingQty of the
// orders in the queue, so depth snapshots never have to walk the queue.
type PriceLevel struct {
	Price      PriceTicks
	TotalQty   int64
	count      int
	head, tail *node
}

func newPriceLevel(price PriceTicks) *PriceLevel {
	return &PriceLevel{Price: price}
}

// Len returns the number of resting orders at this price.
func (l *PriceLevel) Len() int { return l.count }

// Empty reports whether the level holds no resting orders. Empty levels are
// retained briefly so that the price heap and the level map stay in step; see
// bookSide.bestLevel.
func (l *PriceLevel) Empty() bool { return l.count == 0 }

// Front returns the oldest resting order, or nil when the level is empty.
func (l *PriceLevel) Front() *Order {
	if l.head == nil {
		return nil
	}
	return l.head.order
}

// Orders returns the resting orders in FIFO order. It allocates, so it is for
// snapshots and assertions rather than the matching path.
func (l *PriceLevel) Orders() []*Order {
	out := make([]*Order, 0, l.count)
	for n := l.head; n != nil; n = n.next {
		out = append(out, n.order)
	}
	return out
}

func (l *PriceLevel) pushBack(o *Order) *node {
	n := &node{order: o, prev: l.tail}
	if l.tail == nil {
		l.head = n
	} else {
		l.tail.next = n
	}
	l.tail = n
	l.count++
	l.TotalQty += o.RemainingQty
	return n
}

func (l *PriceLevel) remove(n *node) {
	if n.prev == nil {
		l.head = n.next
	} else {
		n.prev.next = n.next
	}
	if n.next == nil {
		l.tail = n.prev
	} else {
		n.next.prev = n.prev
	}
	n.prev, n.next = nil, nil
	l.count--
	l.TotalQty -= n.order.RemainingQty
}

// reduce records that a resting order in this level shed qty to a fill.
func (l *PriceLevel) reduce(qty int64) { l.TotalQty -= qty }
