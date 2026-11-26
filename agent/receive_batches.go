package agent

import (
	"sync"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

const receiveBatchIdle = protocol.DirectShareTimeout + 2*time.Minute

type receiveBatchKey struct{ owner, id string }

type receiveBatch struct {
	sender string
	files  []desktop.ReceivedFile
	timer  *time.Timer
	last   time.Time
}

// A picker operation ends explicitly. The idle timer still notifies about files
// already saved if the phone goes offline or abandons the rest of the batch.
type receiveBatchQueue struct {
	mu      sync.Mutex
	pending map[receiveBatchKey]*receiveBatch
}

func (q *receiveBatchQueue) record(key receiveBatchKey, sender string, file desktop.ReceivedFile, idle time.Duration, notify func(string, []desktop.ReceivedFile)) {
	if key.id == "" {
		notify(sender, []desktop.ReceivedFile{file})
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending == nil {
		q.pending = make(map[receiveBatchKey]*receiveBatch)
	}
	batch := q.pending[key]
	if batch == nil {
		if len(q.pending) >= 128 {
			var oldest receiveBatchKey
			var earliest time.Time
			for candidate, entry := range q.pending {
				if earliest.IsZero() || entry.last.Before(earliest) {
					oldest, earliest = candidate, entry.last
				}
			}
			q.flushLocked(oldest, notify)
		}
		batch = &receiveBatch{sender: sender}
		q.pending[key] = batch
	}
	batch.files = append(batch.files, file)
	if len(batch.files) == 64 {
		q.flushLocked(key, notify)
	} else {
		q.arm(key, batch, idle, notify)
	}
}

func (q *receiveBatchQueue) touch(key receiveBatchKey, idle time.Duration, notify func(string, []desktop.ReceivedFile)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if batch := q.pending[key]; batch != nil {
		q.arm(key, batch, idle, notify)
	}
}

func (q *receiveBatchQueue) arm(key receiveBatchKey, batch *receiveBatch, idle time.Duration, notify func(string, []desktop.ReceivedFile)) {
	if batch.timer != nil {
		batch.timer.Stop()
	}
	batch.last = time.Now()
	timer := time.AfterFunc(idle, func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.pending[key] == batch && batch.last.Add(idle).Compare(time.Now()) <= 0 {
			q.flushLocked(key, notify)
		}
	})
	batch.timer = timer
}

func (q *receiveBatchQueue) flush(key receiveBatchKey, notify func(string, []desktop.ReceivedFile)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.flushLocked(key, notify)
}

func (q *receiveBatchQueue) flushLocked(key receiveBatchKey, notify func(string, []desktop.ReceivedFile)) {
	if batch := q.pending[key]; batch != nil {
		delete(q.pending, key)
		if batch.timer != nil {
			batch.timer.Stop()
		}
		notify(batch.sender, batch.files)
	}
}
