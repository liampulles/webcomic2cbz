package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
)

type Job func(ctx context.Context)
type QueueID string

type QueueDefinition struct {
	ID          QueueID
	Concurrency int
}

var queueWg sync.WaitGroup
var queueMu sync.RWMutex
var queues map[QueueID]chan Job = make(map[QueueID]chan Job)

// Define one or more queue, which can then be used with enqueue.
//
// Accepts a context and returns a context cancel func. The caller
// should call cancel when no more jobs should be enqueued or the
// app is closing abruptly.
//
// Calling the same qid again will panic. You should ensure to
// build unique QIDs if needed, or build them higher up in the chain
// for use later.
func DefineQueues(ctx context.Context, defs ...QueueDefinition) context.CancelFunc {
	jobCtx, cancel := context.WithCancel(ctx)

	for _, def := range defs {
		// Built it already?
		queueMu.RLock()
		_, exists := queues[def.ID]
		queueMu.RUnlock()
		if exists {
			panic(fmt.Sprintf("queue already defined: %s", def.ID))
		}

		// Setup queue
		jobs := make(chan Job)

		// Spin up workers
		for range def.Concurrency {
			worker(jobCtx, def.ID, jobs)
		}

		// Add queue to map, for enqueueing.
		queueMu.Lock()
		queues[def.ID] = jobs
		queueMu.Unlock()
	}

	return cancel
}

// Convenience func if you just want to make 1 queue.
func DefineQueue(ctx context.Context, id QueueID, concurrency int) context.CancelFunc {
	return DefineQueues(ctx, QueueDefinition{id, concurrency})
}

// Enqueue a job on a queue. If the queue has not been defined with
// DefineQueue, this panics.
func Enqueue(qid QueueID, job Job) {
	queueMu.RLock()
	queue, ok := queues[qid]
	queueMu.RUnlock()
	if !ok {
		panic(fmt.Sprintf("must define queue before enqueueing: %s", qid))
	}

	queue <- job
}

// Wait until all queues are finished. This will only happen once
// all their contexts are closed.
func QueuesWait() {
	queueWg.Wait()
}

func worker(ctx context.Context, qid QueueID, jobs <-chan Job) {
	queueWg.Go(func() {
		for {
			select {
			case j := <-jobs:
				j(ctx)
			case <-ctx.Done():
				log.Debug().
					Str("queue_id", string(qid)).
					Msg("context closed, queue worker stopping")
				return
			}
		}
	})
}
