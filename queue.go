package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
)

type Job func(ctx context.Context)
type QueueID string

var queueWg sync.WaitGroup
var queues map[QueueID]chan Job = make(map[QueueID]chan Job)

// Define a queue, which can then be used with enqueue. Accepts a context
// and returns a context cancel func. The caller should call cancel when
// no more jobs should be enqueued or the app is closing abruptly.
func DefineQueue(ctx context.Context, id QueueID, concurrency int) context.CancelFunc {
	jobCtx, cancel := context.WithCancel(ctx)

	// Setup queue
	jobs := make(chan Job)
	queues[id] = jobs

	// Spin up workers
	for range concurrency {
		worker(jobCtx, id, jobs)
	}

	return cancel
}

// Enqueue a job on a queue. If the queue has not been defined with
// DefineQueue, this panics.
func Enqueue(queueID QueueID, job Job) {
	queue, ok := queues[queueID]
	if !ok {
		panic(fmt.Sprintf("must define queue before enqueueing: %s", queueID))
	}

	queue <- job
}

// Wait until all queues are finished. This will only happen once
// all their contexts are closed.
func QueuesWait() {
	queueWg.Wait()
}

func worker(ctx context.Context, queueID QueueID, jobs <-chan Job) {
	queueWg.Go(func() {
		for {
			select {
			case j := <-jobs:
				j(ctx)
			case <-ctx.Done():
				log.Debug().
					Str("queue_id", string(queueID)).
					Msg("context closed, queue stopping")
				return
			}
		}
	})
}
