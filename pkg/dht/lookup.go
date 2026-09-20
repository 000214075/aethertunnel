package dht

import (
	"context"
	"errors"
	"sort"
)

// lookupQueryLimit bounds how many contacts one iterative lookup may question,
// so a peer that keeps answering with unknown contacts cannot spin a search.
const lookupQueryLimit = 8 * DefaultK

// shortlist holds the k closest candidates seen so far, nearest first.
type shortlist struct {
	target ID
	k      int
	items  []contact
}

func newShortlist(target ID, k int) *shortlist {
	return &shortlist{target: target, k: k}
}

func (s *shortlist) add(c contact) {
	if c.id.isZero() || !validContact(c.addr) {
		return
	}
	for _, e := range s.items {
		if e.id == c.id {
			return
		}
	}
	s.items = append(s.items, c)
	sort.Slice(s.items, func(i, j int) bool {
		return lessDistance(s.items[i].id, s.items[j].id, s.target)
	})
	if len(s.items) > s.k {
		s.items = s.items[:s.k]
	}
}

// nextUnqueried returns the closest candidate that has not been asked yet.
func (s *shortlist) nextUnqueried(queried map[ID]bool) (contact, bool) {
	for _, c := range s.items {
		if !queried[c.id] {
			return c, true
		}
	}
	return contact{}, false
}

func (s *shortlist) contacts() []contact {
	return append([]contact(nil), s.items...)
}

type lookupResult struct {
	c   contact
	msg message
	err error
}

// iterate walks the routing table towards target with alpha-way concurrency.
// It questions the closest unqueried candidates and stops once every one of the
// k closest contacts found has answered, which is the point at which no closer
// node can appear. wantValue additionally asks peers for the value stored under
// target in namespace ns and returns as soon as one supplies it.
func (t *Table) iterate(ctx context.Context, target ID, ns byte, wantValue bool) ([]contact, []byte, bool, error) {
	short := newShortlist(target, t.k)
	for _, n := range t.rt.closest(target, t.k) {
		short.add(contact{id: n.ID, addr: n.Addr, lastSeen: n.LastSeen})
	}
	queried := make(map[ID]bool)
	results := make(chan lookupResult, t.alpha)
	inFlight, issued := 0, 0

	start := func(c contact) {
		queried[c.id] = true
		inFlight++
		issued++
		go func() {
			m := message{typ: msgFindNode, id: t.self, target: target}
			if wantValue {
				m = message{typ: msgFindValue, id: t.self, ns: ns, key: target}
			}
			resp, err := t.requestString(ctx, c.addr, m)
			results <- lookupResult{c: c, msg: resp, err: err}
		}()
	}

	for {
		for inFlight < t.alpha && issued < lookupQueryLimit {
			next, ok := short.nextUnqueried(queried)
			if !ok {
				break
			}
			start(next)
		}
		if inFlight == 0 {
			break
		}
		select {
		case r := <-results:
			inFlight--
			if r.err != nil {
				if errors.Is(r.err, ErrTimeout) {
					// A peer that does not answer loses its slot; retrying it
					// would only delay the lookup.
					t.rt.remove(r.c.id)
				}
				continue
			}
			t.noteSeen(r.msg.id, r.c.addr)
			if wantValue && r.msg.typ == msgValue && r.msg.found {
				return short.contacts(), r.msg.value, true, nil
			}
			for _, n := range r.msg.nodes {
				short.add(contact{id: n.ID, addr: n.Addr})
			}
		case <-ctx.Done():
			return short.contacts(), nil, false, ctx.Err()
		}
	}
	return short.contacts(), nil, false, nil
}

// lookupNodes returns the k closest known contacts to target.
func (t *Table) lookupNodes(ctx context.Context, target ID) ([]contact, error) {
	contacts, _, _, err := t.iterate(ctx, target, nsValue, false)
	return contacts, err
}

// lookupValue returns the value stored under target, or ErrNotFound.
func (t *Table) lookupValue(ctx context.Context, target ID, ns byte) ([]byte, error) {
	_, value, found, err := t.iterate(ctx, target, ns, true)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return value, nil
}
