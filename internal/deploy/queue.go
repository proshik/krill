package deploy

import "sync"

// queueCapacity is how many deploys may wait for the single worker at once.
const queueCapacity = 64

// jobQueue is the deploy queue: first in, first out and bounded, like the
// channel it replaced, but a stop can take the jobs it cancelled out of the
// middle in one step that the worker's dequeue and the shutdown drain cannot
// interleave with — a channel cannot be filtered without emptying it, and
// while emptied the worker could run a later job first or the drain could
// miss the jobs held aside.
type jobQueue struct {
	mu   sync.Mutex
	jobs []job
	max  int
	wake chan struct{} // capacity 1: a job was added since the worker last looked
}

func newJobQueue(max int) *jobQueue {
	return &jobQueue{max: max, wake: make(chan struct{}, 1)}
}

// push adds a job at the end; false when the queue is full.
func (q *jobQueue) push(j job) bool {
	q.mu.Lock()
	if len(q.jobs) >= q.max {
		q.mu.Unlock()
		return false
	}
	q.jobs = append(q.jobs, j)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// pop takes the first job, if any.
func (q *jobQueue) pop() (job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.jobs) == 0 {
		return job{}, false
	}
	j := q.jobs[0]
	q.jobs[0] = job{}
	q.jobs = q.jobs[1:]
	return j, true
}

// takeAll empties the queue and returns what it held, in order.
func (q *jobQueue) takeAll() []job {
	q.mu.Lock()
	defer q.mu.Unlock()
	jobs := q.jobs
	q.jobs = nil
	return jobs
}

// remove drops the jobs drop reports true for, keeping the others in order.
func (q *jobQueue) remove(drop func(job) bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.jobs[:0]
	for _, j := range q.jobs {
		if !drop(j) {
			kept = append(kept, j)
		}
	}
	clear(q.jobs[len(kept):])
	q.jobs = kept
}

func (q *jobQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

func (q *jobQueue) full() bool { return q.len() >= q.max }
