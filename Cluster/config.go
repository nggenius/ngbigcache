package clusteredBigCache

import "github.com/nggenius/ngbigcache/message"

type getReplyData struct {
	data []byte
}

type getRequestData struct {
	key       string
	randStr   string
	replyChan chan *getReplyData
	done      chan struct{}
}

type getRequestDataWrapper struct {
	g *getRequestData
	r *remoteNode
}

type replicationMsg struct {
	r *remoteNode
	m message.NodeMessage
}

//DefaultClusterConfig creates a new default configuration
func DefaultClusterConfig() *ClusteredBigCacheConfig {

	return &ClusteredBigCacheConfig{
		Join:                    false,
		BindAll:                 true,
		LocalPort:               9911,
		ConnectRetries:          5,
		TerminateOnListenerExit: false,
		ReplicationFactor:       1,
		WriteAck:                true,
		ReplicationMode:         REPLICATION_MODE_FULL_REPLICATE,
		ReconnectOnDisconnect:   false,
	}
}
