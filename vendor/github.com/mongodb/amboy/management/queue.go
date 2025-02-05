package management

import (
	"context"
	"regexp"
	"time"

	"github.com/mongodb/amboy"
	"github.com/mongodb/grip"
	"github.com/pkg/errors"
)

type queueManager struct {
	queue amboy.Queue
}

// NewQueueManager returns a Manager implementation built on top of the
// amboy.Queue interface. This can be used to manage queues more generically.
//
// The management algorithms may impact performance of queues, as queues may
// require some locking to perform the underlying operations. The performance of
// these operations will degrade with the number of jobs that the queue
// contains, so best practice is to pass contexts with timeouts to all methods.
func NewQueueManager(q amboy.Queue) Manager {
	return &queueManager{
		queue: q,
	}
}

func (m *queueManager) JobStatus(ctx context.Context, f StatusFilter) (*JobStatusReport, error) {
	if err := f.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid filter")
	}

	counters := map[string]int{}
	for info := range m.queue.JobInfo(ctx) {
		if !m.matchesStatusFilter(info, f) {
			continue
		}
		counters[info.Type.Name]++
	}

	out := JobStatusReport{}

	for jt, num := range counters {
		out.Stats = append(out.Stats, JobCounters{
			ID:    jt,
			Count: num,
		})
	}

	out.Filter = f

	return &out, nil
}

func (m *queueManager) RecentTiming(ctx context.Context, window time.Duration, f RuntimeFilter) (*JobRuntimeReport, error) {
	var err error

	if err = f.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid filter")
	}

	if window <= time.Second {
		return nil, errors.New("must specify windows greater than one second")
	}

	counters := map[string][]time.Duration{}

	for info := range m.queue.JobInfo(ctx) {
		switch f {
		case Running:
			if !info.Status.InProgress {
				continue
			}
			counters[info.Type.Name] = append(counters[info.Type.Name], time.Since(info.Time.Start))
		case Latency:
			if info.Status.Completed {
				continue
			}
			if time.Since(info.Time.Created) > window {
				continue
			}
			counters[info.Type.Name] = append(counters[info.Type.Name], time.Since(info.Time.Created))
		case Duration:
			if !info.Status.Completed {
				continue
			}
			if time.Since(info.Time.End) > window {
				continue
			}
			counters[info.Type.Name] = append(counters[info.Type.Name], info.Time.End.Sub(info.Time.Start))
		default:
			return nil, errors.New("invalid job runtime filter")
		}
	}

	runtimes := []JobRuntimes{}

	for k, v := range counters {
		var total time.Duration
		for _, i := range v {
			total += i
		}

		runtimes = append(runtimes, JobRuntimes{
			ID:       k,
			Duration: total / time.Duration(len(v)),
		})
	}

	return &JobRuntimeReport{
		Filter: f,
		Period: window,
		Stats:  runtimes,
	}, nil
}

func (m *queueManager) JobIDsByState(ctx context.Context, jobType string, f StatusFilter) (*JobReportIDs, error) {
	var err error
	if err = f.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid filter")
	}

	uniqueIDs := map[string]struct{}{}
	for info := range m.queue.JobInfo(ctx) {
		if info.Type.Name != jobType {
			continue
		}

		if !m.matchesStatusFilter(info, f) {
			continue
		}

		uniqueIDs[info.ID] = struct{}{}
	}

	ids := make([]GroupedID, 0, len(uniqueIDs))
	for id := range uniqueIDs {
		ids = append(ids, GroupedID{ID: id})
	}

	return &JobReportIDs{
		Filter:     f,
		Type:       jobType,
		GroupedIDs: ids,
	}, nil
}

// matchesStatusFilter returns whether or not a job's information matches the
// given job status filter.
func (m *queueManager) matchesStatusFilter(info amboy.JobInfo, f StatusFilter) bool {
	switch f {
	case Pending:
		return !info.Status.InProgress && !info.Status.Completed
	case InProgress:
		return info.Status.InProgress
	case Stale:
		return info.Status.InProgress && time.Since(info.Status.ModificationTime) > m.queue.Info().LockTimeout
	case Completed:
		return info.Status.Completed
	case Retrying:
		return info.Status.Completed && info.Retry.NeedsRetry
	case All:
		return true
	default:
		return false
	}
}

