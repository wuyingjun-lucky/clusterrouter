package queue

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/clusterrouter-io/clusterrouter/pkg/utils/log"
	"github.com/clusterrouter-io/clusterrouter/pkg/utils/trace"
	pkgerrors "github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
)

const (
	// MaxRetries is the number of times we try to process a given key before permanently forgetting it.
	MaxRetries = 20
)

// ItemHandler is a callback that handles a single key on the Queue
type ItemHandler func(ctx context.Context, key string) error

// Queue implements a wrapper around workqueue with native VK instrumentation
type Queue struct {
	// clock is used for testing
	clock clock.Clock
	// lock protects running, and the items list / map
	lock    sync.Mutex
	running bool
	name    string
	handler ItemHandler

	ratelimiter workqueue.RateLimiter
	// items are items that are marked dirty waiting for processing.
	items *list.List
	// itemInQueue is a map of (string) key -> item while it is in the items list
	itemsInQueue map[string]*list.Element
	// itemsBeingProcessed is a map of (string) key -> item once it has been moved
	itemsBeingProcessed map[string]*queueItem
	// Wait for next semaphore is an exclusive (1 item) lock that is taken every time items is checked to see if there
	// is an item in queue for work
	waitForNextItemSemaphore *semaphore.Weighted

	// wakeup
	wakeupCh chan struct{}
}

type queueItem struct {
	key                    string
	plannedToStartWorkAt   time.Time
	redirtiedAt            time.Time
	redirtiedWithRatelimit bool
	forget                 bool
	requeues               int

	// Debugging information only
	originallyAdded     time.Time
	addedViaRedirty     bool
	delayedViaRateLimit *time.Duration
}

func (item *queueItem) String() string {
	return fmt.Sprintf("<plannedToStartWorkAt:%s key: %s>", item.plannedToStartWorkAt.String(), item.key)
}

// New creates a queue
//
// It expects to get a item rate limiter, and a friendly name which is used in logs, and
// in the internal kubernetes metrics.
func New(ratelimiter workqueue.RateLimiter, name string, handler ItemHandler) *Queue {
	return &Queue{
		clock:                    clock.RealClock{},
		name:                     name,
		ratelimiter:              ratelimiter,
		items:                    list.New(),
		itemsBeingProcessed:      make(map[string]*queueItem),
		itemsInQueue:             make(map[string]*list.Element),
		handler:                  handler,
		wakeupCh:                 make(chan struct{}, 1),
		waitForNextItemSemaphore: semaphore.NewWeighted(1),
	}
}

// Enqueue enqueues the key in a rate limited fashion
func (q *Queue) Enqueue(ctx context.Context, key string) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.insert(ctx, key, true, 0)
}

// EnqueueWithoutRateLimit enqueues the key without a rate limit
func (q *Queue) EnqueueWithoutRateLimit(ctx context.Context, key string) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.insert(ctx, key, false, 0)
}

// Forget forgets the key
func (q *Queue) Forget(ctx context.Context, key string) {
	q.lock.Lock()
	defer q.lock.Unlock()
	ctx, span := trace.StartSpan(ctx, "Forget")
	defer span.End()

	ctx = span.WithFields(ctx, map[string]interface{}{
		"queue": q.name,
		"key":   key,
	})

	if item, ok := q.itemsInQueue[key]; ok {
		span.WithField(ctx, "status", "itemInQueue")
		delete(q.itemsInQueue, key)
		q.items.Remove(item)
		return
	}

	if qi, ok := q.itemsBeingProcessed[key]; ok {
		span.WithField(ctx, "status", "itemBeingProcessed")
		qi.forget = true
		return
	}
	span.WithField(ctx, "status", "notfound")
}

