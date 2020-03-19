package p2p

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	discovery "github.com/libp2p/go-libp2p-discovery"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	maddr "github.com/multiformats/go-multiaddr"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var joinPartyProtocol protocol.ID = "/p2p/join-party"

// TSSProtocolID protocol id used for tss
var TSSProtocolID protocol.ID = "/p2p/tss"

const (
	// TimeoutConnecting maximum time for wait for peers to connect
	TimeoutConnecting = time.Minute * 1
)

// Message that get transfer across the wire
type Message struct {
	PeerID  peer.ID
	Payload []byte
}

// Communication use p2p to broadcast messages among all the TSS nodes
type Communication struct {
	rendezvous       string // based on group
	bootstrapPeers   []maddr.Multiaddr
	logger           zerolog.Logger
	listenAddr       maddr.Multiaddr
	host             host.Host
	routingDiscovery *discovery.RoutingDiscovery
	wg               *sync.WaitGroup
	stopChan         chan struct{} // channel to indicate whether we should stop
	subscribers      map[THORChainTSSMessageType]*MessageIDSubscriber
	subscriberLocker *sync.Mutex
	streamCount      int64
	BroadcastMsgChan chan *BroadcastMsgChan
}

// NewCommunication create a new instance of Communication
func NewCommunication(rendezvous string, bootstrapPeers []maddr.Multiaddr, port int) (*Communication, error) {
	addr, err := maddr.NewMultiaddr(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port))
	if err != nil {
		return nil, fmt.Errorf("fail to create listen addr: %w", err)
	}
	return &Communication{
		rendezvous:       rendezvous,
		bootstrapPeers:   bootstrapPeers,
		logger:           log.With().Str("module", "communication").Logger(),
		listenAddr:       addr,
		wg:               &sync.WaitGroup{},
		stopChan:         make(chan struct{}),
		subscribers:      make(map[THORChainTSSMessageType]*MessageIDSubscriber),
		subscriberLocker: &sync.Mutex{},
		streamCount:      0,
		BroadcastMsgChan: make(chan *BroadcastMsgChan, 1024),
	}, nil
}

// GetHost return the host
func (c *Communication) GetHost() host.Host {
	return c.host
}

// GetLocalPeerID from p2p host
func (c *Communication) GetLocalPeerID() string {
	return c.host.ID().String()
}

// Broadcast message to Peers
func (c *Communication) Broadcast(peers []peer.ID, msg []byte) {
	// try to discover all peers and then broadcast the messages
	c.wg.Add(1)
	go c.broadcastToPeers(peers, msg)
}

func (c *Communication) broadcastToPeers(peers []peer.ID, msg []byte) {
	defer c.wg.Done()
	defer func() {
		c.logger.Debug().Msgf("finished sending message to peer(%v)", peers)
	}()

	for _, p := range peers {
		if err := c.writeToStream(p, msg); nil != err {
			c.logger.Error().Err(err).Msg("fail to write to stream")
		}
	}
}

func (c *Communication) shouldWeWriteToPeer(ai peer.AddrInfo, peers []peer.ID) bool {
	if len(peers) == 0 {
		// broadcast to everyone
		return true
	}
	for _, p := range peers {
		if ai.ID.String() == p.String() {
			return true
		}
	}
	return false
}

func (c *Communication) writeToStream(pID peer.ID, msg []byte) error {
	// don't send to ourself
	if pID == c.host.ID() {
		return nil
	}
	stream, err := c.connectToOnePeer(pID)
	if err != nil {
		return fmt.Errorf("fail to open stream to peer(%s): %w", pID, err)
	}
	if nil == stream {
		return nil
	}

	defer func() {
		if err := stream.Close(); nil != err {
			c.logger.Error().Err(err).Msgf("fail to reset stream to peer(%s)", pID)
		}
	}()
	c.logger.Debug().Msgf(">>>writing messages to peer(%s)", pID)

	return WriteStreamWithBuffer(msg, stream)
}

func (c *Communication) readFromStream(stream network.Stream) {
	peerID := stream.Conn().RemotePeer().String()
	c.logger.Debug().Msgf("reading from stream of peer: %s", peerID)
	defer func() {
		if err := stream.Close(); nil != err {
			c.logger.Error().Err(err).Msg("fail to close stream")
		}
	}()

	select {
	case <-c.stopChan:
		return
	default:
		dataBuf, err := ReadStreamWithBuffer(stream)
		if err != nil {
			c.logger.Error().Err(err).Msgf("fail to read from stream,peerID: %s", peerID)
			return
		}
		var wrappedMsg WrappedMessage
		if err := json.Unmarshal(dataBuf, &wrappedMsg); nil != err {
			c.logger.Error().Err(err).Msg("fail to unmarshal wrapped message bytes")
			return
		}
		c.logger.Debug().Msgf(">>>>>>>[%s] %s", wrappedMsg.MessageType, string(wrappedMsg.Payload))
		channel := c.getSubscriber(wrappedMsg.MessageType, wrappedMsg.MsgID)
		if nil == channel {
			c.logger.Info().Msgf("no MsgID %s found for this message", wrappedMsg.MsgID)
			return
		}
		channel <- &Message{
			PeerID:  stream.Conn().RemotePeer(),
			Payload: dataBuf,
		}

	}
}

func (c *Communication) handleStream(stream network.Stream) {
	peerID := stream.Conn().RemotePeer().String()
	c.logger.Debug().Msgf("handle stream from peer: %s", peerID)
	// we will read from that stream
	c.readFromStream(stream)
}