func (m *queueManager) RecentErrors(ctx context.Context, window time.Duration, f ErrorFilter) (*JobErrorsReport, error) {
	var err error
	if err = f.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid filter")

	}
	if window <= time.Second {
		return nil, errors.New("must specify windows greater than one second")
	}

	collector := map[string]JobErrorsForType{}

	for info := range m.queue.JobInfo(ctx) {
		if !info.Status.Completed {
			continue
		}

		if info.Status.ErrorCount == 0 {
			continue
		}

		if time.Since(info.Time.End) > window {
			continue
		}

		switch f {
		case AllErrors, UniqueErrors:
			val := collector[info.Type.Name]
			val.Count++
			val.Total += info.Status.ErrorCount
			val.Errors = append(val.Errors, info.Status.Errors...)
			collector[info.Type.Name] = val
		case StatsOnly:
			val := collector[info.Type.Name]
			val.Count++
			val.Total += info.Status.ErrorCount
			collector[info.Type.Name] = val
		default:
			return nil, errors.New("operation is not supported")
		}
	}
	if f == UniqueErrors {
		for k, v := range collector {
			errs := map[string]struct{}{}

			for _, e := range v.Errors {
				errs[e] = struct{}{}
			}

			v.Errors = []string{}
			for e := range errs {
				v.Errors = append(v.Errors, e)
			}

			collector[k] = v
		}
	}

	var reports []JobErrorsForType

	for k, v := range collector {
		v.ID = k
		v.Average = float64(v.Total / v.Count)

		reports = append(reports, v)
	}

	return &JobErrorsReport{
		Period:         window,
		FilteredByType: false,
		Data:           reports,
	}, nil
}

func (m *queueManager) RecentJobErrors(ctx context.Context, jobType string, window time.Duration, f ErrorFilter) (*JobErrorsReport, error) {
	var err error
	if err = f.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid filter")

	}
	if window <= time.Second {
		return nil, errors.New("must specify windows greater than one second")
	}

	collector := map[string]JobErrorsForType{}

	for info := range m.queue.JobInfo(ctx) {
		if !info.Status.Completed || info.Status.ErrorCount == 0 {
			continue
		}
		if time.Since(info.Time.End) > window {
			continue
		}
		if info.Type.Name != jobType {
			continue
		}

		switch f {
		case AllErrors, UniqueErrors:
			val := collector[info.Type.Name]
			val.Count++
			val.Total += info.Status.ErrorCount
			val.Errors = append(val.Errors, info.Status.Errors...)
			collector[info.Type.Name] = val
		case StatsOnly:
			val := collector[info.Type.Name]
			val.Count++
			val.Total += info.Status.ErrorCount
			collector[info.Type.Name] = val
		default:
			return nil, errors.New("operation is not supported")
		}
	}
	if f == UniqueErrors {
		for k, v := range collector {
			errs := map[string]struct{}{}

			for _, e := range v.Errors {
				errs[e] = struct{}{}
			}

			v.Errors = []string{}
			for e := range errs {
				v.Errors = append(v.Errors, e)
			}

			collector[k] = v
		}
	}

	var reports []JobErrorsForType

	for k, v := range collector {
		v.ID = k
		v.Average = float64(v.Total / v.Count)

		reports = append(reports, v)
	}

	return &JobErrorsReport{
		Period:         window,
		FilteredByType: true,
		Data:           reports,
	}, nil
}

