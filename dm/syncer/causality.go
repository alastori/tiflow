// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package syncer

import (
	"math"
	"time"

	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tiflow/dm/pkg/log"
	"github.com/pingcap/tiflow/dm/syncer/metrics"
	"go.uber.org/zap"
)

// causality provides a simple mechanism to ensure correctness when we are running
// DMLs concurrently.
// As a table might have one or multiple keys (including PK and UKs), row changes
// might depend on other row changes, together they form a dependency graph, only
// row changes without dependency can run concurrently.
// currently, row changes for a table from upstream are dispatched to DML workers
// by their keys, to make sure row changes with same keys are dispatched to the
// same worker, but this cannot handle dependencies cross row changes with
// different keys.
// suppose we have a table `t(a int unique, b int unique)`, and following row changes:
//   - insert t(a=1, b=1), we put to worker 1
//   - insert t(a=2, b=2), we put to worker 2
//   - delete t(a=2, b=2), we put to worker 2
//   - update t set b=2 where a=1, this row change depends on all above row changes,
//     we must at least wait all row changes related to (a=2, b=2) finish before
//     dispatch it to worker 1, else data inconsistency might happen.
//
// causality is used to detect this kind of dependencies, and it will generate a
// conflict job to wait all DMLs in DML workers are executed before we can continue
// dispatching.
// this mechanism meets quiescent consistency to ensure correctness.
type causality struct {
	relation    *causalityRelation
	outCh       chan *job
	inCh        chan *job
	logger      log.Logger
	sessCtx     sessionctx.Context
	workerCount int

	// for MetricsProxies
	task          string
	source        string
	metricProxies *metrics.Proxies
}

// causalityWrap creates and runs a causality instance.
func causalityWrap(inCh chan *job, syncer *Syncer) chan *job {
	causality := &causality{
		relation:      newCausalityRelation(),
		task:          syncer.cfg.Name,
		source:        syncer.cfg.SourceID,
		metricProxies: syncer.metricsProxies,
		logger:        syncer.tctx.Logger.WithFields(zap.String("component", "causality")),
		inCh:          inCh,
		outCh:         make(chan *job, syncer.cfg.QueueSize),
		sessCtx:       syncer.sessCtx,
		workerCount:   syncer.cfg.WorkerCount,
	}

	go func() {
		causality.run()
		causality.close()
	}()

	return causality.outCh
}

// run receives dml jobs and send causality jobs by adding causality key.
// When meet conflict, sends a conflict job.
func (c *causality) run() {
	for j := range c.inCh {
		c.metricProxies.QueueSizeGauge.WithLabelValues(c.task, "causality_input", c.source).Set(float64(len(c.inCh)))

		startTime := time.Now()

		switch j.tp {
		case flush, asyncFlush:
			c.relation.rotate(j.flushSeq)
		case gc:
			// gc is only used on inner-causality logic
			c.relation.gc(j.flushSeq)
			continue
		default:
			keys := j.dml.CausalityKeys()

			// detectConflict before add
			if c.detectConflict(keys) {
				c.logger.Debug("meet causality key, will generate a conflict job to flush all sqls", zap.Strings("keys", keys))
				c.outCh <- newConflictJob(c.workerCount)
				c.relation.clear()
			}
			j.dmlQueueKey = c.add(keys)
			c.logger.Debug("key for keys", zap.String("key", j.dmlQueueKey), zap.Strings("keys", keys))
		}
		c.metricProxies.Metrics.ConflictDetectDurationHistogram.Observe(time.Since(startTime).Seconds())

		c.outCh <- j
	}
}

// close closes outer channel.
func (c *causality) close() {
	close(c.outCh)
}

// add adds keys relation and return the relation. The keys must `detectConflict` first to ensure correctness.
func (c *causality) add(keys []string) string {
	if len(keys) == 0 {
		return ""
	}

	// find causal key
	selectedRelation := keys[0]
	var nonExistKeys []string
	for _, key := range keys {
		if val, ok := c.relation.get(key); ok {
			selectedRelation = val
		} else {
			nonExistKeys = append(nonExistKeys, key)
		}
	}
	// set causal relations for those non-exist keys
	for _, key := range nonExistKeys {
		c.relation.set(key, selectedRelation)
	}

	return selectedRelation
}

// detectConflict detects whether there is a conflict.
func (c *causality) detectConflict(keys []string) bool {
	if len(keys) == 0 {
		return false
	}

	var existedRelation string
	for _, key := range keys {
		if val, ok := c.relation.get(key); ok {
			if existedRelation != "" && val != existedRelation {
				return true
			}
			existedRelation = val
		}
	}

	return false
}

// dmlJobKeyRelationGroup stores a group of dml job key relations as data, and a flush job seq representing last flush job before adding any job keys.
type dmlJobKeyRelationGroup struct {
	data            map[string]string
	prevFlushJobSeq int64
}

// causalityRelation stores causality keys by group, where each group created on each flush and it helps to remove stale causality keys.
type causalityRelation struct {
	groups []*dmlJobKeyRelationGroup
}

func newCausalityRelation() *causalityRelation {
	m := &causalityRelation{}
	m.rotate(-1)
	return m
}

func (m *causalityRelation) get(key string) (string, bool) {
	for i := len(m.groups) - 1; i >= 0; i-- {
		if v, ok := m.groups[i].data[key]; ok {
			return v, true
		}
	}
	return "", false
}

func (m *causalityRelation) set(key string, val string) {
	m.groups[len(m.groups)-1].data[key] = val
}

func (m *causalityRelation) len() int {
	cnt := 0
	for _, d := range m.groups {
		cnt += len(d.data)
	}
	return cnt
}

func (m *causalityRelation) rotate(flushJobSeq int64) {
	m.groups = append(m.groups, &dmlJobKeyRelationGroup{
		data:            make(map[string]string),
		prevFlushJobSeq: flushJobSeq,
	})
}

func (m *causalityRelation) clear() {
	m.gc(math.MaxInt64)
}

// remove group of keys where its group's prevFlushJobSeq is smaller than or equal with the given flushJobSeq.
func (m *causalityRelation) gc(flushJobSeq int64) {
	if flushJobSeq == math.MaxInt64 {
		m.groups = m.groups[:0]
		m.rotate(-1)
		return
	}

	idx := 0
	for i, d := range m.groups {
		if d.prevFlushJobSeq <= flushJobSeq {
			idx = i
		} else {
			break
		}
	}

	m.groups = m.groups[idx:]
}
