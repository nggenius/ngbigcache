package clusteredBigCache

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"encoding/binary"
	"errors"

	"github.com/nggenius/ngbigcache/comms"
	"github.com/nggenius/ngbigcache/message"
	"github.com/nggenius/ngbigcache/utils"
	"github.com/oklog/run"
)

const (
	nodeStateConnecting = iota
	nodeStateConnected
	nodeStateDisconnected
	nodeStateHandshake
)

type remoteNodeState uint8

type nodeMetrics struct {
	pingSent     uint64
	pingRecieved uint64
	pongSent     uint64
	pongRecieved uint64
	dropedMsg    uint64
}

// remote node configuration
type remoteNodeConfig struct {
	Id                    string `json:"id"`
	IpAddress             string `json:"ip_address"`
	PingFailureThreshHold int32  `json:"ping_failure_thresh_hold"`
	PingInterval          int    `json:"ping_interval"`
	PingTimeout           int    `json:"ping_timeout"`
	ConnectRetries        int    `json:"connect_retries"`
	ServicePort           string `json:"service_port"`
	Sync                  bool   `json:"sync"`
	ReconnectOnDisconnect bool   `json:"reconnect_on_disconnect"`
}

// remote node definition
type remoteNode struct {
	config           *remoteNodeConfig
	metrics          *nodeMetrics
	connection       *comms.Connection
	parentNode       *ClusteredBigCache
	inboundMsgQueue  chan *message.NodeWireMessage
	outboundMsgQueue chan message.NodeMessage
	logger           utils.AppLogger
	state            remoteNodeState
	stateLock        sync.Mutex
	done             chan struct{}
	pingTimer        *time.Ticker //used to send ping message to remote
	pingTimeout      *time.Timer  //used to monitor ping response
	pingFailure      int32        //count the number of pings without response
	pendingGet       *sync.Map
	mode             byte
	wg               *sync.WaitGroup
}

//check configurations for sensible defaults
func checkConfig(logger utils.AppLogger, config *remoteNodeConfig) {

	if config.PingInterval < 1 {
		config.PingInterval = 5
	}

	if config.PingTimeout < 1 {
		config.PingTimeout = 3
	}

	if config.PingTimeout > config.PingInterval {
		utils.Warn(logger, "ping timeout is greater than ping interval, pings will NEVER timeout")
	}

	if config.PingFailureThreshHold == 0 {
		config.PingFailureThreshHold = 5
	}
}

//create a new remoteNode object
func newRemoteNode(config *remoteNodeConfig, parent *ClusteredBigCache, logger utils.AppLogger) *remoteNode {
	checkConfig(logger, config)
	return &remoteNode{
		config:           config,
		inboundMsgQueue:  make(chan *message.NodeWireMessage, CHAN_SIZE),
		outboundMsgQueue: make(chan message.NodeMessage, CHAN_SIZE),
		done:             make(chan struct{}),
		state:            nodeStateDisconnected,
		stateLock:        sync.Mutex{},
		parentNode:       parent,
		logger:           logger,
		metrics:          &nodeMetrics{},
		pendingGet:       &sync.Map{},
		wg:               &sync.WaitGroup{},
	}
}

//set the state of this remoteNode. always use this method because of the lock
func (r *remoteNode) setState(state remoteNodeState) {
	r.stateLock.Lock()
	defer r.stateLock.Unlock()

	r.state = state
}

//just set the connection for this remoteNode
func (r *remoteNode) setConnection(conn *comms.Connection) {
	r.connection = conn
}

//shut down this object
func (r *remoteNode) shutDown() {
	defer func() { recover() }()

	select {
	case r.done <- struct{}{}:
	default:
	}
}

