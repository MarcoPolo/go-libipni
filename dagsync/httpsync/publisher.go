package httpsync

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime"
	"github.com/ipld/go-ipld-prime/codec/dagjson"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	basicnode "github.com/ipld/go-ipld-prime/node/basic"
	ic "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// Publisher serves an advertisement chain over HTTP.
type Publisher struct {
	addr        multiaddr.Multiaddr
	closer      io.Closer
	lsys        ipld.LinkSystem
	handlerPath string
	peerID      peer.ID
	privKey     ic.PrivKey
	lock        sync.Mutex
	root        cid.Cid
}

var _ http.Handler = (*Publisher)(nil)

// NewPublisher creates a new http publisher, listening on the specified
// address.
func NewPublisher(address string, lsys ipld.LinkSystem, privKey ic.PrivKey) (*Publisher, error) {
	if privKey == nil {
		return nil, errors.New("private key required to sign head requests")
	}
	peerID, err := peer.IDFromPrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("could not get peer id from private key: %w", err)
	}

	l, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	maddr, err := manet.FromNetAddr(l.Addr())
	if err != nil {
		l.Close()
		return nil, err
	}
	proto, _ := multiaddr.NewMultiaddr("/http")

	pub := &Publisher{
		addr:    multiaddr.Join(maddr, proto),
		closer:  l,
		lsys:    lsys,
		peerID:  peerID,
		privKey: privKey,
	}

	// Run service on configured port.
	server := &http.Server{
		Handler: pub,
		Addr:    l.Addr().String(),
	}
	go server.Serve(l)

	return pub, nil
}

// NewPublisherForListener creates a new http publisher for an existing
// listener. When providing an existing listener, running the HTTP server
// is the caller's responsibility. ServeHTTP on the returned Publisher
// can be used to handle requests. handlerPath is the path to handle
// requests on, e.g. "ipni" for `/ipni/...` requests.
//
// DEPRECATED: use NewPublisherWithoutServer(listener.Addr(), ...)
func NewPublisherForListener(listener net.Listener, handlerPath string, lsys ipld.LinkSystem, privKey ic.PrivKey) (*Publisher, error) {
	return NewPublisherWithoutServer(listener.Addr().String(), handlerPath, lsys, privKey)
}

// NewPublisherWithoutServer creates a new http publisher for an existing
// network address. When providing an existing network address, running
// the HTTP server is the caller's responsibility. ServeHTTP on the
// returned Publisher can be used to handle requests. handlerPath is the
// path to handle requests on, e.g. "ipni" for `/ipni/...` requests.
func NewPublisherWithoutServer(address string, handlerPath string, lsys ipld.LinkSystem, privKey ic.PrivKey) (*Publisher, error) {
	if privKey == nil {
		return nil, errors.New("private key required to sign head requests")
	}
	peerID, err := peer.IDFromPrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("could not get peer id from private key: %w", err)
	}

	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, err
	}
	maddr, err := manet.FromNetAddr(addr)
	if err != nil {
		return nil, err
	}
	proto, _ := multiaddr.NewMultiaddr("/http")
	handlerPath = strings.TrimPrefix(handlerPath, "/")
	if handlerPath != "" {
		httpath, err := multiaddr.NewComponent("httpath", url.PathEscape(handlerPath))
		if err != nil {
			return nil, err
		}
		proto = multiaddr.Join(proto, httpath)
		handlerPath = "/" + handlerPath
	}

	return &Publisher{
		addr:        multiaddr.Join(maddr, proto),
		closer:      io.NopCloser(nil),
		lsys:        lsys,
		handlerPath: handlerPath,
		peerID:      peerID,
		privKey:     privKey,
	}, nil
}

// Addrs returns the addresses, as []multiaddress, that the Publisher is
// listening on.
func (p *Publisher) Addrs() []multiaddr.Multiaddr {
	return []multiaddr.Multiaddr{p.addr}
}

// ID returns the p2p peer ID of the Publisher.
func (p *Publisher) ID() peer.ID {
	return p.peerID
}

// Protocol returns the multihash protocol ID of the transport used by the
// publisher.
func (p *Publisher) Protocol() int {
	return multiaddr.P_HTTP
}

// SetRoot sets the head of the advertisement chain.
func (p *Publisher) SetRoot(c cid.Cid) {
	p.lock.Lock()
	p.root = c
	p.lock.Unlock()
}

// Close closes the Publisher.
func (p *Publisher) Close() error {
	return p.closer.Close()
}

// ServeHTTP implements the http.Handler interface.
func (p *Publisher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var ask string
	if p.handlerPath != "" {
		if !strings.HasPrefix(r.URL.Path, p.handlerPath) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		ask = path.Base(strings.TrimPrefix(r.URL.Path, p.handlerPath))
	} else {
		ask = path.Base(r.URL.Path)
	}
	if ask == "head" {
		// serve the head
		p.lock.Lock()
		rootCid := p.root
		p.lock.Unlock()

		if rootCid == cid.Undef {
			http.Error(w, "", http.StatusNoContent)
			return
		}
		marshalledMsg, err := newEncodedSignedHead(rootCid, p.privKey)
		if err != nil {
			http.Error(w, "Failed to encode", http.StatusInternalServerError)
			log.Errorw("Failed to serve root", "err", err)
		} else {
			_, _ = w.Write(marshalledMsg)
		}
		return
	}
	// interpret `ask` as a CID to serve.
	c, err := cid.Parse(ask)
	if err != nil {
		http.Error(w, "invalid request: not a cid", http.StatusBadRequest)
		return
	}
	item, err := p.lsys.Load(ipld.LinkContext{}, cidlink.Link{Cid: c}, basicnode.Prototype.Any)
	if err != nil {
		if errors.Is(err, ipld.ErrNotExists{}) {
			http.Error(w, "cid not found", http.StatusNotFound)
			return
		}
		http.Error(w, "unable to load data for cid", http.StatusInternalServerError)
		log.Errorw("Failed to load requested block", "err", err, "cid", c)
		return
	}
	// marshal to json and serve.
	_ = dagjson.Encode(item, w)

	// TODO: Sign message using publisher's private key.
}
