// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	"time"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/service"
)

// Config represents configuration for cadence-history service
type Config struct {
	NumberOfShards int

	// HistoryCache settings
	HistoryCacheInitialSize int
	HistoryCacheMaxSize     int
	HistoryCacheTTL         time.Duration

	// ShardController settings
	RangeSizeBits        uint
	AcquireShardInterval time.Duration

	// Timeout settings
	DefaultScheduleToStartActivityTimeoutInSecs int32
	DefaultScheduleToCloseActivityTimeoutInSecs int32
	DefaultStartToCloseActivityTimeoutInSecs    int32

	// TimerQueueProcessor settings
	TimerTaskBatchSize                    int
	ProcessTimerTaskWorkerCount           int
	TimerProcessorUpdateFailureRetryCount int
	TimerProcessorGetFailureRetryCount    int
	TimerProcessorUpdateAckInterval       time.Duration

	// TransferQueueProcessor settings
	TransferTaskBatchSize              int
	TransferProcessorMaxPollRPS        int
	TransferProcessorMaxPollInterval   time.Duration
	TransferProcessorUpdateAckInterval time.Duration
	TransferTaskWorkerCount            int
}

// NewConfig returns new service config with default values
func NewConfig(numberOfShards int) *Config {
	return &Config{
		NumberOfShards:                              numberOfShards,
		HistoryCacheInitialSize:                     256,
		HistoryCacheMaxSize:                         1 * 1024,
		HistoryCacheTTL:                             time.Hour,
		RangeSizeBits:                               20, // 20 bits for sequencer, 2^20 sequence number for any range
		AcquireShardInterval:                        time.Minute,
		DefaultScheduleToStartActivityTimeoutInSecs: 10,
		DefaultScheduleToCloseActivityTimeoutInSecs: 10,
		DefaultStartToCloseActivityTimeoutInSecs:    10,
		TimerTaskBatchSize:                          100,
		ProcessTimerTaskWorkerCount:                 30,
		TimerProcessorUpdateFailureRetryCount:       5,
		TimerProcessorGetFailureRetryCount:          5,
		TimerProcessorUpdateAckInterval:             10 * time.Second,
		TransferTaskBatchSize:                       10,
		TransferProcessorMaxPollRPS:                 100,
		TransferProcessorMaxPollInterval:            10 * time.Second,
		TransferProcessorUpdateAckInterval:          10 * time.Second,
		TransferTaskWorkerCount:                     10,
	}
}

// Service represents the cadence-history service
type Service struct {
	stopC         chan struct{}
	params        *service.BootstrapParams
	config        *Config
	metricsClient metrics.Client
}

// NewService builds a new cadence-history service
func NewService(params *service.BootstrapParams, config *Config) common.Daemon {
	return &Service{
		params: params,
		stopC:  make(chan struct{}),
		config: config,
	}
}

// Start starts the service
func (s *Service) Start() {

	var p = s.params
	var log = p.Logger

	log.Infof("%v starting", common.HistoryServiceName)

	base := service.New(p)

	s.metricsClient = base.GetMetricsClient()

	shardMgr, err := persistence.NewCassandraShardPersistence(p.CassandraConfig.Hosts,
		p.CassandraConfig.Port,
		p.CassandraConfig.User,
		p.CassandraConfig.Password,
		p.CassandraConfig.Datacenter,
		p.CassandraConfig.Keyspace,
		p.Logger)

	if err != nil {
		log.Fatalf("failed to create shard manager: %v", err)
	}
	shardMgr = persistence.NewShardPersistenceClient(shardMgr, base.GetMetricsClient())

	// Hack to create shards for bootstrap purposes
	// TODO: properly pre-create all shards before deployment.
	for shardID := 0; shardID < p.CassandraConfig.NumHistoryShards; shardID++ {
		err := shardMgr.CreateShard(&persistence.CreateShardRequest{
			ShardInfo: &persistence.ShardInfo{
				ShardID:          shardID,
				RangeID:          0,
				TransferAckLevel: 0,
			}})

		if err != nil {
			if _, ok := err.(*persistence.ShardAlreadyExistError); !ok {
				log.Fatalf("failed to create shard for ShardId: %v, with error: %v", shardID, err)
			}
		}
	}

	metadata, err := persistence.NewCassandraMetadataPersistence(p.CassandraConfig.Hosts,
		p.CassandraConfig.Port,
		p.CassandraConfig.User,
		p.CassandraConfig.Password,
		p.CassandraConfig.Datacenter,
		p.CassandraConfig.Keyspace,
		p.Logger)

	if err != nil {
		log.Fatalf("failed to create metadata manager: %v", err)
	}
	metadata = persistence.NewMetadataPersistenceClient(metadata, base.GetMetricsClient())

	visibility, err := persistence.NewCassandraVisibilityPersistence(p.CassandraConfig.Hosts,
		p.CassandraConfig.Port,
		p.CassandraConfig.User,
		p.CassandraConfig.Password,
		p.CassandraConfig.Datacenter,
		p.CassandraConfig.VisibilityKeyspace,
		p.Logger)

	if err != nil {
		log.Fatalf("failed to create visiblity manager: %v", err)
	}

	history, err := persistence.NewCassandraHistoryPersistence(p.CassandraConfig.Hosts,
		p.CassandraConfig.Port,
		p.CassandraConfig.User,
		p.CassandraConfig.Password,
		p.CassandraConfig.Datacenter,
		p.CassandraConfig.Keyspace,
		p.Logger)

	if err != nil {
		log.Fatalf("Creating Cassandra history manager persistence failed: %v", err)
	}

	history = persistence.NewHistoryPersistenceClient(history, base.GetMetricsClient())
	execMgrFactory := NewExecutionManagerFactory(&p.CassandraConfig, p.Logger, base.GetMetricsClient())

	handler := NewHandler(base,
		s.config,
		shardMgr,
		metadata,
		visibility,
		history,
		execMgrFactory)

	handler.Start()

	log.Infof("%v started", common.HistoryServiceName)

	<-s.stopC
	base.Stop()
}

// Stop stops the service
func (s *Service) Stop() {
	select {
	case s.stopC <- struct{}{}:
	default:
	}
	s.params.Logger.Infof("%v stopped", common.HistoryServiceName)
}