// insert inserts a new item to be processed at time time. It will not further delay items if when is later than the
// original time the item was scheduled to be processed. If when is earlier, it will "bring it forward"
func (q *Queue) insert(ctx context.Context, key string, ratelimit bool, delay time.Duration) *queueItem {
	ctx, span := trace.StartSpan(ctx, "insert")
	defer span.End()

	ctx = span.WithFields(ctx, map[string]interface{}{
		"queue":     q.name,
		"key":       key,
		"ratelimit": ratelimit,
	})
	if delay > 0 {
		ctx = span.WithField(ctx, "delay", delay.String())
	}

	defer func() {
		select {
		case q.wakeupCh <- struct{}{}:
		default:
		}
	}()

	// First see if the item is already being processed
	if item, ok := q.itemsBeingProcessed[key]; ok {
		span.WithField(ctx, "status", "itemsBeingProcessed")
		when := q.clock.Now().Add(delay)
		// Is the item already been redirtied?
		if item.redirtiedAt.IsZero() {
			item.redirtiedAt = when
			item.redirtiedWithRatelimit = ratelimit
		} else if when.Before(item.redirtiedAt) {
			item.redirtiedAt = when
			item.redirtiedWithRatelimit = ratelimit
		}
		item.forget = false
		return item
	}

	// Is the item already in the queue?
	if item, ok := q.itemsInQueue[key]; ok {
		span.WithField(ctx, "status", "itemsInQueue")
		qi := item.Value.(*queueItem)
		when := q.clock.Now().Add(delay)
		q.adjustPosition(qi, item, when)
		return qi
	}

	span.WithField(ctx, "status", "added")
	now := q.clock.Now()
	val := &queueItem{
		key:                  key,
		plannedToStartWorkAt: now,
		originallyAdded:      now,
	}

	if ratelimit {
		if delay > 0 {
			panic("Non-zero delay with rate limiting not supported")
		}
		ratelimitDelay := q.ratelimiter.When(key)
		span.WithField(ctx, "delay", ratelimitDelay.String())
		val.plannedToStartWorkAt = val.plannedToStartWorkAt.Add(ratelimitDelay)
		val.delayedViaRateLimit = &ratelimitDelay
	} else {
		val.plannedToStartWorkAt = val.plannedToStartWorkAt.Add(delay)
	}

	for item := q.items.Back(); item != nil; item = item.Prev() {
		qi := item.Value.(*queueItem)
		if qi.plannedToStartWorkAt.Before(val.plannedToStartWorkAt) {
			q.itemsInQueue[key] = q.items.InsertAfter(val, item)
			return val
		}
	}

	q.itemsInQueue[key] = q.items.PushFront(val)
	return val
}

func (q *Queue) adjustPosition(qi *queueItem, element *list.Element, when time.Time) {
	if when.After(qi.plannedToStartWorkAt) {
		// The item has already been delayed appropriately
		return
	}

	qi.plannedToStartWorkAt = when
	for prev := element.Prev(); prev != nil; prev = prev.Prev() {
		item := prev.Value.(*queueItem)
		// does this item plan to start work *before* the new time? If so add it
		if item.plannedToStartWorkAt.Before(when) {
			q.items.MoveAfter(element, prev)
			return
		}
	}

	q.items.MoveToFront(element)
}

// EnqueueWithoutRateLimitWithDelay enqueues without rate limiting, but work will not start for this given delay period
func (q *Queue) EnqueueWithoutRateLimitWithDelay(ctx context.Context, key string, after time.Duration) {
	q.lock.Lock()
	defer q.lock.Unlock()
	q.insert(ctx, key, false, after)
}

// Empty returns if the queue has no items in it
//
// It should only be used for debugging.
func (q *Queue) Empty() bool {
	return q.Len() == 0
}

// Len includes items that are in the queue, and are being processed
func (q *Queue) Len() int {
	q.lock.Lock()
	defer q.lock.Unlock()
	if q.items.Len() != len(q.itemsInQueue) {
		panic("Internally inconsistent state")
	}

	return q.items.Len() + len(q.itemsBeingProcessed)
}

// Run starts the workers
//
// It blocks until context is cancelled, and all of the workers exit.
func (q *Queue) Run(ctx context.Context, workers int) {
	if workers <= 0 {
		panic(fmt.Sprintf("Workers must be greater than 0, got: %d", workers))
	}

	q.lock.Lock()
	if q.running {
		panic(fmt.Sprintf("Queue %s is already running", q.name))
	}
	q.running = true
	q.lock.Unlock()
	defer func() {
		q.lock.Lock()
		defer q.lock.Unlock()
		q.running = false
	}()

	// Make sure all workers are stopped before we finish up.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	group := &wait.Group{}
	for i := 0; i < workers; i++ {
		// This is required because i is referencing a mutable variable and that's running in a separate goroutine
		idx := i
		group.StartWithContext(ctx, func(ctx context.Context) {
			q.worker(ctx, idx)
		})
	}
	defer group.Wait()
	<-ctx.Done()
}

func (q *Queue) worker(ctx context.Context, i int) {
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(map[string]interface{}{
		"workerId": i,
		"queue":    q.name,
	}))
	for q.handleQueueItem(ctx) {
	}
}

