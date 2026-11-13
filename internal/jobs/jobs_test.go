package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/queue"
	"geoint/internal/testutil"
)

var bg = context.Background()

type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *queue.Client
	svc  *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testutil.DB(t)
	q := testutil.Queue(t)
	svc := New(pool, q)
	svc.ProgressInterval = 10 * time.Millisecond
	return &fixture{t: t, pool: pool, q: q, svc: svc}
}

// startWorker runs the worker until the test ends and returns a stop function
// that cancels it and waits for RunWorker to return.
func (f *fixture) startWorker() (stop func()) {
	f.t.Helper()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- f.svc.RunWorker(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					f.t.Errorf("RunWorker: %v", err)
				}
			case <-time.After(15 * time.Second):
				f.t.Error("RunWorker did not return after cancel")
			}
		})
	}
	f.t.Cleanup(stop)
	return stop
}

// await polls until the job is done or failed.
func (f *fixture) await(id uuid.UUID, user string) Job {
	f.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		j, err := f.svc.Get(bg, id, user, false)
		if err != nil {
			f.t.Fatal(err)
		}
		if j.Status == "done" || j.Status == "failed" {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatal("job did not finish")
	return Job{}
}

func (f *fixture) status(id uuid.UUID) Job {
	f.t.Helper()
	j, err := f.svc.Get(bg, id, "u1", true)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

func TestEnqueueRunAndGetResult(t *testing.T) {
	f := newFixture(t)
	f.svc.Register("echo", func(_ context.Context, p json.RawMessage, progress func(float64)) (any, error) {
		progress(0.5)
		return map[string]int{"n": 1}, nil
	})
	f.startWorker()
	id, err := f.svc.Enqueue(bg, "echo", map[string]string{"x": "y"}, "u1")
	if err != nil {
		t.Fatal(err)
	}
	j := f.await(id, "u1")
	if j.Status != "done" || j.Progress != 1 || string(j.Result) != `{"n": 1}` && string(j.Result) != `{"n":1}` {
		t.Fatalf("job = %+v result=%s", j, j.Result)
	}
	if j.Type != "echo" || j.CreatedBy != "u1" || j.FinishedAt == nil || j.Error != "" {
		t.Fatalf("job = %+v", j)
	}
	if !strings.Contains(string(j.Params), `"x"`) {
		t.Fatalf("params = %s", j.Params)
	}
}

func TestHandlerErrorMarksFailed(t *testing.T) {
	f := newFixture(t)
	f.svc.Register("bad", func(context.Context, json.RawMessage, func(float64)) (any, error) {
		return nil, errors.New("deu ruim")
	})
	f.startWorker()
	id, err := f.svc.Enqueue(bg, "bad", map[string]any{}, "u1")
	if err != nil {
		t.Fatal(err)
	}
	j := f.await(id, "u1")
	if j.Status != "failed" || j.Error != "deu ruim" || j.FinishedAt == nil || j.Result != nil {
		t.Fatalf("job = %+v", j)
	}
}

func TestGetByAnotherUserIsNotFound(t *testing.T) {
	f := newFixture(t)
	f.svc.Register("echo", func(context.Context, json.RawMessage, func(float64)) (any, error) { return 1, nil })
	id, err := f.svc.Enqueue(bg, "echo", map[string]any{}, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(bg, id, "u2", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user: %v", err)
	}
	if _, err := f.svc.Get(bg, uuid.New(), "u1", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if j, err := f.svc.Get(bg, id, "u2", true); err != nil || j.CreatedBy != "u1" {
		t.Fatalf("admin: %+v %v", j, err)
	}
	if j, err := f.svc.Get(bg, id, "u1", false); err != nil || j.Status != "queued" {
		t.Fatalf("owner: %+v %v", j, err)
	}
}

func TestUnknownTypeIsRejectedAtEnqueue(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Enqueue(bg, "nope", map[string]any{}, "u1"); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("err = %v", err)
	}
	var n int
	if err := f.pool.QueryRow(bg, `SELECT count(*) FROM job`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows = %d, %v", n, err)
	}
}

func TestEnqueueRejectsBadParams(t *testing.T) {
	f := newFixture(t)
	f.svc.Register("echo", func(context.Context, json.RawMessage, func(float64)) (any, error) { return 1, nil })
	big := map[string]string{"x": strings.Repeat("a", maxParamsSize)}
	for name, p := range map[string]any{
		"not an object": []int{1},
		"scalar":        "x",
		"null":          nil,
		"unmarshalable": make(chan int),
		"too big":       big,
		"invalid raw":   json.RawMessage(`{`),
	} {
		if _, err := f.svc.Enqueue(bg, "echo", p, "u1"); !errors.Is(err, ErrBadParams) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// A raw JSON object and a struct are fine.
	if _, err := f.svc.Enqueue(bg, "echo", json.RawMessage(`{"a":1}`), "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Enqueue(bg, "echo", struct{ A int }{1}, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Enqueue(bg, "echo", map[string]any{}, ""); err == nil {
		t.Fatal("empty user accepted")
	}
}

func TestTimeoutMarksFailed(t *testing.T) {
	f := newFixture(t)
	f.svc.Timeout = 100 * time.Millisecond
	cancelled := make(chan struct{})
	f.svc.Register("slow", func(ctx context.Context, _ json.RawMessage, _ func(float64)) (any, error) {
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	f.startWorker()
	id, err := f.svc.Enqueue(bg, "slow", map[string]any{}, "u1")
	if err != nil {
		t.Fatal(err)
	}
	j := f.await(id, "u1")
	if j.Status != "failed" || j.Error != "timeout" {
		t.Fatalf("job = %+v", j)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("handler context was not cancelled")
	}
}

func TestPanicMarksFailed(t *testing.T) {
	f := newFixture(t)
	f.svc.Register("boom", func(context.Context, json.RawMessage, func(float64)) (any, error) {
		panic("secret person name")
	})
	f.startWorker()
	id, err := f.svc.Enqueue(bg, "boom", map[string]any{}, "u1")
	if err != nil {
		t.Fatal(err)
	}
	j := f.await(id, "u1")
	if j.Status != "failed" || j.Error != "internal error" {
		t.Fatalf("job = %+v", j)
	}
}

func TestResultTooLarge(t *testing.T) {
	f := newFixture(t)
	f.svc.Register("huge", func(context.Context, json.RawMessage, func(float64)) (any, error) {
		return strings.Repeat("a", maxResultSize), nil
	})
	f.startWorker()
	id, err := f.svc.Enqueue(bg, "huge", map[string]any{}, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if j := f.await(id, "u1"); j.Status != "failed" || j.Error != "result too large" {
		t.Fatalf("job = %+v", j)
	}
}

func TestEnqueueFailsWhenPublishFails(t *testing.T) {
	f := newFixture(t)
	f.svc.Register("echo", func(context.Context, json.RawMessage, func(float64)) (any, error) { return 1, nil })
	f.q.Close()
	if _, err := f.svc.Enqueue(bg, "echo", map[string]any{}, "u1"); err == nil {
		t.Fatal("Enqueue succeeded without a queue")
	}
	var status, msg string
	if err := f.pool.QueryRow(bg, `SELECT status, error FROM job`).Scan(&status, &msg); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || msg != "could not enqueue" {
		t.Fatalf("orphan row: %q %q", status, msg)
	}
}

// enqueueUnstarted creates a job whose message nobody consumes.
func (f *fixture) enqueueUnstarted(typ string) uuid.UUID {
	f.t.Helper()
	id, err := f.svc.Enqueue(bg, typ, map[string]any{}, "u1")
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func TestRedeliveryOfFinishedJobDoesNotRerun(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	f.svc.Register("count", func(context.Context, json.RawMessage, func(float64)) (any, error) {
		calls.Add(1)
		return 1, nil
	})
	id := f.enqueueUnstarted("count")
	for i := 1; i <= 3; i++ {
		if err := f.svc.handleJob(bg, id, queue.Attempt{N: i}); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if calls.Load() != 1 || f.status(id).Status != "done" {
		t.Fatalf("calls = %d, job = %+v", calls.Load(), f.status(id))
	}
	// Failed jobs are not re-run either, nor are unknown ids an error.
	f.svc.Register("fail", func(context.Context, json.RawMessage, func(float64)) (any, error) {
		calls.Add(1)
		return nil, errors.New("x")
	})
	id = f.enqueueUnstarted("fail")
	for i := 1; i <= 2; i++ {
		if err := f.svc.handleJob(bg, id, queue.Attempt{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.handleJob(bg, uuid.New(), queue.Attempt{N: 1}); err != nil {
		t.Fatalf("unknown id: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func (f *fixture) setRunning(id uuid.UUID, age time.Duration) {
	f.t.Helper()
	if _, err := f.pool.Exec(bg, `UPDATE job SET status = 'running', progress = 0.4, run_id = gen_random_uuid(),
		updated_at = now() - make_interval(secs => $2) WHERE id = $1`, id, age.Seconds()); err != nil {
		f.t.Fatal(err)
	}
}

func TestStaleRunningJobIsReclaimedOnRedelivery(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	f.svc.Register("count", func(context.Context, json.RawMessage, func(float64)) (any, error) {
		calls.Add(1)
		return 1, nil
	})
	id := f.enqueueUnstarted("count")
	f.setRunning(id, time.Hour)
	if err := f.svc.handleJob(bg, id, queue.Attempt{N: 2}); err != nil {
		t.Fatal(err)
	}
	if j := f.status(id); calls.Load() != 1 || j.Status != "done" || j.Progress != 1 {
		t.Fatalf("calls = %d, job = %+v", calls.Load(), j)
	}
}

// countingHandler registers a "count" type and returns its call counter.
func (f *fixture) countingHandler() *atomic.Int32 {
	var calls atomic.Int32
	f.svc.Register("count", func(context.Context, json.RawMessage, func(float64)) (any, error) {
		calls.Add(1)
		return 1, nil
	})
	return &calls
}

// delivery runs handleJob in the background and returns its result channel.
func (f *fixture) delivery(ctx context.Context, id uuid.UUID, a queue.Attempt) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f.svc.handleJob(ctx, id, a) }()
	return ch
}

func (f *fixture) mustStillWait(ch <-chan error, d time.Duration) {
	f.t.Helper()
	select {
	case err := <-ch:
		f.t.Fatalf("handleJob returned (%v) while the job's heartbeat was live", err)
	case <-time.After(d):
	}
}

func (f *fixture) mustReturn(ch <-chan error) error {
	f.t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(15 * time.Second):
		f.t.Fatal("handleJob did not return")
		return nil
	}
}

func TestLiveHeartbeatOnLastDeliveryEndsInterruptedNotOrphaned(t *testing.T) {
	f := newFixture(t)
	f.svc.StaleAfter = 400 * time.Millisecond
	calls := f.countingHandler()
	id := f.enqueueUnstarted("count")
	f.setRunning(id, 0)
	ch := f.delivery(bg, id, queue.Attempt{N: 5, Last: true})
	f.mustStillWait(ch, 100*time.Millisecond) // live: waits, does not burn the last delivery
	if err := f.mustReturn(ch); err != nil {
		t.Fatal(err)
	}
	if j := f.status(id); calls.Load() != 0 || j.Status != "failed" || j.Error != "interrupted" {
		t.Fatalf("calls = %d, job = %+v", calls.Load(), j)
	}
}

func TestLiveHeartbeatThenOriginalRunFinishesAcksWithoutRunning(t *testing.T) {
	f := newFixture(t)
	f.svc.StaleAfter = time.Hour
	calls := f.countingHandler()
	id := f.enqueueUnstarted("count")
	f.setRunning(id, 0)
	ch := f.delivery(bg, id, queue.Attempt{N: 2})
	f.mustStillWait(ch, 100*time.Millisecond)
	if _, err := f.pool.Exec(bg, `UPDATE job SET status = 'done', progress = 1, result = '1', finished_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := f.mustReturn(ch); err != nil {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("handler ran for a job another run finished")
	}
}

func TestLiveHeartbeatThatGoesStaleIsReclaimed(t *testing.T) {
	f := newFixture(t)
	f.svc.StaleAfter = 300 * time.Millisecond
	calls := f.countingHandler()
	id := f.enqueueUnstarted("count")
	f.setRunning(id, 0)
	ch := f.delivery(bg, id, queue.Attempt{N: 2})
	f.mustStillWait(ch, 100*time.Millisecond)
	if err := f.mustReturn(ch); err != nil {
		t.Fatal(err)
	}
	if j := f.status(id); calls.Load() != 1 || j.Status != "done" {
		t.Fatalf("calls = %d, job = %+v", calls.Load(), j)
	}
}

func TestShutdownWhileWaitingOnLastDeliveryLeavesRowForTheSweeper(t *testing.T) {
	f := newFixture(t)
	f.svc.StaleAfter = time.Hour
	f.svc.SweepRunningAfter = 300 * time.Millisecond
	f.countingHandler()
	id := f.enqueueUnstarted("count")
	f.setRunning(id, 0)
	ctx, cancel := context.WithCancel(bg)
	ch := f.delivery(ctx, id, queue.Attempt{N: 5, Last: true})
	f.mustStillWait(ch, 50*time.Millisecond)
	cancel()
	if err := f.mustReturn(ch); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	// The message is gone for good (Nak on the last delivery), the row is not final...
	if j := f.status(id); j.Status != "running" {
		t.Fatalf("job = %+v", j)
	}
	f.sweepUntil(id, "interrupted")
}

func TestShutdownWhileRunningOnLastDeliveryLeavesRowForTheSweeper(t *testing.T) {
	f := newFixture(t)
	f.svc.SweepRunningAfter = 300 * time.Millisecond
	started := make(chan struct{})
	f.svc.Register("block", func(ctx context.Context, _ json.RawMessage, _ func(float64)) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	id := f.enqueueUnstarted("block")
	ctx, cancel := context.WithCancel(bg)
	ch := f.delivery(ctx, id, queue.Attempt{N: 5, Last: true})
	<-started
	cancel()
	if err := f.mustReturn(ch); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if j := f.status(id); j.Status != "running" {
		t.Fatalf("job = %+v", j)
	}
	f.sweepUntil(id, "interrupted")
}

// sweepUntil calls Sweep until the job is failed with want, which must happen
// once SweepRunningAfter has passed without a heartbeat; never earlier.
func (f *fixture) sweepUntil(id uuid.UUID, want string) {
	f.t.Helper()
	if i, e, err := f.svc.Sweep(bg); err != nil || i+e != 0 {
		f.t.Fatalf("early sweep: %d %d %v", i, e, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if _, _, err := f.svc.Sweep(bg); err != nil {
			f.t.Fatal(err)
		}
		if j := f.status(id); j.Status == "failed" {
			if j.Error != want || j.FinishedAt == nil {
				f.t.Fatalf("job = %+v", j)
			}
			return
		}
	}
	f.t.Fatalf("job never swept: %+v", f.status(id))
}

func TestSweepFailsStuckRunningAndExpiredQueuedOnly(t *testing.T) {
	f := newFixture(t)
	f.svc.SweepRunningAfter = time.Minute
	f.svc.SweepQueuedAfter = time.Hour
	f.countingHandler()
	stuck, fresh := f.enqueueUnstarted("count"), f.enqueueUnstarted("count")
	old, young := f.enqueueUnstarted("count"), f.enqueueUnstarted("count")
	done := f.enqueueUnstarted("count")
	f.setRunning(stuck, 2*time.Minute)
	f.setRunning(fresh, 10*time.Second)
	f.setRunning(done, 2*time.Minute)
	if _, err := f.pool.Exec(bg, `UPDATE job SET status = 'done', finished_at = now() WHERE id = $1`, done); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(bg, `UPDATE job SET created_at = now() - interval '2 hours' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	i, e, err := f.svc.Sweep(bg)
	if err != nil || i != 1 || e != 1 {
		t.Fatalf("Sweep = %d, %d, %v", i, e, err)
	}
	for id, want := range map[uuid.UUID][2]string{
		stuck: {"failed", "interrupted"}, old: {"failed", "expired"},
		fresh: {"running", ""}, young: {"queued", ""}, done: {"done", ""},
	} {
		if j := f.status(id); j.Status != want[0] || j.Error != want[1] {
			t.Errorf("job %v = %+v, want %v", id, j, want)
		}
	}
	// Idempotent.
	if i, e, err := f.svc.Sweep(bg); err != nil || i+e != 0 {
		t.Fatalf("second Sweep = %d, %d, %v", i, e, err)
	}
}

func TestSweptJobIsNotResurrectedByTheLateRunner(t *testing.T) {
	f := newFixture(t)
	f.svc.ProgressInterval = time.Hour
	f.svc.SweepRunningAfter = time.Minute
	started, release := make(chan struct{}), make(chan struct{})
	f.svc.Register("late", func(context.Context, json.RawMessage, func(float64)) (any, error) {
		close(started)
		<-release
		return "late result", nil
	})
	id := f.enqueueUnstarted("late")
	ch := f.delivery(bg, id, queue.Attempt{N: 1})
	<-started
	if _, err := f.pool.Exec(bg, `UPDATE job SET updated_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if i, _, err := f.svc.Sweep(bg); err != nil || i != 1 {
		t.Fatalf("Sweep = %d, %v", i, err)
	}
	close(release)
	if err := f.mustReturn(ch); err != nil {
		t.Fatal(err)
	}
	if j := f.status(id); j.Status != "failed" || j.Error != "interrupted" || j.Result != nil {
		t.Fatalf("late runner resurrected the job: %+v", j)
	}
	// A redelivered message for the swept job is acknowledged and skipped.
	if err := f.svc.handleJob(bg, id, queue.Attempt{N: 2}); err != nil {
		t.Fatal(err)
	}
}

func TestRunWorkerSweepsOnStartup(t *testing.T) {
	f := newFixture(t)
	f.svc.SweepInterval = time.Hour // only the sweep at startup can act
	f.svc.SweepRunningAfter = time.Minute
	f.countingHandler()
	// A row with no message at all, as when its message was lost.
	id := uuid.New()
	if _, err := f.pool.Exec(bg, `INSERT INTO job (id, type, params, status, created_by, run_id, updated_at)
		VALUES ($1, 'count', '{}', 'running', 'u1', gen_random_uuid(), now() - interval '1 hour')`, id); err != nil {
		t.Fatal(err)
	}
	f.startWorker()
	if j := f.await(id, "u1"); j.Status != "failed" || j.Error != "interrupted" {
		t.Fatalf("job = %+v", j)
	}
}

func TestStaleRunningJobOnLastDeliveryIsInterrupted(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	f.svc.Register("count", func(context.Context, json.RawMessage, func(float64)) (any, error) {
		calls.Add(1)
		return 1, nil
	})
	id := f.enqueueUnstarted("count")
	f.setRunning(id, time.Hour)
	if err := f.svc.handleJob(bg, id, queue.Attempt{N: 5, Last: true}); err != nil {
		t.Fatal(err)
	}
	if j := f.status(id); calls.Load() != 0 || j.Status != "failed" || j.Error != "interrupted" || j.FinishedAt == nil {
		t.Fatalf("calls = %d, job = %+v", calls.Load(), j)
	}
	// A job still queued on its last delivery simply runs.
	id = f.enqueueUnstarted("count")
	if err := f.svc.handleJob(bg, id, queue.Attempt{N: 5, Last: true}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || f.status(id).Status != "done" {
		t.Fatalf("calls = %d, job = %+v", calls.Load(), f.status(id))
	}
}

func TestShutdownLeavesRunningJobAndNextWorkerFinishesIt(t *testing.T) {
	f := newFixture(t)
	f.svc.StaleAfter = 150 * time.Millisecond
	var calls atomic.Int32
	started := make(chan struct{})
	f.svc.Register("block", func(ctx context.Context, _ json.RawMessage, _ func(float64)) (any, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return map[string]string{"by": "second"}, nil
	})
	stop := f.startWorker()
	id, err := f.svc.Enqueue(bg, "block", map[string]any{}, "u1")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("job did not start")
	}
	stop()
	if j := f.status(id); j.Status != "running" || j.Error != "" {
		t.Fatalf("after shutdown: %+v", j)
	}
	f.startWorker()
	j := f.await(id, "u1")
	if j.Status != "done" || calls.Load() != 2 || !strings.Contains(string(j.Result), "second") {
		t.Fatalf("calls = %d, job = %+v", calls.Load(), j)
	}
}

func TestFinishedJobIsAckedEvenIfShutdownIsRequested(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(bg)
	f.svc.Register("late", func(hctx context.Context, _ json.RawMessage, _ func(float64)) (any, error) {
		cancel() // shutdown requested...
		<-hctx.Done()
		return "ok", nil // ...but the handler still finishes successfully
	})
	id := f.enqueueUnstarted("late")
	if err := f.svc.handleJob(ctx, id, queue.Attempt{N: 1}); err != nil {
		t.Fatalf("a finished job was not acknowledged: %v", err)
	}
	if j := f.status(id); j.Status != "done" {
		t.Fatalf("job = %+v", j)
	}
}

func TestTimeoutHoldsWhenHandlerIgnoresContext(t *testing.T) {
	f := newFixture(t)
	f.svc.Timeout = 100 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.svc.Register("deaf", func(context.Context, json.RawMessage, func(float64)) (any, error) {
		<-release
		return "late", nil
	})
	id := f.enqueueUnstarted("deaf")
	if err := f.mustReturn(f.delivery(bg, id, queue.Attempt{N: 1})); err != nil {
		t.Fatal(err)
	}
	if j := f.status(id); j.Status != "failed" || j.Error != "timeout" {
		t.Fatalf("job = %+v", j)
	}
}

func TestRunnerThatLostOwnershipDoesNotOverwriteTheNewOwner(t *testing.T) {
	f := newFixture(t)
	f.svc.ProgressInterval = time.Hour // nobody refreshes a heartbeat
	f.svc.StaleAfter = time.Second
	var calls atomic.Int32
	startedA, startedB := make(chan struct{}), make(chan struct{})
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	f.svc.Register("two", func(_ context.Context, _ json.RawMessage, _ func(float64)) (any, error) {
		if calls.Add(1) == 1 {
			close(startedA)
			<-releaseA
			return "A", nil
		}
		close(startedB)
		<-releaseB
		return "B", nil
	})
	id := f.enqueueUnstarted("two")
	chA := f.delivery(bg, id, queue.Attempt{N: 1})
	<-startedA
	if _, err := f.pool.Exec(bg, `UPDATE job SET updated_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	chB := f.delivery(bg, id, queue.Attempt{N: 2}) // B takes over
	<-startedB
	close(releaseA) // A finishes while B still owns the running job
	if err := f.mustReturn(chA); err != nil {
		t.Fatal(err)
	}
	if j := f.status(id); j.Status != "running" || j.Result != nil {
		t.Fatalf("A wrote over B: %+v result=%s", j, j.Result)
	}
	close(releaseB)
	if err := f.mustReturn(chB); err != nil {
		t.Fatal(err)
	}
	if j := f.status(id); j.Status != "done" || string(j.Result) != `"B"` {
		t.Fatalf("job = %+v result=%s", j, j.Result)
	}
}

func TestRunnerThatLostOwnershipIsCancelled(t *testing.T) {
	f := newFixture(t)
	cancelled := make(chan struct{})
	started := make(chan struct{})
	f.svc.Register("owned", func(ctx context.Context, _ json.RawMessage, _ func(float64)) (any, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	id := f.enqueueUnstarted("owned")
	ch := f.delivery(bg, id, queue.Attempt{N: 1})
	<-started
	if _, err := f.pool.Exec(bg, `UPDATE job SET run_id = gen_random_uuid() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(15 * time.Second):
		t.Fatal("handler context was not cancelled after the job was taken over")
	}
	if err := f.mustReturn(ch); err != nil {
		t.Fatal(err)
	}
	if j := f.status(id); j.Status != "running" {
		t.Fatalf("job = %+v", j)
	}
}

func TestEnqueueRunsTheValidator(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	f.svc.RegisterValidated("strict", func(context.Context, json.RawMessage, func(float64)) (any, error) { return 1, nil },
		func(raw json.RawMessage) error {
			calls.Add(1)
			if !strings.Contains(string(raw), `"ok"`) {
				return fmt.Errorf("%w: needs ok", ErrBadParams)
			}
			return nil
		})
	if _, err := f.svc.Enqueue(bg, "strict", map[string]any{"no": 1}, "u1"); !errors.Is(err, ErrBadParams) || !strings.Contains(err.Error(), "needs ok") {
		t.Fatalf("err = %v", err)
	}
	var n int
	if err := f.pool.QueryRow(bg, `SELECT count(*) FROM job`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after rejection = %d, %v", n, err)
	}
	if _, err := f.svc.Enqueue(bg, "strict", map[string]any{"ok": 1}, "u1"); err != nil || calls.Load() != 2 {
		t.Fatalf("err = %v, validator calls = %d", err, calls.Load())
	}
}

func TestProgressInTheRowIsMonotonicAndAdvances(t *testing.T) {
	f := newFixture(t)
	step1, step2, step3 := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.svc.Register("steps", func(ctx context.Context, _ json.RawMessage, progress func(float64)) (any, error) {
		progress(0.3)
		<-step1
		progress(0.1) // lower: ignored
		progress(-5)  // clamped, still ignored
		<-step2
		progress(0.6)
		progress(40) // clamped to 1
		<-step3
		return 1, nil
	})
	id := f.enqueueUnstarted("steps")
	done := f.delivery(bg, id, queue.Attempt{N: 1})
	waitFor := func(want float64) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if p := f.status(id).Progress; p == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("progress is %v, want %v", f.status(id).Progress, want)
	}
	waitFor(0.3)
	close(step1)
	// Give several writes a chance (interval is 10 ms) to wrongly lower it.
	time.Sleep(60 * time.Millisecond)
	if p := f.status(id).Progress; p != 0.3 {
		t.Fatalf("progress went to %v after a lower report", p)
	}
	close(step2)
	waitFor(1)
	if j := f.status(id); j.Status != "running" {
		t.Fatalf("job = %+v", j)
	}
	close(step3)
	if err := f.mustReturn(done); err != nil {
		t.Fatal(err)
	}
}

func TestProgressWriterClamps(t *testing.T) {
	w := newProgressWriter(time.Hour, nil)
	for _, c := range []struct{ in, want float64 }{{0.3, 0.3}, {-1, 0.3}, {math.NaN(), 0.3}, {0.2, 0.3}, {7, 1}, {0.5, 1}} {
		w.report(c.in)
		if w.cur != c.want {
			t.Fatalf("after report(%v): %v, want %v", c.in, w.cur, c.want)
		}
	}
}

func TestProgressWriterThrottlesAndNeverDecreases(t *testing.T) {
	const interval, window = 20 * time.Millisecond, 200 * time.Millisecond
	var mu sync.Mutex
	var written []float64
	w := newProgressWriter(interval, func(_ context.Context, v float64) error {
		mu.Lock()
		defer mu.Unlock()
		written = append(written, v)
		return nil
	})
	ctx, cancel := context.WithCancel(bg)
	finished := make(chan struct{})
	go func() { w.run(ctx); close(finished) }()
	// Report a tight loop that rises to 0.8 in the first half of the window
	// and falls back to 0.2 in the second.
	start := time.Now()
	for {
		phase := float64(time.Since(start)) / float64(window)
		if phase >= 1 {
			break
		}
		w.report(0.2 + 0.6*(1-math.Abs(2*phase-1)))
	}
	cancel()
	<-finished
	mu.Lock()
	defer mu.Unlock()
	if n := len(written); n < 3 || n > int(window/interval)+2 {
		t.Fatalf("%d writes in %v at a %v interval: %v", n, window, interval, written)
	}
	distinct := map[float64]bool{}
	for i, v := range written {
		if v >= 1 || i > 0 && v < written[i-1] {
			t.Fatalf("writes not non-decreasing or reached 1: %v", written)
		}
		distinct[v] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("progress never advanced: %v", written)
	}
	if last := written[len(written)-1]; last < 0.5 {
		t.Fatalf("last write %v: the falling half of the reports lowered it? %v", last, written)
	}
}

func TestPurgeDeletesOnlyFinishedJobsPastRetention(t *testing.T) {
	f := newFixture(t)
	f.svc.RetainFor = 30 * 24 * time.Hour
	f.countingHandler()
	ids := map[string]uuid.UUID{}
	for _, name := range []string{"old done", "old failed", "young done", "old running", "old queued"} {
		ids[name] = f.enqueueUnstarted("count")
	}
	for name, set := range map[string]string{
		"old done":    `status = 'done', finished_at = now() - interval '31 days'`,
		"old failed":  `status = 'failed', error = 'timeout', finished_at = now() - interval '31 days'`,
		"young done":  `status = 'done', finished_at = now() - interval '29 days'`,
		"old running": `status = 'running', run_id = gen_random_uuid(), created_at = now() - interval '40 days'`,
		"old queued":  `created_at = now() - interval '40 days'`,
	} {
		if _, err := f.pool.Exec(bg, `UPDATE job SET `+set+` WHERE id = $1`, ids[name]); err != nil {
			t.Fatal(err)
		}
	}
	n, err := f.svc.Purge(bg)
	if err != nil || n != 2 {
		t.Fatalf("Purge = %d, %v; want 2", n, err)
	}
	for name, id := range ids {
		_, err := f.svc.Get(bg, id, "u1", true)
		gone := errors.Is(err, ErrNotFound)
		if want := strings.HasPrefix(name, "old done") || strings.HasPrefix(name, "old failed"); gone != want {
			t.Errorf("%s: deleted %v, want %v (err %v)", name, gone, want, err)
		}
	}
	if n, err := f.svc.Purge(bg); err != nil || n != 0 {
		t.Fatalf("second Purge = %d, %v", n, err)
	}
}

func TestPurgeIsBoundedPerRun(t *testing.T) {
	f := newFixture(t)
	f.svc.purgeBatch = 2
	f.countingHandler()
	for range 5 {
		id := f.enqueueUnstarted("count")
		if _, err := f.pool.Exec(bg, `UPDATE job SET status = 'done', finished_at = now() - interval '400 days' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []int64{2, 2, 1, 0} {
		if n, err := f.svc.Purge(bg); err != nil || n != want {
			t.Fatalf("Purge = %d, %v; want %d", n, err, want)
		}
	}
}

// A hand-built Service with no SweepInterval or RetainFor uses the defaults
// instead of panicking (time.NewTicker(0)) or purging every finished job.
func TestZeroTuningUsesDefaults(t *testing.T) {
	f := newFixture(t)
	f.svc.SweepInterval, f.svc.RetainFor = 0, 0
	f.countingHandler()
	recent := f.enqueueUnstarted("count")
	if _, err := f.pool.Exec(bg, `UPDATE job SET status = 'done', finished_at = now() - interval '1 day' WHERE id = $1`, recent); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 200*time.Millisecond)
	defer cancel()
	f.svc.sweepLoop(ctx) // returns when ctx ends; must not panic
	if _, err := f.svc.Get(bg, recent, "u1", true); err != nil {
		t.Fatalf("a job finished a day ago was purged: %v", err)
	}
}
