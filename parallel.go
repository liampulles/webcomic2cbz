package main

import (
	"errors"
	"sync"
)

type Job func()

// Do a set of jobs concurrently. They will be picked up in
// order but are not guaranteed to complete in order.
func DoConcurrent(concurrency int, jobs []Job) {
	var wg sync.WaitGroup
	jobChan := make(chan Job)

	// Create concurrent workers
	for range concurrency {
		wg.Go(func() {
			for j := range jobChan {
				j()
			}
		})
	}

	// Fill the jobs channel
	for _, job := range jobs {
		jobChan <- job
	}
	close(jobChan)

	// Wait for currently runnings jobs to finish
	wg.Wait()
}

// Process a set of input via a func that produces output and may fail, concurrently.
//
// The output slice is NOT guaranteed to match input order and NOT guaranteed to match
// the length of input (in particular, if some jobs have errors).
//
// An error is returned with errors for all the jobs that failed, however job fails do
// not stop other jobs from processing.
func DoConcurrentProcess[I, O any](concurrency int, input []I, fn func(I) (O, error)) ([]O, error) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	inputChan := make(chan I)

	// Create concurrent workers
	allOut := make([]O, 0, len(input))
	var allErr error
	for range concurrency {
		wg.Go(func() {
			for in := range inputChan {
				out, err := fn(in)
				mu.Lock()
				if err == nil {
					allOut = append(allOut, out)
				}
				allErr = errors.Join(allErr, err)
				mu.Unlock()
			}
		})
	}

	// Fill the input channel
	for _, in := range input {
		inputChan <- in
	}
	close(inputChan)

	// Wait for currently runnings jobs to finish
	wg.Wait()

	return allOut, allErr
}
