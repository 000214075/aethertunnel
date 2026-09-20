package dht

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// requestTimeout bounds one request/reply exchange. It applies in addition to
// the caller's context, so a single dead peer cannot stall a lookup.
const requestTimeout = 2 * time.Second

// bootstrapTimeout bounds the whole bootstrap phase of a node.
const bootstrapTimeout = 15 * time.Second

var errAlreadyStarted = errors.New("dht: table already started")

// Table is a running DHT node.
type Table struct {
	self    ID
	listen  string
	k       int
	alpha   int
	ttl     time.Duration
	refresh time.Duration
	boot    []string
	log     *log.Logger

	rt    *routingTable
	store *valueStore

	mu      sync.Mutex
	started bool
	closed  bool
	conn    *net.UDPConn
	addr    net.Addr
	ownAddr string
	pending map[[txLen]byte]chan message

	rootCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New creates a table. Call Start to begin serving.
func New(cfg Config) (*Table, error) {
	listen := listenAddr(cfg.ListenAddr)
	if _, err := net.ResolveUDPAddr("udp", listen); err != nil {
		return nil, fmt.Errorf("dht: listen address %q: %w", cfg.ListenAddr, err)
	}
	self := cfg.ID
	if self.isZero() {
		self = RandomID()
	}
	k := cfg.K
	if k <= 0 {
		k = DefaultK
	}
	alpha := cfg.Alpha
	if alpha <= 0 {
		alpha = DefaultAlpha
	}
	if alpha > k {
		alpha = k
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	refresh := cfg.RefreshInterval
	if refresh <= 0 {
		refresh = DefaultRefreshInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Table{
		self:    self,
		listen:  listen,
		k:       k,
		alpha:   alpha,
		ttl:     ttl,
		refresh: refresh,
		boot:    append([]string(nil), cfg.Bootstrap...),
		log:     cfg.Logger,
		rt:      newRoutingTable(self, k),
		store:   newValueStore(),
		pending: make(map[[txLen]byte]chan message),
		rootCtx: ctx,
		cancel:  cancel,
	}, nil
}

// Start binds the socket and begins serving. It returns the bound address.
// Bootstrap contacts are only used by the background join that Start launches,
// so Start itself never waits for the network.
func (t *Table) Start() (net.Addr, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, ErrClosed
	}
	if t.started {
		t.mu.Unlock()
		return nil, errAlreadyStarted
	}
	uaddr, err := net.ResolveUDPAddr("udp", t.listen)
	if err == nil {
		var conn *net.UDPConn
		if conn, err = net.ListenUDP("udp", uaddr); err == nil {
			t.conn = conn
			t.addr = conn.LocalAddr()
			t.ownAddr = advertiseAddr(t.listen, conn.LocalAddr())
			t.started = true
		}
	}
	addr := t.addr
	t.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("dht: start on %q: %w", t.listen, err)
	}
	t.logf("dht: listening on %s id=%s", addr, t.self)
	t.launch(t.readLoop)
	t.launch(t.refreshLoop)
	if len(t.boot) > 0 {
		t.launch(t.bootstrap)
	}
	return addr, nil
}

// Close stops the node and waits for its goroutines. It is idempotent.
func (t *Table) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	conn := t.conn
	t.mu.Unlock()
	t.cancel()
	if conn != nil {
		// Closing the socket is what unblocks the read loop.
		_ = conn.Close()
	}
	t.wg.Wait()
	return nil
}

// Self returns this node's ID.
func (t *Table) Self() ID { return t.self }

// Addr returns the bound address, or nil before Start.
func (t *Table) Addr() net.Addr {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.addr
}

// Len returns the number of contacts in the routing table.
func (t *Table) Len() int { return t.rt.len() }

// Closest returns up to n known contacts closest to target, nearest first.
func (t *Table) Closest(target ID, n int) []Node { return t.rt.closest(target, n) }

// Ping contacts addr and adds it to the routing table when it answers.
func (t *Table) Ping(ctx context.Context, addr string) error { return t.ping(ctx, addr) }