func (c *Communication) startChannel(privKeyBytes []byte) error {
	ctx := context.Background()
	p2pPriKey, err := crypto.UnmarshalSecp256k1PrivateKey(privKeyBytes)
	if err != nil {
		c.logger.Error().Msgf("error is %f", err)
		return err
	}

	h, err := libp2p.New(ctx,
		libp2p.ListenAddrs([]maddr.Multiaddr{c.listenAddr}...),
		libp2p.Identity(p2pPriKey),
	)
	if err != nil {
		return fmt.Errorf("fail to create p2p host: %w", err)
	}
	c.host = h
	c.logger.Info().Msgf("Host created, we are: %s, at: %s", h.ID(), h.Addrs())
	h.SetStreamHandler(TSSProtocolID, c.handleStream)
	// Start a DHT, for use in peer discovery. We can't just make a new DHT
	// client because we want each peer to maintain its own local copy of the
	// DHT, so that the bootstrapping node of the DHT can go down without
	// inhibiting future peer discovery.
	kademliaDHT, err := dht.New(ctx, h)
	if err != nil {
		return fmt.Errorf("fail to create DHT: %w", err)
	}
	c.logger.Debug().Msg("Bootstrapping the DHT")
	if err = kademliaDHT.Bootstrap(ctx); err != nil {
		return fmt.Errorf("fail to bootstrap DHT: %w", err)
	}
	if err := c.connectToBootstrapPeers(); nil != err {
		return fmt.Errorf("fail to connect to bootstrap peer: %w", err)
	}
	// We use a rendezvous point "meet me here" to announce our location.
	// This is like telling your friends to meet you at the Eiffel Tower.

	routingDiscovery := discovery.NewRoutingDiscovery(kademliaDHT)
	discovery.Advertise(ctx, routingDiscovery, c.rendezvous)
	c.routingDiscovery = routingDiscovery
	c.logger.Info().Msg("Successfully announced!")

	return nil
}

func (c *Communication) connectToOnePeer(pID peer.ID) (network.Stream, error) {
	c.logger.Debug().Msgf("peer:%s,current:%s", pID, c.host.ID())
	// dont connect to itself
	if pID == c.host.ID() {
		return nil, nil
	}
	c.logger.Debug().Msgf("connect to peer : %s", pID.String())
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutConnecting)
	defer cancel()
	stream, err := c.host.NewStream(ctx, pID, TSSProtocolID)
	if err != nil {
		return nil, fmt.Errorf("fail to create new stream to peer: %s, %w", pID, err)
	}
	return stream, nil
}

func (c *Communication) connectToBootstrapPeers() error {
	// Let's connect to the bootstrap nodes first. They will tell us about the
	// other nodes in the network.
	var wg sync.WaitGroup
	for _, peerAddr := range c.bootstrapPeers {
		pi, err := peer.AddrInfoFromP2pAddr(peerAddr)
		if err != nil {
			return fmt.Errorf("fail to add peer: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), TimeoutConnecting)
			defer cancel()
			if err := c.host.Connect(ctx, *pi); err != nil {
				c.logger.Error().Err(err)
				return
			}
			c.logger.Info().Msgf("Connection established with bootstrap node: %s", *pi)
		}()
	}
	wg.Wait()
	return nil
}

// Start will start the communication
func (c *Communication) Start(priKeyBytes []byte) error {
	return c.startChannel(priKeyBytes)
}

// Stop communication
func (c *Communication) Stop() error {
	// we need to stop the handler and the p2p services firstly, then terminate the our communication threads
	if err := c.host.Close(); err != nil {
		c.logger.Err(err).Msg("fail to close host network")
	}

	close(c.stopChan)
	c.wg.Wait()
	return nil
}

func (c *Communication) SetSubscribe(topic THORChainTSSMessageType, msgID string, channel chan *Message) {
	c.subscriberLocker.Lock()
	defer c.subscriberLocker.Unlock()

	messageIDSubscribers, ok := c.subscribers[topic]
	if !ok {
		messageIDSubscribers = NewMessageIDSubscriber()
		c.subscribers[topic] = messageIDSubscribers
	}
	messageIDSubscribers.Subscribe(msgID, channel)
}

func (c *Communication) getSubscriber(topic THORChainTSSMessageType, msgID string) chan *Message {
	c.subscriberLocker.Lock()
	defer c.subscriberLocker.Unlock()
	messageIDSubscribers, ok := c.subscribers[topic]
	if !ok {
		c.logger.Debug().Msgf("fail to find subscribers for %s", topic)
		return nil
	}
	return messageIDSubscribers.GetSubscriber(msgID)
}

func (c *Communication) CancelSubscribe(topic THORChainTSSMessageType, msgID string) {
	c.subscriberLocker.Lock()
	defer c.subscriberLocker.Unlock()

	messageIDSubscribers, ok := c.subscribers[topic]
	if !ok {
		c.logger.Debug().Msgf("cannot find the given channels %s", topic.String())
		return
	}
	if nil == messageIDSubscribers {
		return
	}
	messageIDSubscribers.UnSubscribe(msgID)
	if messageIDSubscribers.IsEmpty() {
		delete(c.subscribers, topic)
	}
}

func (c *Communication) ProcessBroadcast() {
	c.logger.Info().Msg("start to process broadcast message channel")
	c.wg.Add(1)
	defer c.logger.Info().Msg("stop process broadcast message channel")
	defer c.wg.Done()
	for {
		select {
		case msg := <-c.BroadcastMsgChan:
			wrappedMsgBytes, err := json.Marshal(msg.WrappedMessage)
			if err != nil {
				c.logger.Error().Err(err).Msg("fail to marshal a wrapped message to json bytes")
				continue
			}
			c.logger.Debug().Msgf("broadcast message %s to %+v", msg.WrappedMessage, msg.PeersID)
			c.Broadcast(msg.PeersID, wrappedMsgBytes)

		case <-c.stopChan:
			return
		}
	}
}
