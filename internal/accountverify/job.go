// 批量验证任务：账号多了必须后台跑（一次几十上百个号 = 几十秒），并且要能看进度、能停。
//
// 职责边界：任务只负责"跑探测 + 记账 + 报进度"，**不碰账号池**——
// 写回由调用方通过 onResult / onDone 回调完成（怎么落盘、要不要落盘由上层决定）。
package accountverify

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// JobStatus 任务状态。
type JobStatus string

const (
	JobRunning  JobStatus = "running"
	JobDone     JobStatus = "done"
	JobCanceled JobStatus = "canceled"
)

// maxJobsKept 管理器保留的最近任务数（避免长时间运行内存里堆满历史任务）。
const maxJobsKept = 10

// JobSnapshot 任务快照（可直接 JSON 回给面板）。
type JobSnapshot struct {
	ID         string    `json:"id"`
	Status     JobStatus `json:"status"`
	Zone       string    `json:"zone"`
	Total      int       `json:"total"`
	Done       int       `json:"done"`
	Usable     int       `json:"usable"`
	Unusable   int       `json:"unusable"`
	NotExists  int       `json:"not_exists"`
	Error      int       `json:"error"`
	Results    []Result  `json:"results"` // 与输入同序（只含已完成的）
	StartedAt  int64     `json:"started_at"`
	FinishedAt int64     `json:"finished_at,omitempty"`
	ElapsedMs  int64     `json:"elapsed_ms"`
}

// Job 一次批量验证任务。
type Job struct {
	id    string
	zone  string
	total int

	mu         sync.Mutex
	status     JobStatus
	slots      []Result // 按输入下标存放（保证结果与输入同序）
	filled     []bool
	done       int
	usable     int
	unusable   int
	notExists  int
	errCount   int
	startedAt  time.Time
	finishedAt time.Time
	cancel     context.CancelFunc
}

// ID 任务 ID。
func (j *Job) ID() string { return j.id }

// Snapshot 当前进度快照（可随时安全调用）。
func (j *Job) Snapshot() JobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := JobSnapshot{
		ID: j.id, Status: j.status, Zone: j.zone, Total: j.total, Done: j.done,
		Usable: j.usable, Unusable: j.unusable, NotExists: j.notExists, Error: j.errCount,
		StartedAt: j.startedAt.Unix(),
		Results:   make([]Result, 0, j.done),
	}
	for i := range j.slots {
		if j.filled[i] {
			out.Results = append(out.Results, j.slots[i])
		}
	}
	if !j.finishedAt.IsZero() {
		out.FinishedAt = j.finishedAt.Unix()
		out.ElapsedMs = j.finishedAt.Sub(j.startedAt).Milliseconds()
	} else {
		out.ElapsedMs = time.Since(j.startedAt).Milliseconds()
	}
	return out
}

// Cancel 请求停止（在途探测会随 context 取消而中断，且**不记入结果**）。
func (j *Job) Cancel() {
	j.mu.Lock()
	cancel := j.cancel
	running := j.status == JobRunning
	j.mu.Unlock()
	if cancel != nil && running {
		cancel()
	}
}

// record 记录一条结果（按输入下标存放，保持顺序）。
func (j *Job) record(i int, res Result) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.filled[i] {
		return
	}
	j.slots[i] = res
	j.filled[i] = true
	j.done++
	switch {
	case res.Err != "":
		j.errCount++
	case !res.Exists:
		j.notExists++
	case res.Usable:
		j.usable++
	default:
		j.unusable++
	}
}

// finish 收尾（canceled=true 表示被停止）。
func (j *Job) finish(canceled bool) JobSnapshot {
	j.mu.Lock()
	if j.status == JobRunning {
		if canceled {
			j.status = JobCanceled
		} else {
			j.status = JobDone
		}
		j.finishedAt = time.Now()
	}
	j.mu.Unlock()
	return j.Snapshot()
}

// Manager 任务管理器（面板按 ID 查进度；空 ID 取最近一个）。
type Manager struct {
	mu   sync.Mutex
	jobs map[string]*Job
	seq  []string // 插入顺序（淘汰最旧的）
	last string
	n    int
}

// NewManager 创建管理器。
func NewManager() *Manager {
	return &Manager{jobs: map[string]*Job{}}
}

// Start 启动一个批量验证任务：
//   - targets：待验证账号（顺序即结果顺序）
//   - concurrency：并发（<=0 按 1，>MaxConcurrency 收敛）
//   - onResult：每完成一个账号回调（用于**增量**写回账号池），可为 nil
//   - onDone：任务结束（含被停止）回调一次，可为 nil
func (m *Manager) Start(addr string, targets []Target, opt Options, concurrency int,
	onResult func(Result), onDone func(JobSnapshot)) *Job {
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > MaxConcurrency() {
		concurrency = MaxConcurrency()
	}
	opt = opt.withDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{
		id:        newJobID(&m.n),
		zone:      addr,
		total:     len(targets),
		status:    JobRunning,
		slots:     make([]Result, len(targets)),
		filled:    make([]bool, len(targets)),
		startedAt: time.Now(),
		cancel:    cancel,
	}

	m.mu.Lock()
	m.jobs[j.id] = j
	m.seq = append(m.seq, j.id)
	m.last = j.id
	for len(m.seq) > maxJobsKept {
		delete(m.jobs, m.seq[0])
		m.seq = m.seq[1:]
	}
	m.mu.Unlock()

	go func() {
		sem := make(chan struct{}, concurrency)
		var wg sync.WaitGroup
		for i, tg := range targets {
			if ctx.Err() != nil {
				break
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(i int, tg Target) {
				defer wg.Done()
				defer func() { <-sem }()
				res := Probe(ctx, addr, tg.Account, tg.Password, opt)
				if ctx.Err() != nil {
					return // 任务被停止：在途结果丢弃（不能把好号写成"验证失败"）
				}
				j.record(i, res)
				if onResult != nil {
					onResult(res)
				}
			}(i, tg)
		}
		wg.Wait()
		snap := j.finish(ctx.Err() != nil)
		if onDone != nil {
			onDone(snap)
		}
	}()

	return j
}

// Get 取任务：id 为空返回最近一个；未知 ID 返回 nil。
func (m *Manager) Get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" {
		id = m.last
	}
	if id == "" {
		return nil
	}
	return m.jobs[id]
}

func newJobID(seq *int) string {
	*seq++
	return "v" + strconv.FormatInt(time.Now().Unix(), 10) + "-" + strconv.Itoa(*seq)
}

// String 便于日志输出。
func (j *Job) String() string {
	s := j.Snapshot()
	return fmt.Sprintf("job %s [%s] %d/%d", s.ID, s.Status, s.Done, s.Total)
}
