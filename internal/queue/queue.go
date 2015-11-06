// Package queue implements our email queue.
// Accepted envelopes get put in the queue, and processed asynchronously.
package queue

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"blitiri.com.ar/go/chasquid/internal/courier"

	"github.com/golang/glog"
	"golang.org/x/net/trace"
)

const (
	// Maximum size of the queue; we reject emails when we hit this.
	maxQueueSize = 200

	// Give up sending attempts after this duration.
	giveUpAfter = 12 * time.Hour
)

var (
	queueFullError = fmt.Errorf("Queue size too big, try again later")
)

// Channel used to get random IDs for items in the queue.
var newID chan string

func generateNewIDs() {
	// IDs are base64(8 random bytes), but the code doesn't care.
	var err error
	buf := make([]byte, 8)
	id := ""
	for {
		_, err = rand.Read(buf)
		if err != nil {
			panic(err)
		}

		id = base64.RawURLEncoding.EncodeToString(buf)
		newID <- id
	}

}

func init() {
	newID = make(chan string, 4)
	go generateNewIDs()
}

// Queue that keeps mail waiting for delivery.
type Queue struct {
	// Items in the queue. Map of id -> Item.
	q map[string]*Item

	// Mutex protecting q.
	mu sync.RWMutex

	// Courier to use to deliver mail.
	courier courier.Courier
}

// TODO: Store the queue on disk.
// Load the queue and launch the sending loops on startup.
func New(c courier.Courier) *Queue {
	return &Queue{
		q:       map[string]*Item{},
		courier: c,
	}
}

func (q *Queue) Len() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.q)
}

// Put an envelope in the queue.
func (q *Queue) Put(from string, to []string, data []byte) (string, error) {
	if q.Len() >= maxQueueSize {
		return "", queueFullError
	}

	item := &Item{
		ID:      <-newID,
		From:    from,
		To:      to,
		Data:    data,
		Created: time.Now(),
		Results: map[string]error{},
	}
	q.mu.Lock()
	q.q[item.ID] = item
	q.mu.Unlock()

	glog.Infof("%s accepted from %q", item.ID, from)

	// Begin to send it right away.
	go item.SendLoop(q)

	return item.ID, nil
}

// Remove an item from the queue.
func (q *Queue) Remove(id string) {
	q.mu.Lock()
	delete(q.q, id)
	q.mu.Unlock()
}

// TODO: http handler for dumping the queue.
// Register it in main().

// An item in the queue.
// This must be easily serializable, so no pointers.
type Item struct {
	// Item ID. Uniquely identifies this item.
	ID string

	// The envelope for this item.
	From string
	To   []string
	Data []byte

	// Creation time.
	Created time.Time

	// Next attempt to send.
	NextAttempt time.Time

	// Map of recipient -> last result of sending it.
	Results map[string]error
}

func (item *Item) SendLoop(q *Queue) {
	defer q.Remove(item.ID)

	tr := trace.New("Queue", item.ID)
	defer tr.Finish()
	tr.LazyPrintf("from: %s", item.From)

	var err error
	for time.Since(item.Created) < giveUpAfter {
		// Send to all recipients that are still pending.
		successful := 0
		for _, to := range item.To {
			if err, ok := item.Results[to]; ok && err == nil {
				// Successful send for this recipient, nothing to do.
				successful++
				continue
			}

			tr.LazyPrintf("%s sending", to)

			// TODO: deliver, serially or in parallel with a waitgroup.
			err = q.courier.Deliver(item.From, to, item.Data)
			item.Results[to] = err
			if err != nil {
				tr.LazyPrintf("error: %v", err)
				glog.Infof("%s  -> %q fail: %v", item.ID, to, err)
			} else {
				successful++
				tr.LazyPrintf("%s successful", to)
				glog.Infof("%s  -> %q sent", item.ID, to)
			}
		}

		if successful == len(item.To) {
			// Successfully sent to all recipients.
			glog.Infof("%s all successful", item.ID)
			return
		}

		// TODO: Consider sending a non-final notification after 30m or so,
		// that some of the messages have been delayed.

		// TODO: Next attempt incremental wrt. previous one.
		// Do 3m, 5m, 10m, 15m, 40m, 60m, 2h, 5h, 12h, perturbed.
		// Put a table and function below, to change this easily.
		// We should track the duration of the previous one too? Or computed
		// based on created?
		time.Sleep(30 * time.Second)

	}

	// TODO: Send a notification message for the recipients we failed to send.
}