// Put stores a value under key on the K nodes closest to key that this node
// knows.
//
// Replication is best effort: peers that cannot be reached are logged and
// skipped, because the local copy already answers Get on this node.
func (t *Table) Put(ctx context.Context, key string, value []byte) error {
	if len(value) > MaxValueSize {
		return ErrValueTooLarge
	}
	if err := t.ready(); err != nil {
		return err
	}
	id := keyFor(nsValue, key)
	t.store.put(storeKey(nsValue, id), value, time.Now().Add(t.ttl), true)
	t.replicate(ctx, nsValue, id, value)
	return nil
}

// Get looks a value up iteratively, preferring locally stored values and
// falling back to a network lookup.
func (t *Table) Get(ctx context.Context, key string) ([]byte, error) {
	if err := t.ready(); err != nil {
		return nil, err
	}
	id := keyFor(nsValue, key)
	if v, ok := t.store.get(storeKey(nsValue, id)); ok {
		return v, nil
	}
	return t.lookupValue(ctx, id, nsValue)
}

// Provide announces that this node serves key, storing this node's address as
// the value, so other nodes can find it.
func (t *Table) Provide(ctx context.Context, key string) error {
	if err := t.ready(); err != nil {
		return err
	}
	own := t.own()
	if own == "" {
		return ErrNotStarted
	}
	id := keyFor(nsProvider, key)
	t.store.add(storeKey(nsProvider, id), own, time.Now().Add(t.ttl))
	t.replicate(ctx, nsProvider, id, []byte(own))
	return nil
}

// FindProviders returns the addresses announced for key, or ErrNotFound when no
// node knows one.
func (t *Table) FindProviders(ctx context.Context, key string) ([]string, error) {
	if err := t.ready(); err != nil {
		return nil, err
	}
	id := keyFor(nsProvider, key)
	if v, ok := t.store.get(storeKey(nsProvider, id)); ok {
		return splitAddresses(string(v)), nil
	}
	v, err := t.lookupValue(ctx, id, nsProvider)
	if err != nil {
		return nil, err
	}
	return splitAddresses(string(v)), nil
}

// replicate stores a record on the k closest known contacts.
func (t *Table) replicate(ctx context.Context, ns byte, id ID, value []byte) {
	contacts, err := t.lookupNodes(ctx, id)
	if err != nil {
		t.logf("dht: replicate %s: %v", id, err)
		return
	}
	m := message{
		typ:    msgStore,
		id:     t.self,
		ns:     ns,
		key:    id,
		expiry: time.Now().Add(t.ttl).Unix(),
		value:  value,
	}
	for _, c := range contacts {
		t.sendTo(c.addr, m)
	}
}

func (t *Table) readLoop() {
	buf := make([]byte, maxDatagram)
	readErrs := 0
	for {
		n, from, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if t.isClosed() {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			readErrs++
			if readErrs > 1000 {
				t.logf("dht: giving up on the socket after %d read errors: %v", readErrs, err)
				return
			}
			continue
		}
		readErrs = 0
		m, err := decode(buf[:n])
		if err != nil {
			// Malformed input is dropped without a reply: answering would turn
			// a spoofed source address into a packet reflector.
			continue
		}
		t.handle(m, from)
	}
}

// handle dispatches one decoded datagram. It runs on the read loop and performs
// no blocking network work, so replies are always sent promptly.
func (t *Table) handle(m message, from *net.UDPAddr) {
	switch m.typ {
	case msgPong, msgNodes, msgValue:
		t.deliver(m)
		return
	}
	addr := from.String()
	t.noteSeen(m.id, addr)
	switch m.typ {
	case msgPing:
		t.sendTo(addr, message{typ: msgPong, tx: m.tx, id: t.self})
	case msgFindNode:
		t.sendTo(addr, t.nodesMessage(m.target, m.tx))
	case msgFindValue:
		resp := message{typ: msgValue, tx: m.tx, id: t.self, ns: m.ns, key: m.key}
		if v, ok := t.store.get(storeKey(m.ns, m.key)); ok {
			resp.found = true
			resp.value = v
		} else {
			// A miss answers with the closest contacts, as Kademlia expects.
			resp = t.nodesMessage(m.key, m.tx)
		}
		t.sendTo(addr, resp)
	case msgStore:
		t.storeRemote(m)
	}
}

func (t *Table) nodesMessage(target ID, tx [txLen]byte) message {
	return message{typ: msgNodes, tx: tx, id: t.self, target: target, nodes: t.rt.closest(target, t.k)}
}

