package etcd_leader_election

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
	_ "go.etcd.io/etcd/tests/v3/framework/integration"
)

type closable interface {
	Close() error
}

type Config struct {
	EtcdSessionTTL time.Duration
	ElectionPrefix	 string
	EtcdEndpoint string
	InstanceId string
	// This is used for unit tests only. Don't need to set it for production.
	EtcdClient *clientv3.Client
}

type LeaderElection struct {
	etcdClient *clientv3.Client
	etcdSession *concurrency.Session
	etcdElection *concurrency.Election
	cancelCampaign context.CancelFunc
	instanceId string
	isClosed atomic.Bool
	dontCloseEtcdClient bool

	// Once a caller is elected as a leader, it will receive a message on this channel.
	// The leader won't involuntarily lose the leadership as long as its etcd session is valid.
	// A session expires when the etcd servers don't receive a heartbeat from the client within the session TTL.
	// The leader doesn't need to proactively send heartbeats to the etcd servers. The etcd client library will do it automatically.
	// Buffer size = 1
	BecomeLeaderCh chan struct{}
	// An error can happen before or after the caller becomes a leader.
	// If an error happens before the caller becomes a leader, the caller will never become a leader.
	// If an error happens after the caller becomes a leader, the caller is not the leader anymore.
	// When an error happens, the caller should close the LeaderElection object.
	// Buffer size = 1
	AnyErrorCh chan error
}

// Will resign the leadership (if the caller is elected) and close the etcd session.
// "l" will be invalid after this call.
func (l *LeaderElection) Close(logger *log.Logger) {
	if l.isClosed.Swap(true) {
		logger.Println(l.instanceId, ": Already closed.")
		return 
	}
	logger.Println(l.instanceId, ": Canceling the campaign...")
	l.cancelCampaign()
	logger.Println(l.instanceId, ": Resigning the election...")
	l.etcdElection.Resign(context.Background())
	logger.Println(l.instanceId, ": Closing the etcd session...")
	l.etcdSession.Close()
	if !l.dontCloseEtcdClient {
		logger.Println(l.instanceId, ": Closing the etcd client...")
	    l.etcdClient.Close()
	}
}

func createEtcdClient(etcdEndpoint string) (*clientv3.Client, error) {
	timeout := time.Duration(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := clientv3.New(clientv3.Config{
    		Endpoints:   []string{etcdEndpoint},
    		DialTimeout: timeout,
    		Context:	 ctx,
	})	
	return client, err
}

func StartLeaderElectionAsync(config Config, logger *log.Logger) (LeaderElection, error){
	toClose := make([]closable, 0)
	defer func() {
		for i := int(len(toClose)) - 1; i >= 0; i-- {
			toClose[i].Close()
		}	
	}()

	client := config.EtcdClient
	dontCloseEtcdClient := true
	var err error = nil
	if client == nil {
		logger.Printf("Establishing connection to etcd endpoint: %s\n", config.EtcdEndpoint)
		client, err = createEtcdClient(config.EtcdEndpoint)
		dontCloseEtcdClient = false
	}
	if err != nil {
		logger.Printf("Failed to created an ETCD client with error: %v\n", err)
		return LeaderElection{}, err
	}
	toClose = append(toClose, client)
	logger.Println("Etcd connection is established successfully.")

	// If this caller exits without calling Resign() or Close(), the session will expire after the TTL
	// and the leadership will be lost, if this caller was the leader.
    session, err := concurrency.NewSession(client, concurrency.WithTTL(
		int(config.EtcdSessionTTL.Seconds()),
	))
	if err != nil {
		logger.Printf("Failed to created an ETCD session with error: %v\n", err)
		return LeaderElection{}, err
	}
	toClose = append(toClose, session)
	logger.Println("Etcd session is created successfully.")

	election := concurrency.NewElection(session, config.ElectionPrefix)
	campaignCtx, cancelCampaign := context.WithCancel(context.Background())
	becomeLeaderCh := make(chan struct{}, 1)
	anyErrorCh := make(chan error, 1)
	go func(campaignErrorCh chan error, becomeLeaderCh chan struct{}) {
		logger.Printf("%s: Obtaining leadership with etcd prefix: %s\n", config.InstanceId, config.ElectionPrefix)
		// This will block until the caller becomes the leader, an error occurs, or the context is cancelled.
		err := election.Campaign(campaignCtx, config.ElectionPrefix)
		if err == nil {
			logger.Printf("%s: I am the leader for election prefix: %s\n", config.InstanceId, config.ElectionPrefix)
			// The leader will hold the leadership until it resigns or the session expires. The session will keep alive by the underlying etcd client
			// automatically sending heartbeats to the etcd server. The session will expire if the etcd server does not receive heartbeats from the client within the session TTL.
			becomeLeaderCh <- struct{}{}
			logger.Printf("%s: Waiting for session done.\n", config.InstanceId)
			<-session.Done()
			logger.Printf("%s: The session is done. I am not the leader anymore for election prefix: %s\n", config.InstanceId, config.ElectionPrefix)
			anyErrorCh <- errors.New("the session is done. I am not the leader anymore")
		} else {
			logger.Printf("%s: Campaign() returned an error: %+v.\n", config.InstanceId, err)
			anyErrorCh <- err
		}
	}(anyErrorCh, becomeLeaderCh)

	toClose = toClose[:0]
	return LeaderElection{
		etcdClient: client,
		etcdSession: session,
		etcdElection: election,
		cancelCampaign: cancelCampaign,
		instanceId: config.InstanceId,
		isClosed: atomic.Bool{},
		dontCloseEtcdClient: dontCloseEtcdClient,

		BecomeLeaderCh: becomeLeaderCh,
		AnyErrorCh: anyErrorCh,
	}, nil
}
 
