// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package stats

import (
	_ "embed"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/ini.v1"

	pb "github.com/DataDog/datadog-agent/pkg/proto/pbgo/trace"
	"github.com/DataDog/datadog-agent/pkg/trace/config"
	"github.com/DataDog/datadog-agent/pkg/trace/log"
	"github.com/DataDog/datadog-agent/pkg/trace/traceutil"
	"github.com/DataDog/datadog-agent/pkg/trace/watchdog"

	"github.com/DataDog/datadog-go/v5/statsd"
)

// defaultBufferLen represents the default buffer length; the number of bucket size
// units used by the concentrator.
const defaultBufferLen = 2

// Writer is an interface for something that can Write Stats Payloads
type Writer interface {
	// Write this payload
	Write(*pb.StatsPayload)
}

// Concentrator produces time bucketed statistics from a stream of raw traces.
// https://en.wikipedia.org/wiki/Knelson_concentrator
// Gets an imperial shitton of traces, and outputs pre-computed data structures
// allowing to find the gold (stats) amongst the traces.
type Concentrator struct {
	Writer Writer

	// bucket duration in nanoseconds
	bsize int64
	// Timestamp of the oldest time bucket for which we allow data.
	// Any ingested stats older than it get added to this bucket.
	oldestTs int64
	// bufferLen is the number of 10s stats bucket we keep in memory before flushing them.
	// It means that we can compute stats only for the last `bufferLen * bsize` and that we
	// wait such time before flushing the stats.
	// This only applies to past buckets. Stats buckets in the future are allowed with no restriction.
	bufferLen              int
	exit                   chan struct{}
	exitWG                 sync.WaitGroup
	buckets                map[int64]*RawBucket // buckets used to aggregate stats per timestamp
	mu                     sync.Mutex
	agentEnv               string
	agentHostname          string
	agentVersion           string
	peerTagsAggregation    bool     // flag to enable aggregation of peer tags
	computeStatsBySpanKind bool     // flag to enable computation of stats through checking the span.kind field
	peerTagKeys            []string // keys for supplementary tags that describe peer.service entities
	statsd                 statsd.ClientInterface
}

//go:embed peer_tags.ini
var peerTagFile []byte

var defaultPeerTags = func() []string {
	var tags []string = []string{"_dd.base_service"}

	cfg, err := ini.Load(peerTagFile)
	if err != nil {
		log.Error("Error loading file for peer tags: ", err)
		return tags
	}
	keys := cfg.Section("dd.apm.peer.tags").Keys()

	for _, key := range keys {
		value := strings.Split(key.Value(), ",")
		tags = append(tags, value...)
	}

	sort.Strings(tags)

	return tags
}()

func preparePeerTags(tags ...string) []string {
	if len(tags) == 0 {
		return nil
	}
	var deduped []string
	seen := make(map[string]struct{})
	for _, t := range tags {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			deduped = append(deduped, t)
		}
	}
	sort.Strings(deduped)
	return deduped
}

// NewConcentrator initializes a new concentrator ready to be started
func NewConcentrator(conf *config.AgentConfig, writer Writer, now time.Time, statsd statsd.ClientInterface) *Concentrator {
	bsize := conf.BucketInterval.Nanoseconds()
	c := Concentrator{
		bsize:   bsize,
		buckets: make(map[int64]*RawBucket),
		// At start, only allow stats for the current time bucket. Ensure we don't
		// override buckets which could have been sent before an Agent restart.
		oldestTs: alignTs(now.UnixNano(), bsize),
		// TODO: Move to configuration.
		bufferLen:              defaultBufferLen,
		Writer:                 writer,
		exit:                   make(chan struct{}),
		agentEnv:               conf.DefaultEnv,
		agentHostname:          conf.Hostname,
		agentVersion:           conf.AgentVersion,
		peerTagsAggregation:    conf.PeerServiceAggregation || conf.PeerTagsAggregation,
		computeStatsBySpanKind: conf.ComputeStatsBySpanKind,
		statsd:                 statsd,
	}
	// NOTE: maintain backwards-compatibility with old peer service flag that will eventually be deprecated.
	if conf.PeerServiceAggregation || conf.PeerTagsAggregation {
		c.peerTagKeys = preparePeerTags(append(defaultPeerTags, conf.PeerTags...)...)
	}
	return &c
}

// Start starts the concentrator.
func (c *Concentrator) Start() {
	c.exitWG.Add(1)
	go func() {
		defer watchdog.LogOnPanic(c.statsd)
		defer c.exitWG.Done()
		c.Run()
	}()
}

// Run runs the main loop of the concentrator goroutine. Traces are received
// through `Add`, this loop only deals with flushing.
func (c *Concentrator) Run() {
	// flush with the same period as stats buckets
	flushTicker := time.NewTicker(time.Duration(c.bsize) * time.Nanosecond)
	defer flushTicker.Stop()

	log.Debug("Starting concentrator")

	for {
		select {
		case <-flushTicker.C:
			c.Writer.Write(c.Flush(false))
		case <-c.exit:
			log.Info("Exiting concentrator, computing remaining stats")
			c.Writer.Write(c.Flush(true))
			return
		}
	}
}

// Stop stops the main Run loop.
func (c *Concentrator) Stop() {
	close(c.exit)
	c.exitWG.Wait()
}

// computeStatsForSpanKind returns true if the span.kind value makes the span eligible for stats computation.
func computeStatsForSpanKind(s *pb.Span) bool {
	k := strings.ToLower(s.Meta["span.kind"])
	switch k {
	case "server", "consumer", "client", "producer":
		return true
	default:
		return false
	}
}