//startup this remoteNode
func (r *remoteNode) start() {
	r.wg.Add(1)     //temporary increment
	var g run.Group //uses run.Group
	{
		g.Add(func() error { //this is to terminate this remoteNode from its parent
			<-r.done
			return errors.New("terminating")
		}, func(err error) {
			close(r.done)
		})
	}
	{
		done := make(chan struct{})
		g.Add(func() error { //this is for the ping timer
			r.pingTimer = time.NewTicker(time.Second * time.Duration(r.config.PingInterval))
			r.sendMessage(&message.PingMessage{}) //send the first ping message
			exit := false
			r.wg.Add(1)
			for {
				select {
				case <-r.pingTimer.C:
					atomic.AddUint64(&r.metrics.pingSent, 1)
					if !r.pingTimeout.Stop() {
						select {
						case <-r.pingTimeout.C:
						default:
						}
					}
					r.pingTimeout.Reset(time.Second * time.Duration(r.config.PingTimeout))
					r.sendMessage(&message.PingMessage{})
				case <-done: //we have this so that the goroutine would not linger after this node disconnects because
					exit = true //of the blocking channel in the above case statement
					break
				}

				if exit {
					break
				}
			}
			utils.Info(r.logger, fmt.Sprintf("shutting down ping timer goroutine for '%s'", r.config.Id))
			r.wg.Done()
			return errors.New("terminating timer goroutine")
		}, func(err error) {
			close(done)
		})
	}
	{
		done := make(chan struct{})
		g.Add(func() error { //this is for the ping response (pong) timer
			r.pingTimeout = time.NewTimer(time.Second * time.Duration(r.config.PingTimeout))
			fault := false
			exit := false
			r.wg.Add(1)
			for {
				select {
				case <-r.pingTimeout.C:
					if r.state != nodeStateHandshake {
						utils.Warn(r.logger,
							fmt.Sprintf("no ping response within configured time frame from remote node '%s'", r.config.Id))
					} else {
						utils.Warn(r.logger, "remote node not verified, therefore ping failing")
					}
					atomic.AddInt32(&r.pingFailure, 1)
					if r.pingFailure >= r.config.PingFailureThreshHold {
						r.pingTimeout.Stop()
						fault = true
						break
					}
				case <-done: //we have this so that the goroutine would not linger after this node disconnects because
					exit = true //of the blocking channel in the above case statement
					break
				}

				if exit || fault {
					break
				}
			}

			if fault {
				//the remote node is assumed to be 'dead' since it has not responded to recent ping request
				utils.Warn(r.logger, fmt.Sprintf("shutting down connection to remote node '%s' due to no ping response", r.config.Id))
			}
			utils.Info(r.logger, fmt.Sprintf("shutting down ping timeout goroutine for '%s'", r.config.Id))
			r.wg.Done()
			return errors.New("terminating timeout goroutine")
		}, func(err error) {
			close(done)
		})
	}
	{
		g.Add(func() error { //this is for the network consumer. ie reading data off the network
			r.wg.Add(1)
			r.networkConsumer()
			r.wg.Done()
			return errors.New("terminating networkConsumer")
		}, func(err error) {
			r.setState(nodeStateDisconnected)
		})
	}
	{
		g.Add(func() error { //this is for handle and dispatching messages
			r.wg.Add(1)
			r.handleMessage()
			r.wg.Done()
			return errors.New("terminating handleMessage")
		}, func(err error) {
			close(r.inboundMsgQueue)
			r.setState(nodeStateDisconnected)
		})
	}
	{
		g.Add(func() error { //this is for sending messages on the network
			r.wg.Add(1)
			r.networkSender()
			r.wg.Done()
			return errors.New("terminating networkSender")
		}, func(err error) {
			close(r.outboundMsgQueue)
			r.setState(nodeStateDisconnected)
		})
	}
	{
		g.Add(func() error { //this is for gracefully bringing down this remoteNode
			r.wg.Wait()
			r.tearDown()
			return errors.New("terminating tearDown")
		}, func(err error) {
			r.connection.Close()
			r.setState(nodeStateDisconnected)
			r.wg.Done() //decrement the temporary increment
		})
	}

	go func() {
		utils.Warn(r.logger, fmt.Sprintf("remoteNode '%s' shutting down, caused by: %q", r.config.Id, g.Run()))
	}()
	r.sendVerify()
}

