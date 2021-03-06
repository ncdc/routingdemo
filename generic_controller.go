/*
Copyright 2018 the Heptio Ark contributors.

Modifications copyright 2018 Heptio.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type genericController struct {
	name                 string
	queue                workqueue.RateLimitingInterface
	syncHandler          func(key string) error
	resyncFunc           func()
	resyncPeriod         time.Duration
	cacheSyncWaiters     []cache.InformerSynced
	additionalGoroutines []func(done <-chan struct{})
	startHandler         func()
	stopHandler          func()
}

func newGenericController(name string) *genericController {
	c := &genericController{
		name:  name,
		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), name),
	}

	return c
}

func (c *genericController) log(msg string, args ...interface{}) {
	log.Printf("["+c.name+"] "+msg+"\n", args...)
}

// Run is a blocking function that runs the specified number of worker goroutines
// to process items in the work queue. It will return when it receives on the
// ctx.Done() channel.
func (c *genericController) Run(ctx context.Context, numWorkers int) error {
	if c.syncHandler == nil {
		// programmer error
		panic("syncHandler is required")
	}

	var wg sync.WaitGroup

	defer func() {
		c.log("Waiting for workers to finish their work")

		c.queue.ShutDown()

		// We have to wait here in the deferred function instead of at the bottom of the function body
		// because we have to shut down the queue in order for the workers to shut down gracefully, and
		// we want to shut down the queue via defer and not at the end of the body.
		wg.Wait()

		c.log("All workers have finished")

	}()

	c.log("Starting controller")
	defer c.log("Shutting down controller")

	c.log("Waiting for caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(), c.cacheSyncWaiters...) {
		return errors.New("timed out waiting for caches to sync")
	}
	c.log("Caches are synced")

	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go func() {
			wait.Until(c.runWorker, time.Second, ctx.Done())
			wg.Done()
		}()
	}

	if c.resyncFunc != nil {
		if c.resyncPeriod == 0 {
			// Programmer error
			panic("non-zero resyncPeriod is required")
		}

		wg.Add(1)
		go func() {
			wait.Until(c.resyncFunc, c.resyncPeriod, ctx.Done())
			wg.Done()
		}()
	}

	for i := range c.additionalGoroutines {
		go c.additionalGoroutines[i](ctx.Done())
	}

	if c.startHandler != nil {
		c.startHandler()
	}

	<-ctx.Done()

	if c.stopHandler != nil {
		c.log("invoking stop handler")
		c.stopHandler()
		c.log("done invoking stop handler")
	}

	return nil
}

func (c *genericController) runWorker() {
	// continually take items off the queue (waits if it's
	// empty) until we get a shutdown signal from the queue
	for c.processNextWorkItem() {
	}
}

func (c *genericController) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// always call done on this item, since if it fails we'll add
	// it back with rate-limiting below
	defer c.queue.Done(key)

	err := c.syncHandler(key.(string))
	if err == nil {
		// If you had no error, tell the queue to stop tracking history for your key. This will reset
		// things like failure counts for per-item rate limiting.
		c.queue.Forget(key)
		return true
	}

	c.log("Error in syncHandler for %v, re-adding item to queue: %v", key, err)
	// we had an error processing the item so add it back
	// into the queue for re-processing with rate-limiting
	c.queue.AddRateLimited(key)

	return true
}

func (c *genericController) enqueue(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		c.log("Error creating queue key, item not added to queue: %v", err)
		return
	}

	c.queue.Add(key)
}
