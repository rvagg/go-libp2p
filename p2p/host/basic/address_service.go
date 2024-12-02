package basichost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/record"
	"github.com/libp2p/go-libp2p/core/transport"
	"github.com/libp2p/go-libp2p/p2p/host/autonat"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	"github.com/libp2p/go-libp2p/p2p/host/basic/internal/backoff"
	"github.com/libp2p/go-libp2p/p2p/host/eventbus"
	libp2pwebrtc "github.com/libp2p/go-libp2p/p2p/transport/webrtc"
	libp2pwebtransport "github.com/libp2p/go-libp2p/p2p/transport/webtransport"
	"github.com/libp2p/go-netroute"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

type peerRecordFunc func([]ma.Multiaddr) (*record.Envelope, error)

type observedAddrsService interface {
	OwnObservedAddrs() []ma.Multiaddr
	ObservedAddrsFor(local ma.Multiaddr) []ma.Multiaddr
}

type addressService struct {
	net          network.Network
	peerstore    peerstore.Peerstore
	id           peer.ID
	addrsFactory AddrsFactory
	// peerRecord is used to create peer records when the addresses change
	peerRecord           peerRecordFunc
	autonat              autonat.AutoNAT
	autorelay            *autorelay.AutoRelay
	natmgr               NATManager
	observedAddrsService observedAddrsService
	addrChangeChan       chan struct{}
	relayAddrsSub        event.Subscription
	emitter              event.Emitter
	ifaceAddrs           *interfaceAddrsCache
	wg                   sync.WaitGroup
	ctx                  context.Context
	ctxCancel            context.CancelFunc
}