//join a cluster. this will be called if 'join' in the config is set to true
func (r *remoteNode) join() {
	utils.Info(r.logger, "joining remote node via "+r.config.IpAddress)

	go func() { //goroutine will try to connect to the cluster until it succeeds or max tries reached
		r.setState(nodeStateConnecting)
		var err error
		tries := 0
		for {
			if err = r.connect(); err == nil {
				break
			}
			utils.Error(r.logger, err.Error())
			time.Sleep(time.Second * 3)
			if r.config.ConnectRetries > 0 {
				tries++
				if tries >= r.config.ConnectRetries {
					utils.Warn(r.logger, fmt.Sprintf("unable to connect to remote node '%s' after max retires", r.config.IpAddress))
					r.parentNode.eventUnableToConnect(r.config)
					return
				}
			}
		}
		utils.Info(r.logger, "connected to node via "+r.config.IpAddress)
		r.start()
	}()
}

//handles to low level connection to remote node
func (r *remoteNode) connect() error {
	var err error
	utils.Info(r.logger, "connecting to "+r.config.IpAddress)
	r.connection, err = comms.NewConnection(r.config.IpAddress, time.Second*5)
	if err != nil {
		return err
	}

	r.setState(nodeStateHandshake)

	return nil
}

//this is a goroutine dedicated in reading data from the network
//it does this by reading a 6bytes header which is
//	byte 1 - 4 == length of data
//	byte 5 & 6 == message code
//	the rest of the data based on length is the message body
func (r *remoteNode) networkConsumer() {

	for (r.state == nodeStateConnected) || (r.state == nodeStateHandshake) {

		header, err := r.connection.ReadData(6, 0) //read 6 byte header
		if nil != err {
			utils.Critical(r.logger, fmt.Sprintf("remote node '%s' has disconnected", r.config.Id))
			break
		}

		dataLength := binary.LittleEndian.Uint32(header) - 2 //subtracted 2 becos of message code
		msgCode := binary.LittleEndian.Uint16(header[4:])
		var data []byte
		if dataLength > 0 {
			data, err = r.connection.ReadData(uint(dataLength), 0)
			if nil != err {
				utils.Critical(r.logger, fmt.Sprintf("remote node '%s' has disconnected", r.config.Id))
				break
			}
		}
		r.queueInboundMessage(&message.NodeWireMessage{Code: msgCode, Data: data}) //queue message to be processed
	}
	utils.Info(r.logger, fmt.Sprintf("network consumer loop terminated... %s", r.config.Id))

}

//just queue the message in the outbound channel
func (r *remoteNode) sendMessage(msg message.NodeMessage) {
	defer func() { recover() }()

	if r.state == nodeStateDisconnected {
		return
	}

	select {
	case <-r.done:
	case r.outboundMsgQueue <- msg:
	}
}

//this sends a message on the network
//it builds the message using the following protocol
// bytes 1 & 2 == total length of the data (including the 2 byte message code)
// bytes 3 & 4 == message code
// bytes 5 upwards == message content
func (r *remoteNode) networkSender() {

	for m := range r.outboundMsgQueue {
		if r.state == nodeStateDisconnected {
			continue
		}
		msg := m.Serialize()
		data := make([]byte, 6+len(msg.Data))                        // 6 ==> 4bytes for length of message, 2bytes for message code
		binary.LittleEndian.PutUint32(data, uint32(len(msg.Data)+2)) //the 2 is for the message code
		binary.LittleEndian.PutUint16(data[4:], msg.Code)
		copy(data[6:], msg.Data)
		if err := r.connection.SendData(data); err != nil {
			utils.Critical(r.logger, fmt.Sprintf("unexpected error while sending %s data [%s]", message.MsgCodeToString(msg.Code), err))
			break
		}
	}
	utils.Info(r.logger, "terminated network sender for "+r.config.Id)
}

//bring down the remote node. should not be called from outside networkConsumer()
func (r *remoteNode) tearDown() {

	r.parentNode.eventRemoteNodeDisconneced(r)
	jq := r.parentNode.joinQueue
	if r.config.ReconnectOnDisconnect {
		jq <- &message.ProposedPeer{Id: r.config.Id, IpAddress: r.config.IpAddress}
	}

	if r.pingTimeout != nil {
		r.pingTimeout.Stop()
	}
	if r.pingTimer != nil {
		r.pingTimer.Stop()
	}

	r.pendingGet = nil
	utils.Info(r.logger, fmt.Sprintf("remote node '%s' completely shutdown", r.config.Id))
}

