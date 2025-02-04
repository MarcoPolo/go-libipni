package dagsync_test

import (
	"context"
	"log"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	_ "github.com/ipld/go-ipld-prime/codec/dagcbor"
	_ "github.com/ipld/go-ipld-prime/codec/dagjson"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	basicnode "github.com/ipld/go-ipld-prime/node/basic"
	"github.com/ipni/go-libipni/announce"
	"github.com/ipni/go-libipni/announce/p2psender"
	"github.com/ipni/go-libipni/dagsync"
	"github.com/ipni/go-libipni/dagsync/dtsync"
	"github.com/ipni/go-libipni/dagsync/test"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if runtime.GOARCH == "386" {
		log.Println("Skipping tests, cannot use GOARCH=386")
		return
	}

	// Run tests.
	os.Exit(m.Run())
}

func initPubSub(t *testing.T, srcStore, dstStore datastore.Batching, allowPeer func(peer.ID) bool) (host.Host, host.Host, dagsync.Publisher, *dagsync.Subscriber, announce.Sender) {
	srcHost := test.MkTestHost(t)
	dstHost := test.MkTestHost(t)

	topics := test.WaitForMeshWithMessage(t, testTopic, srcHost, dstHost)

	srcLnkS := test.MkLinkSystem(srcStore)

	p2pSender, err := p2psender.New(nil, "", p2psender.WithTopic(topics[0]), p2psender.WithExtraData([]byte("t01000")))
	require.NoError(t, err)

	pub, err := dtsync.NewPublisher(srcHost, srcStore, srcLnkS, testTopic)
	require.NoError(t, err)

	srcHost.Peerstore().AddAddrs(dstHost.ID(), dstHost.Addrs(), time.Hour)
	dstHost.Peerstore().AddAddrs(srcHost.ID(), srcHost.Addrs(), time.Hour)
	dstLnkS := test.MkLinkSystem(dstStore)

	sub, err := dagsync.NewSubscriber(dstHost, dstStore, dstLnkS, testTopic,
		dagsync.RecvAnnounce(announce.WithTopic(topics[1]), announce.WithAllowPeer(allowPeer)))
	require.NoError(t, err)

	err = srcHost.Connect(context.Background(), dstHost.Peerstore().PeerInfo(dstHost.ID()))
	require.NoError(t, err)

	require.NoError(t, test.WaitForP2PPublisher(pub, dstHost, testTopic))

	return srcHost, dstHost, pub, sub, p2pSender
}

func TestAllowPeerReject(t *testing.T) {
	t.Parallel()

	// Set function to reject anything except dstHost, which is not the one
	// generating the update.
	var destID peer.ID
	allow := func(peerID peer.ID) bool {
		return peerID == destID
	}

	// Init dagsync publisher and subscriber
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	srcHost, dstHost, pub, sub, sender := initPubSub(t, srcStore, dstStore, allow)
	defer srcHost.Close()
	defer dstHost.Close()
	defer pub.Close()
	defer sub.Close()

	destID = dstHost.ID()

	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	c := mkLnk(t, srcStore)

	// Update root with item
	pub.SetRoot(c)
	err := announce.Send(context.Background(), c, pub.Addrs(), sender)
	require.NoError(t, err)

	select {
	case <-time.After(3 * time.Second):
	case _, open := <-watcher:
		require.False(t, open, "something was exchanged, and that is wrong")
	}
}

func TestAllowPeerAllows(t *testing.T) {
	t.Parallel()

	// Set function to allow any peer.
	allow := func(_ peer.ID) bool {
		return true
	}

	// Init dagsync publisher and subscriber
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	srcHost, dstHost, pub, sub, sender := initPubSub(t, srcStore, dstStore, allow)
	defer srcHost.Close()
	defer dstHost.Close()
	defer pub.Close()
	defer sub.Close()

	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	c := mkLnk(t, srcStore)

	// Update root with item
	pub.SetRoot(c)
	err := announce.Send(context.Background(), c, pub.Addrs(), sender)
	require.NoError(t, err)

	select {
	case <-time.After(updateTimeout):
		t.Fatal("timed out waiting for SyncFinished")
	case <-watcher:
	}
}

