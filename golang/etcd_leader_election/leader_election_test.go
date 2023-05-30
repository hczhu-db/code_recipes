package etcd_leader_election

import (
	"log"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/framework/integration"
)

type testCluster struct {
	t       *testing.T
	cluster *integration.Cluster
}

func newTestCluster(t *testing.T) *testCluster {
	integration.BeforeTestExternal(t)
	return &testCluster{
		t: t,
		cluster: integration.NewCluster(t, &integration.ClusterConfig{
			Size: 1,
		}),
	}
}

func (tc *testCluster) close() {
	tc.cluster.Terminate(tc.t)
}

func (tc *testCluster) etcdClient() *clientv3.Client {
	return tc.cluster.RandClient()
}

func TestDialTimeout(t *testing.T) {
	_, err := StartLeaderElectionAsync(
		Config{
			EtcdSessionTTL: 3,
			ElectionPrefix: "TestSingleCampaign",
			EtcdEndpoint:   "http://localhost:80",
		},
		log.Default(),
	)
	assert.Error(t, err)
}

func TestSingleCampaign(t *testing.T) {
	tc := newTestCluster(t)
	defer func () {
		log.Default().Println("Closing the cluster")
		tc.close()
	}()

	le, err := StartLeaderElectionAsync(
		Config{
			EtcdSessionTTL: 3,
			ElectionPrefix: "TestSingleCampaign",
			EtcdClient: tc.etcdClient(),
		},
		log.Default(),
	)
	assert.NoError(t, err)
	select {
	case <-le.BecomeLeaderCh:
		break
	case <-time.After(5 * time.Second):
		t.Error("should have become leader")
	case <-le.ErrorCh:
		t.Error("should have become leader")
	}

	time.Sleep(5 * time.Second)
	select {
	case <-le.ErrorCh:
		t.Error("should have kept leadership")
	case <-time.After(10 * time.Second):
		break
	}
	le.Close(log.Default())

	select {
	case <-le.ErrorCh:
		break
	case <-time.After(5 * time.Second):
		t.Error("should have lost leadership")
	}
}
func TestLongLivedLeader(t *testing.T) {
	tc := newTestCluster(t)
	defer func () {
		log.Default().Println("Closing the cluster")
		tc.close()
	}()

	leader, err := StartLeaderElectionAsync(
		Config{
			EtcdSessionTTL: 2,
			ElectionPrefix: "TestLongLivedLeader",
			EtcdClient: tc.etcdClient(),
			InstanceId: "leader",
		},
		log.Default(),
	)
	require.NoError(t, err)
	defer leader.Close(log.Default())
	select {
	case <-leader.BecomeLeaderCh:
		break
	case <-time.After(5 * time.Second):
		t.Error("should have become leader")
	case <-leader.ErrorCh:
		t.Error("should have become leader")
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		follower, err := StartLeaderElectionAsync(
			Config{
				EtcdSessionTTL: 2,
				ElectionPrefix: "TestLongLivedLeader",
				EtcdClient: tc.etcdClient(),
				InstanceId: "follower",
			},
			log.Default(),
		)
		require.NoError(t, err)
		defer follower.Close(log.Default())
		select {
		case <-follower.BecomeLeaderCh:
			t.Error("shouldn't got leadership")
		case <-time.After(10 * time.Second):
			break
		}
	}()

	log.Default().Println("Waiting for follower to close.")
	wg.Wait()
	log.Default().Println("Checking that the leader keeps leadership.")
	select {
	case <-leader.ErrorCh:
		t.Error("should have kept leadership")
	case <-time.After(3 * time.Second):
		break
	}
}
func TestMultipleCampaigns(t *testing.T) {
	tc := newTestCluster(t)
	defer func () {
		log.Default().Println("Closing the cluster")
		tc.close()
	}()

	elections := make([]LeaderElection, 0)
	for i := 0; i < 3; i++ {
		le, err := StartLeaderElectionAsync(
			Config{
				EtcdSessionTTL: 3,
				ElectionPrefix: "TsetMultipleCampaigns",
				EtcdClient: tc.etcdClient(),
				InstanceId: "instance-" + strconv.Itoa(i),
			},
			log.Default(),
		)
		assert.NoError(t, err)
		assert.NotNil(t, le)
		elections = append(elections, le)
	}
	leaders := 0
	time.Sleep(time.Second * 2)
	for _, le := range elections {
		select {
		case <-le.BecomeLeaderCh:
			leaders++
		case <-time.After(3 * time.Second):
		}
	}
	assert.Equal(t, 1, leaders)
	for _, le := range elections {
		le.Close(log.Default())
	}
}
func TestYieldingLeadership(t *testing.T) {
	tc := newTestCluster(t)
	defer func () {
		log.Default().Println("Closing the cluster")
		tc.close()
	}()

	elections := make([]LeaderElection, 0)
	numInstances := 3
	for i := 0; i < numInstances; i++ {
		le, err := StartLeaderElectionAsync(
			Config{
				EtcdSessionTTL: 2,
				ElectionPrefix: "TestYieldingLeadership",
				EtcdClient: tc.etcdClient(),
				InstanceId: "instance-" + strconv.Itoa(i),
			},
			log.Default(),
		)
		assert.NoError(t, err)
		assert.NotNil(t, le)
		elections = append(elections, le)
	}
	isClosed := make([]bool, len(elections))
	for range elections {
		time.Sleep(time.Second * 3)
		leaders := 0
		leaderIdx := -1
		for i, le := range elections {
			if isClosed[i] {
				continue
			}
			select {
			case <-le.BecomeLeaderCh:
				leaders++
				leaderIdx = i
			case <-time.After(3 * time.Second):
			}
		}
		assert.Equal(t, 1, leaders)
		require.NotEqual(t, -1, leaderIdx)
		elections[leaderIdx].Close(log.Default())
		isClosed[leaderIdx] = true
		log.Default().Println("closed leader", leaderIdx)
	}
}
func TestLeaderDeath(t *testing.T) {
	tc := newTestCluster(t)
	defer func () {
		log.Default().Println("Closing the cluster")
		tc.close()
	}()

	leader, err := StartLeaderElectionAsync(
		Config{
			EtcdSessionTTL: 2,
			ElectionPrefix: "TestLongLivedLeader",
			EtcdClient: tc.etcdClient(),
			InstanceId: "leader",
		},
		log.Default(),
	)
	require.NoError(t, err)
	defer leader.Close(log.Default())
	select {
	case <-leader.BecomeLeaderCh:
		break
	case <-time.After(5 * time.Second):
		t.Error("should have become leader")
	case <-leader.ErrorCh:
		t.Error("should have become leader")
	}

	syncCh := make(chan struct{})
	go func() {
		follower, err := StartLeaderElectionAsync(
			Config{
				EtcdSessionTTL: 2,
				ElectionPrefix: "TestLongLivedLeader",
				EtcdClient: tc.etcdClient(),
				InstanceId: "follower",
			},
			log.Default(),
		)
		require.NoError(t, err)
		defer follower.Close(log.Default())
		select {
		case <-follower.BecomeLeaderCh:
			break
		case <-time.After(10 * time.Second):
			t.Error("should have become leader within 10 seconds")
		}
		syncCh <- struct{}{}
	}()
	leader.etcdSession.Close()
	select {
	case <-leader.ErrorCh:
		break
	case <-time.After(5 * time.Second):
		t.Error("should have lost leadership after session closed")
	}
	<-syncCh
}
func TestConcurrentCampaigns(t *testing.T) {
	tc := newTestCluster(t)
	defer func () {
		log.Default().Println("Closing the cluster")
		tc.close()
	}()

	electionParticipants := make(chan *LeaderElection, 3)
	leaderCh := make(chan *LeaderElection, 3)
	cl1 := tc.etcdClient()
	cl2 := tc.etcdClient()
	cl3 := tc.etcdClient()
	process := func(instaceId string, cl *clientv3.Client) {
		le, err := StartLeaderElectionAsync(
			Config{
				EtcdSessionTTL: 2,
				ElectionPrefix: "TestConcurrentCampaigns",
				EtcdClient: cl,
				InstanceId: instaceId,
			},
			log.Default(),
		)
		electionParticipants <- &le
		require.NoError(t, err)
		var wg sync.WaitGroup
		defer wg.Wait()

		cancelCh := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := <-le.ErrorCh
			log.Default().Println("Election error: ", err)
			close(cancelCh)
		}()
		
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-le.BecomeLeaderCh:
				log.Default().Println("became leader: ", instaceId)
				leaderCh <- &le
			case <-cancelCh:
			    break
			}
		}()
	}

	go process("one", cl1)
	go process("two", cl2)
	go process("three", cl3)

	leader1 := <-leaderCh
	leader1.Close(log.Default())

	leader2 := <-leaderCh
	for i := 0; i < 3; i++ {
		el := <- electionParticipants
		el.Close(log.Default())
	}
	leader2.Close(log.Default())
}

func TestBlockingWait(t *testing.T) {
	tc := newTestCluster(t)
	defer func () {
		log.Default().Println("Closing the cluster")
		tc.close()
	}()

	electionParticipants := make(chan *LeaderElection, 3)
	leaderCh := make(chan *LeaderElection, 3)
	cl1 := tc.etcdClient()
	cl2 := tc.etcdClient()
	cl3 := tc.etcdClient()

	process := func(instaceId string, cl *clientv3.Client) {
		le, err := StartLeaderElectionAsync(
			Config{
				EtcdSessionTTL: 2,
				ElectionPrefix: "TestBlockingWait",
				EtcdClient: cl,
				InstanceId: instaceId,
			},
			log.Default(),
		)
		electionParticipants <- &le
		require.NoError(t, err)
		if le.BlockingWaitForLeadership() {
			leaderCh <- &le
		}
	}

	go process("one", cl1)
	go process("two", cl2)
	go process("three", cl3)

	leader1 := <-leaderCh
	leader1.Close(log.Default())

	leader2 := <-leaderCh
	for i := 0; i < 3; i++ {
		el := <- electionParticipants
		el.Close(log.Default())
	}
	leader2.Close(log.Default())
}