//just queue the message in a channel
func (r *remoteNode) queueInboundMessage(msg *message.NodeWireMessage) {
	defer func() { recover() }()

	if r.state == nodeStateHandshake { //when in the handshake state only accept MsgVERIFY and MsgVERIFYOK messages
		code := msg.Code
		if (code != message.MsgVERIFY) && (code != message.MsgVERIFYOK) {
			r.metrics.dropedMsg++
			return
		}
	}

	if r.state != nodeStateDisconnected {
		select {
		case <-r.done:
		case r.inboundMsgQueue <- msg:
		}
	}
}

//message handler
func (r *remoteNode) handleMessage() {

	for msg := range r.inboundMsgQueue {
		if r.state == nodeStateDisconnected {
			continue
		}
		switch msg.Code {
		case message.MsgVERIFY:
			if !r.handleVerify(msg) {
				return
			}
		case message.MsgVERIFYOK:
			r.handleVerifyOK()
		case message.MsgPING:
			r.handlePing()
		case message.MsgPONG:
			r.handlePong()
		case message.MsgSyncRsp:
			r.handleSyncResponse(msg)
		case message.MsgSyncReq:
			r.handleSyncRequest(msg)
		case message.MsgGETReq:
			r.handleGetRequest(msg)
		case message.MsgGETRsp:
			r.handleGetResponse(msg)
		case message.MsgPUT:
			r.handlePut(msg)
		case message.MsgDEL:
			r.handleDelete(msg)
		}
	}

	utils.Info(r.logger, fmt.Sprintf("terminated message handler goroutine for '%s'", r.config.Id))

}

//send a verify message. this is always the first message to be sent once a connection is established.
func (r *remoteNode) sendVerify() {
	verifyMsgRsp := message.VerifyMessage{Id: r.parentNode.config.Id,
		ServicePort: strconv.Itoa(r.parentNode.config.LocalPort), Mode: r.parentNode.mode}
	r.sendMessage(&verifyMsgRsp)
}

//use the verify message been sent by a remote node to configure the node in this system
func (r *remoteNode) handleVerify(msg *message.NodeWireMessage) bool {

	verifyMsgRsp := message.VerifyMessage{}
	verifyMsgRsp.DeSerialize(msg)

	r.config.Id = verifyMsgRsp.Id
	r.config.ServicePort = verifyMsgRsp.ServicePort
	r.mode = verifyMsgRsp.Mode

	//check if connecting node and this node are both in passive mode
	if verifyMsgRsp.Mode == clusterModePASSIVE {
		if r.parentNode.mode == clusterModePASSIVE { //passive nodes are not allowed to connect to each other
			utils.Warn(r.logger, fmt.Sprintf("node '%s' and '%s' are both passive nodes, shuting down the connection", r.parentNode.config.Id, verifyMsgRsp.Id))
			return false
		}
	}

	if !r.parentNode.eventVerifyRemoteNode(r) { //seek parent's node approval on this
		utils.Warn(r.logger, fmt.Sprintf("node already has remote node '%s' so shutdown new connection", r.config.Id))
		return false
	}

	if verifyMsgRsp.Mode == clusterModePASSIVE { //if the node is a passive node dont reconnect on disconnect
		r.config.ReconnectOnDisconnect = false
	}

	r.setState(nodeStateConnected)
	r.sendMessage(&message.VerifyOKMessage{}) //must reply back with a verify OK message if all goes well

	return true
}

//handles verify OK from a remote node. this allows this system to sync with remote node
func (r *remoteNode) handleVerifyOK() {
	go func() {
		count := 0
		for r.state == nodeStateHandshake {
			time.Sleep(time.Second * 1)
			count++
			if count >= 5 {
				utils.Warn(r.logger, fmt.Sprintf("node '%s' state refused to change out of handshake", r.config.Id))
				break
			}
		}
		if count < 5 {
			if r.config.Sync { //only sync if you are joining the cluster
				r.sendMessage(&message.SyncReqMessage{Mode: r.parentNode.mode})
			}
		}
	}()
}

//handles ping message from a remote node
func (r *remoteNode) handlePing() {
	atomic.AddUint64(&r.metrics.pingRecieved, 1)
	r.sendMessage(&message.PongMessage{})
	atomic.AddUint64(&r.metrics.pongSent, 1)
}