// getJob resolves a job's information into the job that supplied the
// information.
func (m *queueManager) getJob(ctx context.Context, info amboy.JobInfo) (amboy.Job, error) {
	if info.Retry.Retryable {
		var j amboy.Job
		var err error
		isRetryable := amboy.WithRetryableQueue(m.queue, func(rq amboy.RetryableQueue) {
			j, err = rq.GetAttempt(ctx, info.ID, info.Retry.CurrentAttempt)
		})

		// If the queue is retryable, return the result immediately. Otherwise,
		// if it's not a retryable queue, then a retryable job is treated no
		// differently from a non-retryable job (i.e. it's not retried).
		if isRetryable {
			return j, err
		}
	}

	j, ok := m.queue.Get(ctx, info.ID)
	if !ok {
		return j, errors.New("could not find job")
	}
	return j, nil

}

// CompleteJob marks a job complete by ID. The ID matches the logical job ID
// rather than the internally-stored job ID.
func (m *queueManager) CompleteJob(ctx context.Context, id string) error {
	j, ok := m.queue.Get(ctx, id)
	if !ok {
		return errors.Errorf("cannot recover job with ID '%s'", id)
	}

	return m.completeJob(ctx, j)
}

// CompleteJobsByType marks all jobs complete that match the status filter and
// job type.
func (m *queueManager) CompleteJobsByType(ctx context.Context, f StatusFilter, jobType string) error {
	if err := f.Validate(); err != nil {
		return errors.Wrap(err, "invalid filter")
	}

	catcher := grip.NewBasicCatcher()
	for info := range m.queue.JobInfo(ctx) {
		if info.Type.Name != jobType {
			continue
		}

		if !m.matchesStatusFilter(info, f) {
			continue
		}

		j, err := m.getJob(ctx, info)
		if err != nil {
			catcher.Wrapf(err, "getting job '%s' from info", info.ID)
			continue
		}

		catcher.Wrapf(m.completeJob(ctx, j), "marking job '%s' complete", j.ID())
	}

	return catcher.Resolve()
}

func (m *queueManager) completeJob(ctx context.Context, j amboy.Job) error {
	if err := m.queue.Complete(ctx, j); err != nil {
		return errors.Wrap(err, "completing job")
	}

	var err error
	amboy.WithRetryableQueue(m.queue, func(rq amboy.RetryableQueue) {
		err = rq.CompleteRetrying(ctx, j)
	})

	return errors.Wrap(err, "marking retryable job as complete")
}

// CompleteJobs marks all jobs complete that match the status filter.
func (m *queueManager) CompleteJobs(ctx context.Context, f StatusFilter) error {
	if err := f.Validate(); err != nil {
		return errors.Wrap(err, "invalid filter")
	}

	catcher := grip.NewBasicCatcher()
	for info := range m.queue.JobInfo(ctx) {
		if !m.matchesStatusFilter(info, f) {
			continue
		}

		j, err := m.getJob(ctx, info)
		if err != nil {
			catcher.Wrapf(err, "getting job '%s' from info", info.ID)
			continue
		}

		catcher.Wrapf(m.completeJob(ctx, j), "marking job '%s' complete", j.ID())
	}

	return catcher.Resolve()
}

// CompleteJobsByPattern marks all jobs complete that match the status filter
// and pattern. Patterns should be in Perl compatible regular expression syntax
// (https://golang.org/pkg/regexp) and match logical job IDs rather than
// internally-stored job IDs.
func (m *queueManager) CompleteJobsByPattern(ctx context.Context, f StatusFilter, pattern string) error {
	if err := f.Validate(); err != nil {
		return errors.Wrap(err, "invalid filter")
	}

	regex, err := regexp.Compile(pattern)
	if err != nil {
		return errors.Wrap(err, "invalid regexp")
	}

	catcher := grip.NewBasicCatcher()
	for info := range m.queue.JobInfo(ctx) {
		if !regex.MatchString(info.ID) {
			continue
		}

		if !m.matchesStatusFilter(info, f) {
			continue
		}

		j, err := m.getJob(ctx, info)
		if err != nil {
			catcher.Wrapf(err, "could not get job '%s' from info", info.ID)
			continue
		}

		catcher.Wrapf(m.completeJob(ctx, j), "marking job '%s' complete", j.ID())
	}

	return catcher.Resolve()
}