func (q *Queue) getNextItem(ctx context.Context) (*queueItem, error) {
	if err := q.waitForNextItemSemaphore.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer q.waitForNextItemSemaphore.Release(1)

	for {
		q.lock.Lock()
		element := q.items.Front()
		if element == nil {
			// Wait for the next item
			q.lock.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-q.wakeupCh:
			}
		} else {
			qi := element.Value.(*queueItem)
			timeUntilProcessing := time.Until(qi.plannedToStartWorkAt)

			// Do we need to sleep? If not, let's party.
			if timeUntilProcessing <= 0 {
				q.itemsBeingProcessed[qi.key] = qi
				q.items.Remove(element)
				delete(q.itemsInQueue, qi.key)
				q.lock.Unlock()
				return qi, nil
			}

			q.lock.Unlock()
			if err := func() error {
				timer := q.clock.NewTimer(timeUntilProcessing)
				defer timer.Stop()
				select {
				case <-timer.C():
				case <-ctx.Done():
					return ctx.Err()
				case <-q.wakeupCh:
				}
				return nil
			}(); err != nil {
				return nil, err
			}
		}
	}
}

// handleQueueItem handles a single item
//
// A return value of "false" indicates that further processing should be stopped.
func (q *Queue) handleQueueItem(ctx context.Context) bool {
	ctx, span := trace.StartSpan(ctx, "handleQueueItem")
	defer span.End()

	qi, err := q.getNextItem(ctx)
	if err != nil {
		span.SetStatus(err)
		return false
	}

	// We expect strings to come off the work Queue.
	// These are of the form namespace/name.
	// We do this as the delayed nature of the work Queue means the items in the informer cache may actually be more u
	// to date that when the item was initially put onto the workqueue.
	ctx = span.WithField(ctx, "key", qi.key)
	log.G(ctx).Debug("Got Queue object")

	err = q.handleQueueItemObject(ctx, qi)
	if err != nil {
		// We've actually hit an error, so we set the span's status based on the error.
		span.SetStatus(err)
		log.G(ctx).WithError(err).Error("Error processing Queue item")
		return true
	}
	log.G(ctx).Debug("Processed Queue item")

	return true
}

func (q *Queue) handleQueueItemObject(ctx context.Context, qi *queueItem) error {
	// This is a separate function / span, because the handleQueueItem span is the time spent waiting for the object
	// plus the time spend handling the object. Instead, this function / span is scoped to a single object.
	ctx, span := trace.StartSpan(ctx, "handleQueueItemObject")
	defer span.End()

	ctx = span.WithFields(ctx, map[string]interface{}{
		"requeues":        qi.requeues,
		"originallyAdded": qi.originallyAdded.String(),
		"addedViaRedirty": qi.addedViaRedirty,
		"plannedForWork":  qi.plannedToStartWorkAt.String(),
	})

	if qi.delayedViaRateLimit != nil {
		ctx = span.WithField(ctx, "delayedViaRateLimit", qi.delayedViaRateLimit.String())
	}

	// Add the current key as an attribute to the current span.
	ctx = span.WithField(ctx, "key", qi.key)
	// Run the syncHandler, passing it the namespace/name string of the Pod resource to be synced.
	err := q.handler(ctx, qi.key)

	q.lock.Lock()
	defer q.lock.Unlock()

	delete(q.itemsBeingProcessed, qi.key)
	if qi.forget {
		q.ratelimiter.Forget(qi.key)
		log.G(ctx).WithError(err).Warnf("forgetting %q as told to forget while in progress", qi.key)
		return nil
	}

	if err != nil {
		if qi.requeues+1 < MaxRetries {
			// Put the item back on the work Queue to handle any transient errors.
			log.G(ctx).WithError(err).Warnf("requeuing %q due to failed sync", qi.key)
			newQI := q.insert(ctx, qi.key, true, 0)
			newQI.requeues = qi.requeues + 1
			newQI.originallyAdded = qi.originallyAdded

			return nil
		}
		err = pkgerrors.Wrapf(err, "forgetting %q due to maximum retries reached", qi.key)
	}

	// We've exceeded the maximum retries or we were successful.
	q.ratelimiter.Forget(qi.key)
	if !qi.redirtiedAt.IsZero() {
		newQI := q.insert(ctx, qi.key, qi.redirtiedWithRatelimit, time.Until(qi.redirtiedAt))
		newQI.addedViaRedirty = true
	}

	return err
}

func (q *Queue) String() string {
	q.lock.Lock()
	defer q.lock.Unlock()

	items := make([]string, 0, q.items.Len())

	for next := q.items.Front(); next != nil; next = next.Next() {
		items = append(items, next.Value.(*queueItem).String())
	}
	return fmt.Sprintf("<items:%s>", items)
}