// storeRemote keeps a record a peer asked this node to hold. The expiry is
// capped at this node's own TTL: a peer does not get to decide how long its
// data occupies local memory.
func (t *Table) storeRemote(m message) {
	expires := time.Unix(m.expiry, 0)
	if limit := time.Now().Add(t.ttl); expires.After(limit) {
		expires = limit
	}
	switch m.ns {
	case nsValue:
		t.store.put(storeKey(m.ns, m.key), m.value, expires, false)
	case nsProvider:
		// A provider record is an address; anything else would only pollute
		// FindProviders results for other nodes.
		if addr := string(m.value); validContact(addr) {
			t.store.add(storeKey(m.ns, m.key), addr, expires)
		}
	}
}

// noteSeen records a contact that just proved it is alive and probes the least
// recently seen occupant of a full bucket.
func (t *Table) noteSeen(id ID, addr string) {
	if id.isZero() || id == t.self || !validContact(addr) {
		return
	}
	if cand := t.rt.seen(id, addr); cand != nil {
		t.checkContact(cand)
	}
}

// checkContact pings a contact that was pushed out of a full bucket: a node
// that still answers keeps its slot, a silent one is replaced from the cache.
func (t *Table) checkContact(c *contact) {
	t.launch(func() {
		ctx, cancel := context.WithTimeout(t.rootCtx, requestTimeout)
		defer cancel()
		if err := t.ping(ctx, c.addr); err != nil {
			t.rt.remove(c.id)
			t.logf("dht: evicted %s (%s): %v", c.id, c.addr, err)
		}
	})
}

func (t *Table) ping(ctx context.Context, addr string) error {
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("dht: contact %q: %w", addr, err)
	}
	resp, err := t.request(ctx, uaddr, message{typ: msgPing, id: t.self})
	if err != nil {
		return err
	}
	if resp.typ != msgPong {
		return fmt.Errorf("dht: %s answered a ping with %s", addr, resp.typ)
	}
	t.noteSeen(resp.id, uaddr.String())
	return nil
}

// requestString resolves addr and performs one request/reply exchange.
func (t *Table) requestString(ctx context.Context, addr string, m message) (message, error) {
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return message{}, fmt.Errorf("dht: contact %q: %w", addr, err)
	}
	return t.request(ctx, uaddr, m)
}

// request sends m and waits for the reply carrying the same transaction id.
func (t *Table) request(ctx context.Context, addr *net.UDPAddr, m message) (message, error) {
	if err := t.ready(); err != nil {
		return message{}, err
	}
	base, cancelBase := context.WithCancel(ctx)
	stop := context.AfterFunc(t.rootCtx, cancelBase)
	defer func() {
		stop()
		cancelBase()
	}()
	reqCtx, cancelReq := context.WithTimeout(base, requestTimeout)
	defer cancelReq()

	tx := randomTxID()
	ch := make(chan message, 1)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return message{}, ErrClosed
	}
	if !t.started {
		t.mu.Unlock()
		return message{}, ErrNotStarted
	}
	t.pending[tx] = ch
	conn := t.conn
	t.mu.Unlock()
	defer t.forget(tx)

	m.tx = tx
	pkt, err := encode(m)
	if err != nil {
		return message{}, err
	}
	if _, err := conn.WriteToUDP(pkt, addr); err != nil {
		if t.isClosed() {
			return message{}, ErrClosed
		}
		return message{}, fmt.Errorf("dht: send to %s: %w", addr, err)
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-reqCtx.Done():
		if err := ctx.Err(); err != nil {
			return message{}, err
		}
		if t.isClosed() {
			return message{}, ErrClosed
		}
		return message{}, ErrTimeout
	}
}

// sendTo writes a datagram without waiting for an answer.
func (t *Table) sendTo(addr string, m message) {
	if err := t.ready(); err != nil {
		return
	}
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.logf("dht: contact %q: %v", addr, err)
		return
	}
	pkt, err := encode(m)
	if err != nil {
		t.logf("dht: encode %s: %v", m.typ, err)
		return
	}
	if _, err := t.conn.WriteToUDP(pkt, uaddr); err != nil && !t.isClosed() {
		t.logf("dht: send %s to %s: %v", m.typ, addr, err)
	}
}