// Input specifies a set of traces originating from a certain payload.
type Input struct {
	Traces        []traceutil.ProcessedTrace
	ContainerID   string
	ContainerTags []string
}

// NewStatsInput allocates a stats input for an incoming trace payload
func NewStatsInput(numChunks int, containerID string, clientComputedStats bool, conf *config.AgentConfig) Input {
	if clientComputedStats {
		return Input{}
	}
	in := Input{Traces: make([]traceutil.ProcessedTrace, 0, numChunks)}
	_, enabledCIDStats := conf.Features["enable_cid_stats"]
	_, disabledCIDStats := conf.Features["disable_cid_stats"]
	enableContainers := enabledCIDStats || (conf.FargateOrchestrator != config.OrchestratorUnknown)
	if enableContainers && !disabledCIDStats {
		// only allow the ContainerID stats dimension if we're in a Fargate instance or it's
		// been explicitly enabled and it's not prohibited by the disable_cid_stats feature flag.
		in.ContainerID = containerID
	}
	return in
}

// Add applies the given input to the concentrator.
func (c *Concentrator) Add(t Input) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, trace := range t.Traces {
		c.addNow(&trace, t.ContainerID, t.ContainerTags)
	}
}

// addNow adds the given input into the concentrator.
// Callers must guard!
func (c *Concentrator) addNow(pt *traceutil.ProcessedTrace, containerID string, containerTags []string) {
	hostname := pt.TracerHostname
	if hostname == "" {
		hostname = c.agentHostname
	}
	env := pt.TracerEnv
	if env == "" {
		env = c.agentEnv
	}
	weight := weight(pt.Root)
	aggKey := PayloadAggregationKey{
		Env:          env,
		Hostname:     hostname,
		Version:      pt.AppVersion,
		ContainerID:  containerID,
		GitCommitSha: pt.GitCommitSha,
		ImageTag:     pt.ImageTag,
	}
	for _, s := range pt.TraceChunk.Spans {
		isTop := traceutil.HasTopLevel(s)
		eligibleSpanKind := c.computeStatsBySpanKind && computeStatsForSpanKind(s)
		if !(isTop || traceutil.IsMeasured(s) || eligibleSpanKind) {
			continue
		}
		if traceutil.IsPartialSnapshot(s) {
			continue
		}
		end := s.Start + s.Duration
		btime := end - end%c.bsize

		// If too far in the past, count in the oldest-allowed time bucket instead.
		if btime < c.oldestTs {
			btime = c.oldestTs
		}

		b, ok := c.buckets[btime]
		if !ok {
			b = NewRawBucket(uint64(btime), uint64(c.bsize))
			if containerID != "" && len(containerTags) > 0 {
				b.containerTagsByID[containerID] = containerTags
			}
			c.buckets[btime] = b
		}
		b.HandleSpan(s, weight, isTop, pt.TraceChunk.Origin, aggKey, c.peerTagsAggregation, c.peerTagKeys)
	}
}

// Flush deletes and returns complete statistic buckets.
// The force boolean guarantees flushing all buckets if set to true.
func (c *Concentrator) Flush(force bool) *pb.StatsPayload {
	return c.flushNow(time.Now().UnixNano(), force)
}

func (c *Concentrator) flushNow(now int64, force bool) *pb.StatsPayload {
	m := make(map[PayloadAggregationKey][]*pb.ClientStatsBucket)
	containerTagsByID := make(map[string][]string)

	c.mu.Lock()
	for ts, srb := range c.buckets {
		// Always keep `bufferLen` buckets (default is 2: current + previous one).
		// This is a trade-off: we accept slightly late traces (clock skew and stuff)
		// but we delay flushing by at most `bufferLen` buckets.
		//
		// This delay might result in not flushing stats payload (data loss)
		// if the agent stops while the latest buckets aren't old enough to be flushed.
		// The "force" boolean skips the delay and flushes all buckets, typically on agent shutdown.
		if !force && ts > now-int64(c.bufferLen)*c.bsize {
			log.Tracef("Bucket %d is not old enough to be flushed, keeping it", ts)
			continue
		}
		log.Debugf("Flushing bucket %d", ts)
		for k, b := range srb.Export() {
			m[k] = append(m[k], b)
			if ctags, ok := srb.containerTagsByID[k.ContainerID]; ok {
				containerTagsByID[k.ContainerID] = ctags
			}
		}
		delete(c.buckets, ts)
	}
	// After flushing, update the oldest timestamp allowed to prevent having stats for
	// an already-flushed bucket.
	newOldestTs := alignTs(now, c.bsize) - int64(c.bufferLen-1)*c.bsize
	if newOldestTs > c.oldestTs {
		log.Debugf("Update oldestTs to %d", newOldestTs)
		c.oldestTs = newOldestTs
	}
	c.mu.Unlock()
	sb := make([]*pb.ClientStatsPayload, 0, len(m))
	for k, s := range m {
		p := &pb.ClientStatsPayload{
			Env:          k.Env,
			Hostname:     k.Hostname,
			ContainerID:  k.ContainerID,
			Version:      k.Version,
			GitCommitSha: k.GitCommitSha,
			ImageTag:     k.ImageTag,
			Stats:        s,
			Tags:         containerTagsByID[k.ContainerID],
		}
		sb = append(sb, p)
	}

	return &pb.StatsPayload{Stats: sb, AgentHostname: c.agentHostname, AgentEnv: c.agentEnv, AgentVersion: c.agentVersion}
}

// alignTs returns the provided timestamp truncated to the bucket size.
// It gives us the start time of the time bucket in which such timestamp falls.
func alignTs(ts int64, bsize int64) int64 {
	return ts - ts%bsize
}
