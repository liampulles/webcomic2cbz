package main

import (
	"sync"
)

type Job func()

func DoConcurrent(concurrency int, jobs []Job) {
	var wg sync.WaitGroup
	jobChan := make(chan Job)

	// Create concurrent workers
	for i := 0; i < concurrency; i++ {
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