func TestPublisherRejectsPeer(t *testing.T) {
	t.Parallel()
	// Init dagsync publisher and subscriber
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())

	srcHost := test.MkTestHost(t)
	dstHost := test.MkTestHost(t)

	topics := test.WaitForMeshWithMessage(t, testTopic, srcHost, dstHost)

	srcLnkS := test.MkLinkSystem(srcStore)

	blockID := dstHost.ID()
	var blockMutex sync.Mutex

	allowPeer := func(peerID peer.ID) bool {
		blockMutex.Lock()
		defer blockMutex.Unlock()
		return peerID != blockID
	}

	p2pSender, err := p2psender.New(nil, "", p2psender.WithTopic(topics[0]))
	require.NoError(t, err)

	pub, err := dtsync.NewPublisher(srcHost, srcStore, srcLnkS, testTopic, dtsync.WithAllowPeer(allowPeer))
	require.NoError(t, err)
	defer pub.Close()

	srcHost.Peerstore().AddAddrs(dstHost.ID(), dstHost.Addrs(), time.Hour)
	dstHost.Peerstore().AddAddrs(srcHost.ID(), srcHost.Addrs(), time.Hour)
	dstLnkS := test.MkLinkSystem(dstStore)

	sub, err := dagsync.NewSubscriber(dstHost, dstStore, dstLnkS, testTopic, dagsync.RecvAnnounce(announce.WithTopic(topics[1])))
	require.NoError(t, err)
	defer sub.Close()

	err = srcHost.Connect(context.Background(), dstHost.Peerstore().PeerInfo(dstHost.ID()))
	require.NoError(t, err)

	require.NoError(t, test.WaitForP2PPublisher(pub, dstHost, testTopic))

	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	c := mkLnk(t, srcStore)

	// Update root with item
	pub.SetRoot(c)
	err = announce.Send(context.Background(), c, pub.Addrs(), p2pSender)
	require.NoError(t, err)

	select {
	case <-time.After(updateTimeout):
		t.Log("publisher blocked")
	case <-watcher:
		t.Fatal("sync should not have happened with blocked ID")
	}

	blockMutex.Lock()
	blockID = peer.ID("")
	blockMutex.Unlock()

	c = mkLnk(t, srcStore)

	// Update root with item
	pub.SetRoot(c)
	err = announce.Send(context.Background(), c, pub.Addrs(), p2pSender)
	require.NoError(t, err)

	select {
	case <-time.After(updateTimeout):
		t.Fatal("timed out waiting for SyncFinished")
	case <-watcher:
		t.Log("synced with allowed ID")
	}
}

func TestIdleHandlerCleaner(t *testing.T) {
	t.Parallel()
	blocksSeenByHook := make(map[cid.Cid]struct{})
	blockHook := func(p peer.ID, c cid.Cid, _ dagsync.SegmentSyncActions) {
		blocksSeenByHook[c] = struct{}{}
	}

	ttl := time.Second
	te := setupPublisherSubscriber(t, []dagsync.Option{dagsync.BlockHook(blockHook), dagsync.IdleHandlerTTL(ttl)})

	rootLnk, err := test.Store(te.srcStore, basicnode.NewString("hello world"))
	require.NoError(t, err)
	te.pub.SetRoot(rootLnk.(cidlink.Link).Cid)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Do a sync to create the handler.
	peerInfo := peer.AddrInfo{
		ID:    te.srcHost.ID(),
		Addrs: te.pub.Addrs(),
	}
	_, err = te.sub.Sync(ctx, peerInfo, cid.Undef, nil)
	require.NoError(t, err)

	// Check that the handler is preeent by seeing if it can be removed.
	require.True(t, te.sub.RemoveHandler(te.srcHost.ID()), "Expected handler to be present")

	// Do another sync to re-create the handler.
	_, err = te.sub.Sync(ctx, peerInfo, cid.Undef, nil)
	require.NoError(t, err)

	// For long enough for the idle cleaner to remove the handler, and check
	// that it was removed.
	time.Sleep(3 * ttl)
	require.False(t, te.sub.RemoveHandler(te.srcHost.ID()), "Expected handler to already be removed")
}

func mkLnk(t *testing.T, srcStore datastore.Batching) cid.Cid {
	// Update root with item
	np := basicnode.Prototype__Any{}
	nb := np.NewBuilder()
	ma, _ := nb.BeginMap(2)
	require.NoError(t, ma.AssembleKey().AssignString("hey"))
	require.NoError(t, ma.AssembleValue().AssignString("it works!"))
	require.NoError(t, ma.AssembleKey().AssignString("yes"))
	require.NoError(t, ma.AssembleValue().AssignBool(true))
	require.NoError(t, ma.Finish())

	n := nb.Build()
	lnk, err := test.Store(srcStore, n)
	require.NoError(t, err)

	return lnk.(cidlink.Link).Cid
}