func (t *Table) deliver(m message) {
	t.mu.Lock()
	ch, ok := t.pending[m.tx]
	t.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- m:
	default:
		// The requester already gave up.
	}
}

func (t *Table) forget(tx [txLen]byte) {
	t.mu.Lock()
	delete(t.pending, tx)
	t.mu.Unlock()
}

// bootstrap joins the network: every configured contact is pinged, and the
// first one that answers is used to fill the routing table around the local ID.
func (t *Table) bootstrap() {
	ctx, cancel := context.WithTimeout(t.rootCtx, bootstrapTimeout)
	defer cancel()
	for _, addr := range t.boot {
		if err := t.ping(ctx, addr); err != nil {
			t.logf("dht: bootstrap %s: %v", addr, err)
			continue
		}
		if _, _, _, err := t.iterate(ctx, t.self, nsValue, false); err != nil {
			t.logf("dht: bootstrap lookup: %v", err)
		}
	}
}

func (t *Table) refreshLoop() {
	ticker := time.NewTicker(t.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-t.rootCtx.Done():
			return
		case <-ticker.C:
			t.refreshOnce()
			t.republish()
		}
	}
}

// refreshOnce looks up a random ID in every bucket that has been idle for a
// whole interval, which repairs a routing table that drifted as peers left.
func (t *Table) refreshOnce() {
	for _, idx := range t.rt.takeStaleBuckets(t.refresh) {
		ctx, cancel := context.WithTimeout(t.rootCtx, 3*requestTimeout)
		if _, _, _, err := t.iterate(ctx, t.rt.randomIDInBucket(idx), nsValue, false); err != nil {
			t.logf("dht: refresh bucket %d: %v", idx, err)
		}
		cancel()
	}
}

// republish re-stores the records this node owns before they expire elsewhere.
func (t *Table) republish() {
	for _, rec := range t.store.localRecords() {
		ctx, cancel := context.WithTimeout(t.rootCtx, 3*requestTimeout)
		contacts, err := t.lookupNodes(ctx, rec.id)
		cancel()
		if err != nil {
			continue
		}
		m := message{
			typ:    msgStore,
			id:     t.self,
			ns:     rec.ns,
			key:    rec.id,
			expiry: time.Now().Add(t.ttl).Unix(),
			value:  rec.value,
		}
		for _, c := range contacts {
			t.sendTo(c.addr, m)
		}
	}
}

// launch runs fn in a goroutine that Close waits for. Once the table is closed
// no further work starts, which keeps the wait group balanced.
func (t *Table) launch(fn func()) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.wg.Add(1)
	t.mu.Unlock()
	go func() {
		defer t.wg.Done()
		fn()
	}()
}

func (t *Table) ready() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return ErrClosed
	}
	if !t.started {
		return ErrNotStarted
	}
	return nil
}

func (t *Table) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

func (t *Table) own() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ownAddr
}

func (t *Table) logf(format string, args ...any) {
	if t.log != nil {
		t.log.Printf(format, args...)
	}
}

func randomTxID() [txLen]byte {
	var tx [txLen]byte
	if _, err := rand.Read(tx[:]); err != nil {
		panic("dht: random transaction id: " + err.Error())
	}
	return tx
}

// listenAddr maps the empty listen address to the ephemeral form.
func listenAddr(addr string) string {
	if addr == "" {
		return ":0"
	}
	return addr
}

// advertiseAddr is the address announced in provider records. An explicit host
// in the listen address wins; an unspecified host is replaced by the address of
// the interface the host routes through, because "0.0.0.0" is useless to a peer.
func advertiseAddr(listen string, bound net.Addr) string {
	_, port, err := net.SplitHostPort(bound.String())
	if err != nil {
		port = "0"
	}
	host, _, err := net.SplitHostPort(listen)
	if err == nil && host != "" && !isUnspecifiedHost(host) {
		return net.JoinHostPort(host, port)
	}
	if ip := outboundIP(); ip != "" {
		return net.JoinHostPort(ip, port)
	}
	return net.JoinHostPort("127.0.0.1", port)
}

func isUnspecifiedHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// outboundIP reports the address of the interface used for off-host traffic.
// Dialing a UDP socket performs no handshake and sends no packet.
func outboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return ""
	}
	return host
}
