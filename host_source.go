package gocql

import (
	"fmt"
	"log"
	"net"
	"sync"
)

type nodeState int32

func (n nodeState) String() string {
	if n == NodeUp {
		return "UP"
	} else if n == NodeDown {
		return "DOWN"
	}
	return fmt.Sprintf("UNKNOWN_%d", n)
}

const (
	NodeUp nodeState = iota
	NodeDown
)

type cassVersion struct {
	Major, Minor, Patch int
}

func (c cassVersion) String() string {
	return fmt.Sprintf("v%d.%d.%d", c.Major, c.Minor, c.Patch)
}

type HostInfo struct {
	// TODO(zariel): reduce locking maybe, not all values will change, but to ensure
	// that we are thread safe use a mutex to access all fields.
	mu         sync.RWMutex
	peer       string
	dataCenter string
	rack       string
	hostId     string
	version    cassVersion
	state      nodeState
	tokens     []string
}

func (h *HostInfo) Equal(host *HostInfo) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	host.mu.RLock()
	defer host.mu.RUnlock()

	return h.peer == host.peer && h.hostId == host.hostId
}

func (h *HostInfo) Peer() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.peer
}

func (h *HostInfo) setPeer(peer string) *HostInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.peer = peer
	return h
}

func (h *HostInfo) DataCenter() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.dataCenter
}

func (h *HostInfo) setDataCenter(dataCenter string) *HostInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dataCenter = dataCenter
	return h
}

func (h *HostInfo) Rack() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rack
}

func (h *HostInfo) setRack(rack string) *HostInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rack = rack
	return h
}

func (h *HostInfo) HostID() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.hostId
}

func (h *HostInfo) setHostID(hostID string) *HostInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hostId = hostID
	return h
}

func (h *HostInfo) Version() cassVersion {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.version
}

func (h *HostInfo) setVersion(major, minor, patch int) *HostInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.version.Major = major
	h.version.Minor = minor
	h.version.Patch = patch
	return h
}

func (h *HostInfo) State() nodeState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.state
}

func (h *HostInfo) setState(state nodeState) *HostInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = state
	return h
}

func (h *HostInfo) Tokens() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.tokens
}

func (h *HostInfo) setTokens(tokens []string) *HostInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tokens = tokens
	return h
}

func (h *HostInfo) IsUp() bool {
	return h.State() == NodeUp
}

func (h HostInfo) String() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return fmt.Sprintf("[hostinfo peer=%q data_centre=%q rack=%q host_id=%q version=%q state=%s num_tokens=%d]", h.peer, h.dataCenter, h.rack, h.hostId, h.version, h.state, len(h.tokens))
}

// Polls system.peers at a specific interval to find new hosts
type ringDescriber struct {
	dcFilter   string
	rackFilter string
	session    *Session
	closeChan  chan bool
	// indicates that we can use system.local to get the connections remote address
	localHasRpcAddr bool

	mu              sync.Mutex
	prevHosts       []*HostInfo
	prevPartitioner string
}

func checkSystemLocal(control *controlConn) (bool, error) {
	iter := control.query("SELECT broadcast_address FROM system.local")
	if err := iter.err; err != nil {
		if errf, ok := err.(*errorFrame); ok {
			if errf.code == errSyntax {
				return false, nil
			}
		}

		return false, err
	}

	return true, nil
}

func (r *ringDescriber) GetHosts() (hosts []*HostInfo, partitioner string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// we need conn to be the same because we need to query system.peers and system.local
	// on the same node to get the whole cluster

	const (
		legacyLocalQuery = "SELECT data_center, rack, host_id, tokens, partitioner FROM system.local"
		// only supported in 2.2.0, 2.1.6, 2.0.16
		localQuery = "SELECT broadcast_address, data_center, rack, host_id, tokens, partitioner FROM system.local"
	)

	localHost := &HostInfo{}
	if r.localHasRpcAddr {
		iter := r.session.control.query(localQuery)
		if iter == nil {
			return r.prevHosts, r.prevPartitioner, nil
		}

		iter.Scan(&localHost.peer, &localHost.dataCenter, &localHost.rack,
			&localHost.hostId, &localHost.tokens, &partitioner)

		if err = iter.Close(); err != nil {
			return nil, "", err
		}
	} else {
		iter := r.session.control.query(legacyLocalQuery)
		if iter == nil {
			return r.prevHosts, r.prevPartitioner, nil
		}

		iter.Scan(&localHost.dataCenter, &localHost.rack, &localHost.hostId, &localHost.tokens, &partitioner)

		if err = iter.Close(); err != nil {
			return nil, "", err
		}

		addr, _, err := net.SplitHostPort(r.session.control.addr())
		if err != nil {
			// this should not happen, ever, as this is the address that was dialed by conn, here
			// a panic makes sense, please report a bug if it occurs.
			panic(err)
		}

		localHost.peer = addr
	}

	hosts = []*HostInfo{localHost}

	iter := r.session.control.query("SELECT rpc_address, data_center, rack, host_id, tokens FROM system.peers")
	if iter == nil {
		return r.prevHosts, r.prevPartitioner, nil
	}

	host := &HostInfo{}
	for iter.Scan(&host.peer, &host.dataCenter, &host.rack, &host.hostId, &host.tokens) {
		if r.matchFilter(host) {
			hosts = append(hosts, host)
		}
		host = &HostInfo{}
	}

	if err = iter.Close(); err != nil {
		return nil, "", err
	}

	r.prevHosts = hosts
	r.prevPartitioner = partitioner

	return hosts, partitioner, nil
}

func (r *ringDescriber) matchFilter(host *HostInfo) bool {

	if r.dcFilter != "" && r.dcFilter != host.DataCenter() {
		return false
	}

	if r.rackFilter != "" && r.rackFilter != host.Rack() {
		return false
	}

	return true
}

func (r *ringDescriber) refreshRing() {
	// if we have 0 hosts this will return the previous list of hosts to
	// attempt to reconnect to the cluster otherwise we would never find
	// downed hosts again, could possibly have an optimisation to only
	// try to add new hosts if GetHosts didnt error and the hosts didnt change.
	hosts, partitioner, err := r.GetHosts()
	if err != nil {
		log.Println("RingDescriber: unable to get ring topology:", err)
		return
	}

	// TODO: move this to session
	for _, h := range hosts {
		if r.session.ring.addHostIfMissing(h) {
			r.session.pool.addHost(h)
			// TODO: trigger OnUp/OnAdd
		}
	}

	r.session.pool.SetPartitioner(partitioner)
}