func NewAddressService(h *BasicHost, natmgr func(network.Network) NATManager,
	addrFactory AddrsFactory) (*addressService, error) {
	emitter, err := h.eventbus.Emitter(&event.EvtLocalAddressesUpdated{}, eventbus.Stateful)
	if err != nil {
		return nil, err
	}
	var nmgr NATManager
	if natmgr != nil {
		nmgr = natmgr(h.Network())
	}
	addrSub, err := h.EventBus().Subscribe(new(event.EvtAutoRelayAddrs))
	if err != nil {
		return nil, err
	}
	peerRecord := h.makeSignedPeerRecord
	if !h.disableSignedPeerRecord {
		peerRecord = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	as := &addressService{
		net:                  h.Network(),
		peerstore:            h.Peerstore(),
		observedAddrsService: h.IDService(),
		id:                   h.ID(),
		peerRecord:           peerRecord,
		natmgr:               nmgr,
		emitter:              emitter,
		autorelay:            h.autorelay,
		addrsFactory:         addrFactory,
		addrChangeChan:       make(chan struct{}, 1),
		relayAddrsSub:        addrSub,
		ctx:                  ctx,
		ctxCancel:            cancel,
		ifaceAddrs:           &interfaceAddrsCache{},
	}
	if as.peerRecord != nil {
		cab, ok := peerstore.GetCertifiedAddrBook(as.peerstore)
		if !ok {
			return nil, errors.New("peerstore should also be a certified address book")
		}
		rec, err := as.peerRecord(as.Addrs())
		if err != nil {
			return nil, fmt.Errorf("failed to create signed record for self: %w", err)
		}
		if _, err := cab.ConsumePeerRecord(rec, peerstore.PermanentAddrTTL); err != nil {
			return nil, fmt.Errorf("failed to persist signed record to peerstore: %w", err)
		}
	}
	return as, nil
}

func (a *addressService) SetAutoNAT(an autonat.AutoNAT) {
	a.autonat = an
}

func (a *addressService) Start() {
	a.wg.Add(1)
	go a.background()
}

func (a *addressService) Close() {
	a.ctxCancel()
	a.wg.Wait()
	if a.natmgr != nil {
		err := a.natmgr.Close()
		if err != nil {
			log.Warnf("error closing natmgr: %s", err)
		}
	}
	err := a.emitter.Close()
	if err != nil {
		log.Warnf("error closing addrs update emitter: %s", err)
	}
	err = a.relayAddrsSub.Close()
	if err != nil {
		log.Warnf("error closing addrs update emitter: %s", err)
	}
}

func (a *addressService) SignalAddressChange() {
	select {
	case a.addrChangeChan <- struct{}{}:
	default:
	}
}

func (a *addressService) background() {
	defer a.wg.Done()
	var lastAddrs []ma.Multiaddr

	emitAddrChange := func(currentAddrs []ma.Multiaddr, lastAddrs []ma.Multiaddr) {
		changeEvt := a.makeUpdatedAddrEvent(lastAddrs, currentAddrs)
		if changeEvt == nil {
			return
		}
		// Our addresses have changed.
		// store the signed peer record in the peer store.
		if a.peerRecord != nil {
			cabook, ok := peerstore.GetCertifiedAddrBook(a.peerstore)
			if !ok {
				log.Errorf("peerstore doesn't implement certified address book")
				return
			}
			if _, err := cabook.ConsumePeerRecord(changeEvt.SignedPeerRecord, peerstore.PermanentAddrTTL); err != nil {
				log.Errorf("failed to persist signed peer record in peer store, err=%s", err)
				return
			}
		}
		// update host addresses in the peer store
		removedAddrs := make([]ma.Multiaddr, 0, len(changeEvt.Removed))
		for _, ua := range changeEvt.Removed {
			removedAddrs = append(removedAddrs, ua.Address)
		}
		a.peerstore.SetAddrs(a.id, currentAddrs, peerstore.PermanentAddrTTL)
		a.peerstore.SetAddrs(a.id, removedAddrs, 0)

		// emit addr change event
		if err := a.emitter.Emit(*changeEvt); err != nil {
			log.Warnf("error emitting event for updated addrs: %s", err)
		}
	}

	// periodically schedules an IdentifyPush to update our peers for changes
	// in our address set (if needed)
	ticker := time.NewTicker(addrChangeTickrInterval)
	defer ticker.Stop()

	for {
		curr := a.Addrs()
		emitAddrChange(curr, lastAddrs)
		lastAddrs = curr

		select {
		case <-ticker.C:
		case <-a.addrChangeChan:
		case <-a.relayAddrsSub.Out():
		case <-a.ctx.Done():
			return
		}
	}
}

// Addrs returns the node's dialable addresses both public and private.
// If autorealy is enabled and node reachability is private, it returns
// the node's relay addresses and private network addresses.
func (a *addressService) Addrs() []ma.Multiaddr {
	addrs := a.AllAddrs()
	// Delete public addresses if the node's reachability is private, and we have autorelay.
	if a.autonat != nil && a.autorelay != nil && a.autonat.Status() == network.ReachabilityPrivate {
		addrs = slices.DeleteFunc(addrs, func(a ma.Multiaddr) bool { return manet.IsPublicAddr(a) })
		addrs = append(addrs, a.autorelay.RelayAddrs()...)
	}
	// Make a copy. Consumers can modify the slice elements
	addrs = slices.Clone(a.addrsFactory(addrs))
	// Add certhashes for the addresses provided by the user via address factory.
	return a.addCertHashes(ma.Unique(addrs))
}

// GetHolePunchAddrs returns the node's public direct listen addresses.
func (a *addressService) GetHolePunchAddrs() []ma.Multiaddr {
	addrs := a.AllAddrs()
	addrs = slices.Clone(a.addrsFactory(addrs))
	// AllAddrs may ignore observed addresses in favour of NAT mappings.
	// Use both for hole punching.
	addrs = append(addrs, a.observedAddrsService.OwnObservedAddrs()...)
	addrs = ma.Unique(addrs)
	return slices.DeleteFunc(addrs, func(a ma.Multiaddr) bool { return !manet.IsPublicAddr(a) })
}

var p2pCircuitAddr = ma.StringCast("/p2p-circuit")

// AllAddrs returns all the addresses the host is listening on except circuit addresses.
func (a *addressService) AllAddrs() []ma.Multiaddr {
	listenAddrs := a.net.ListenAddresses()
	if len(listenAddrs) == 0 {
		return nil
	}

	finalAddrs := make([]ma.Multiaddr, 0, 8)
	finalAddrs = a.appendInterfaceAddrs(finalAddrs, listenAddrs)

	// use nat mappings if we have them
	finalAddrs = a.appendNATAddrs(finalAddrs, listenAddrs)
	finalAddrs = ma.Unique(finalAddrs)

	// Remove /p2p-circuit addresses from the list.
	// The p2p-circuit transport listener reports its address as just /p2p-circuit
	// This is useless for dialing. Users need to manage their circuit addresses themselves,
	// or use AutoRelay.
	finalAddrs = slices.DeleteFunc(finalAddrs, func(a ma.Multiaddr) bool {
		return a.Equal(p2pCircuitAddr)
	})
	// Add certhashes for /webrtc-direct, /webtransport, etc addresses discovered
	// using identify.
	finalAddrs = a.addCertHashes(finalAddrs)
	return finalAddrs
}

func (a *addressService) appendInterfaceAddrs(result []ma.Multiaddr, listenAddrs []ma.Multiaddr) []ma.Multiaddr {
	// resolving any unspecified listen addressees to use only the primary
	// interface to avoid advertising too many addresses.
	if resolved, err := manet.ResolveUnspecifiedAddresses(listenAddrs, a.ifaceAddrs.Filtered()); err != nil {
		log.Warnw("failed to resolve listen addrs", "error", err)
	} else {
		result = append(result, resolved...)
	}
	result = ma.Unique(result)
	return result
}

func (a *addressService) appendNATAddrs(result []ma.Multiaddr, listenAddrs []ma.Multiaddr) []ma.Multiaddr {
	ifaceAddrs := a.ifaceAddrs.All()
	// use nat mappings if we have them
	if a.natmgr != nil && a.natmgr.HasDiscoveredNAT() {
		// we have a NAT device
		for _, listen := range listenAddrs {
			extMaddr := a.natmgr.GetMapping(listen)
			result = appendValidNATAddrs(result, listen, extMaddr, a.observedAddrsService.ObservedAddrsFor, ifaceAddrs)
		}
	} else {
		if a.observedAddrsService != nil {
			result = append(result, a.observedAddrsService.OwnObservedAddrs()...)
		}
	}
	return result
}

func (a *addressService) addCertHashes(addrs []ma.Multiaddr) []ma.Multiaddr {
	// This is a temporary workaround/hack that fixes #2233. Once we have a
	// proper address pipeline, rework this. See the issue for more context.
	type transportForListeninger interface {
		TransportForListening(a ma.Multiaddr) transport.Transport
	}

	type addCertHasher interface {
		AddCertHashes(m ma.Multiaddr) (ma.Multiaddr, bool)
	}

	s, ok := a.net.(transportForListeninger)
	if !ok {
		return addrs
	}

	// Copy addrs slice since we'll be modifying it.
	addrsOld := addrs
	addrs = make([]ma.Multiaddr, len(addrsOld))
	copy(addrs, addrsOld)

	for i, addr := range addrs {
		wtOK, wtN := libp2pwebtransport.IsWebtransportMultiaddr(addr)
		webrtcOK, webrtcN := libp2pwebrtc.IsWebRTCDirectMultiaddr(addr)
		if (wtOK && wtN == 0) || (webrtcOK && webrtcN == 0) {
			t := s.TransportForListening(addr)
			tpt, ok := t.(addCertHasher)
			if !ok {
				continue
			}
			addrWithCerthash, added := tpt.AddCertHashes(addr)
			if !added {
				log.Debugf("Couldn't add certhashes to multiaddr: %s", addr)
				continue
			}
			addrs[i] = addrWithCerthash
		}
	}
	return addrs
}

func (a *addressService) makeUpdatedAddrEvent(prev, current []ma.Multiaddr) *event.EvtLocalAddressesUpdated {
	if prev == nil && current == nil {
		return nil
	}
	prevmap := make(map[string]ma.Multiaddr, len(prev))
	currmap := make(map[string]ma.Multiaddr, len(current))
	evt := &event.EvtLocalAddressesUpdated{Diffs: true}
	addrsAdded := false

	for _, addr := range prev {
		prevmap[string(addr.Bytes())] = addr
	}
	for _, addr := range current {
		currmap[string(addr.Bytes())] = addr
	}
	for _, addr := range currmap {
		_, ok := prevmap[string(addr.Bytes())]
		updated := event.UpdatedAddress{Address: addr}
		if ok {
			updated.Action = event.Maintained
		} else {
			updated.Action = event.Added
			addrsAdded = true
		}
		evt.Current = append(evt.Current, updated)
		delete(prevmap, string(addr.Bytes()))
	}
	for _, addr := range prevmap {
		updated := event.UpdatedAddress{Action: event.Removed, Address: addr}
		evt.Removed = append(evt.Removed, updated)
	}

	if !addrsAdded && len(evt.Removed) == 0 {
		return nil
	}

	// Our addresses have changed. Make a new signed peer record.
	if a.peerRecord != nil {
		// add signed peer record to the event
		sr, err := a.peerRecord(current)
		if err != nil {
			log.Errorf("error creating a signed peer record from the set of current addresses, err=%s", err)
			// drop this change
			return nil
		}
		evt.SignedPeerRecord = sr
	}

	return evt
}

const ifaceAddrsTTL = time.Minute

type interfaceAddrsCache struct {
	mx                     sync.RWMutex
	filtered               []ma.Multiaddr
	all                    []ma.Multiaddr
	updateLocalIPv4Backoff backoff.ExpBackoff
	updateLocalIPv6Backoff backoff.ExpBackoff
	lastUpdated            time.Time
}

func (i *interfaceAddrsCache) Filtered() []ma.Multiaddr {
	i.mx.RLock()
	if time.Now().After(i.lastUpdated.Add(ifaceAddrsTTL)) {
		i.mx.RUnlock()
		return i.update(true)
	}
	defer i.mx.RUnlock()
	return i.filtered
}

func (i *interfaceAddrsCache) All() []ma.Multiaddr {
	i.mx.RLock()
	if time.Now().After(i.lastUpdated.Add(ifaceAddrsTTL)) {
		i.mx.RUnlock()
		return i.update(false)
	}
	defer i.mx.RUnlock()
	return i.all
}

func (i *interfaceAddrsCache) update(filtered bool) []ma.Multiaddr {
	i.mx.Lock()
	defer i.mx.Unlock()
	if !time.Now().After(i.lastUpdated.Add(ifaceAddrsTTL)) {
		if filtered {
			return i.filtered
		}
		return i.all
	}
	i.updateUnlocked()
	i.lastUpdated = time.Now()
	if filtered {
		return i.filtered
	}
	return i.all
}

func (i *interfaceAddrsCache) updateUnlocked() {
	i.filtered = nil
	i.all = nil

	// Try to use the default ipv4/6 addresses.
	// TODO: Remove this. We should advertise all interface addresses.
	if r, err := netroute.New(); err != nil {
		log.Debugw("failed to build Router for kernel's routing table", "error", err)
	} else {

		var localIPv4 net.IP
		var ran bool
		err, ran = i.updateLocalIPv4Backoff.Run(func() error {
			_, _, localIPv4, err = r.Route(net.IPv4zero)
			return err
		})

		if ran && err != nil {
			log.Debugw("failed to fetch local IPv4 address", "error", err)
		} else if ran && localIPv4.IsGlobalUnicast() {
			maddr, err := manet.FromIP(localIPv4)
			if err == nil {
				i.filtered = append(i.filtered, maddr)
			}
		}

		var localIPv6 net.IP
		err, ran = i.updateLocalIPv6Backoff.Run(func() error {
			_, _, localIPv6, err = r.Route(net.IPv6unspecified)
			return err
		})

		if ran && err != nil {
			log.Debugw("failed to fetch local IPv6 address", "error", err)
		} else if ran && localIPv6.IsGlobalUnicast() {
			maddr, err := manet.FromIP(localIPv6)
			if err == nil {
				i.filtered = append(i.filtered, maddr)
			}
		}
	}

	// Resolve the interface addresses
	ifaceAddrs, err := manet.InterfaceMultiaddrs()
	if err != nil {
		// This usually shouldn't happen, but we could be in some kind
		// of funky restricted environment.
		log.Errorw("failed to resolve local interface addresses", "error", err)

		// Add the loopback addresses to the filtered addrs and use them as the non-filtered addrs.
		// Then bail. There's nothing else we can do here.
		i.filtered = append(i.filtered, manet.IP4Loopback, manet.IP6Loopback)
		i.all = i.filtered
		return
	}

	// remove link local ipv6 addresses
	i.all = slices.DeleteFunc(ifaceAddrs, manet.IsIP6LinkLocal)

	// If netroute failed to get us any interface addresses, use all of
	// them.
	if len(i.filtered) == 0 {
		// Add all addresses.
		i.filtered = i.all
	} else {
		// Only add loopback addresses. Filter these because we might
		// not _have_ an IPv6 loopback address.
		for _, addr := range i.all {
			if manet.IsIPLoopback(addr) {
				i.filtered = append(i.filtered, addr)
			}
		}
	}
}

func getAllPossibleLocalAddrs(listenAddr ma.Multiaddr, ifaceAddrs []ma.Multiaddr) []ma.Multiaddr {
	// If the nat mapping fails, use the observed addrs
	resolved, err := manet.ResolveUnspecifiedAddress(listenAddr, ifaceAddrs)
	if err != nil {
		log.Warnf("failed to resolve listen addr %s, %s: %s", listenAddr, ifaceAddrs, err)
		return nil
	}
	return append(resolved, listenAddr)
}

// appendValidNATAddrs adds the NAT-ed addresses to the result. If the NAT device doesn't provide
// us with a public IP address, we use the observed addresses.
func appendValidNATAddrs(result []ma.Multiaddr, listenAddr ma.Multiaddr, natMapping ma.Multiaddr,
	obsAddrsFunc func(ma.Multiaddr) []ma.Multiaddr,
	ifaceAddrs []ma.Multiaddr) []ma.Multiaddr {
	if natMapping == nil {
		allAddrs := getAllPossibleLocalAddrs(listenAddr, ifaceAddrs)
		for _, a := range allAddrs {
			result = append(result, obsAddrsFunc(a)...)
		}
		return result
	}

	// if the router reported a sane address, use it.
	if !manet.IsIPUnspecified(natMapping) {
		result = append(result, natMapping)
	} else {
		log.Warn("NAT device reported an unspecified IP as it's external address")
	}

	// If the router gave us a public address, use it and ignore observed addresses
	if manet.IsPublicAddr(natMapping) {
		return result
	}

	// Router gave us a private IP; maybe we're behind a CGNAT.
	// See if we have a public IP from observed addresses.
	_, extMaddrNoIP := ma.SplitFirst(natMapping)
	if extMaddrNoIP == nil {
		return result
	}

	allAddrs := getAllPossibleLocalAddrs(listenAddr, ifaceAddrs)
	for _, addr := range allAddrs {
		for _, obsMaddr := range obsAddrsFunc(addr) {
			// Extract a public observed addr.
			ip, _ := ma.SplitFirst(obsMaddr)
			if ip == nil || !manet.IsPublicAddr(ip) {
				continue
			}
			result = append(result, ma.Join(ip, extMaddrNoIP))
		}
	}
	return result
}
