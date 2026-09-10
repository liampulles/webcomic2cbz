package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"
)

func main() {
	// Define main queue
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cancel := DefineQueue(ctx, "main", 1)

	// Enqueue the main queue
	Enqueue("main", mainQueue)
	cancel()

	// Wait for all queues
	QueuesWait()
}

func mainQueue(ctx context.Context) {
	fmt.Println("Hello world!")
}