//handle a pong message from the remote node, reset flags
func (r *remoteNode) handlePong() {
	atomic.AddUint64(&r.metrics.pongRecieved, 1)
	if !r.pingTimeout.Stop() { //stop the timer since we got a response
		select {
		case <-r.pingTimeout.C:
		default:
		}
	}
	atomic.StoreInt32(&r.pingFailure, 0) //reset failure counter since we got a response
}

//build and send a sync message
func (r *remoteNode) sendSyncResponse(msg *message.SyncReqMessage) {
	values := r.parentNode.getRemoteNodes() //call this because of the lock that needs to be held by parentNode
	nodeList := make([]message.ProposedPeer, 0)
	for _, v := range values {
		n := v.(*remoteNode)
		if n.config.Id == r.config.Id {
			continue
		}
		if (n.mode == clusterModePASSIVE) && (msg.Mode == clusterModePASSIVE) {
			continue
		}
		host, _, _ := net.SplitHostPort(n.config.IpAddress)
		nodeList = append(nodeList, message.ProposedPeer{Id: n.config.Id, IpAddress: net.JoinHostPort(host, n.config.ServicePort)})
	}

	if len(nodeList) > 0 {
		r.sendMessage(&message.SyncRspMessage{List: nodeList, ReplicationFactor: r.parentNode.config.ReplicationFactor})
	}
}

//handles sync request by just sending a sync response
func (r *remoteNode) handleSyncRequest(msg *message.NodeWireMessage) {
	m := &message.SyncReqMessage{}
	m.DeSerialize(msg)
	r.sendSyncResponse(m)
}

//accept the sync response and send to parentNode for processing
func (r *remoteNode) handleSyncResponse(msg *message.NodeWireMessage) {
	syncMsg := message.SyncRspMessage{}
	syncMsg.DeSerialize(msg)
	r.parentNode.setReplicationFactor(syncMsg.ReplicationFactor)
	length := len(syncMsg.List)
	for x := 0; x < length; x++ {
		r.parentNode.joinQueue <- &syncMsg.List[x]
	}
}

func (r *remoteNode) getData(reqData *getRequestData) {
	if r.state == nodeStateDisconnected {
		return
	}
	randStr := reqData.randStr
	r.pendingGet.Store(reqData.key+randStr, reqData)
	r.sendMessage(&message.GetReqMessage{Key: reqData.key, PendingKey: reqData.key + randStr})
}

func (r *remoteNode) handleGetRequest(msg *message.NodeWireMessage) {
	reqMsg := message.GetReqMessage{}
	reqMsg.DeSerialize(msg)
	data, _ := r.parentNode.cache.Get(reqMsg.Key)
	r.sendMessage(&message.GetRspMessage{PendingKey: reqMsg.PendingKey, Data: data})
}

func (r *remoteNode) handleGetResponse(msg *message.NodeWireMessage) {
	rspMsg := message.GetRspMessage{}
	rspMsg.DeSerialize(msg)
	origReq, ok := r.pendingGet.Load(rspMsg.PendingKey)
	if !ok {
		utils.Error(r.logger, "handling get response without finding the pending key")
		return
	}

	r.pendingGet.Delete(rspMsg.PendingKey)
	if len(rspMsg.Data) < 1 {
		return
	}

	//some other remote node might have sent the data so we do not want to block forever on the channel hence the select
	reqData := origReq.(*getRequestData)
	select {
	case <-reqData.done:
	case reqData.replyChan <- &getReplyData{data: rspMsg.Data}:
	default:
	}
}

func (r *remoteNode) handlePut(msg *message.NodeWireMessage) {

	putMsg := message.PutMessage{}
	putMsg.DeSerialize(msg)
	if putMsg.Expiry == 0 {
		r.parentNode.cache.Set(putMsg.Key, putMsg.Data, 0)
	} else {
		t1 := time.Unix(int64(putMsg.Expiry), 0)
		t2 := t1.Sub(time.Now())
		r.parentNode.cache.Set(putMsg.Key, putMsg.Data, t2)
	}
}

func (r *remoteNode) handleDelete(msg *message.NodeWireMessage) {
	delMsg := message.DeleteMessage{}
	delMsg.DeSerialize(msg)
	r.parentNode.cache.Delete(delMsg.Key)
}
