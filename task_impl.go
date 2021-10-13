package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"meepo/misc"

	"github.com/go-redis/redis"
	"github.com/golang/glog"
)

const (
	createTableSql = `CREATE TABLE IF NOT EXISTS %s (
		id bigint NOT NULL AUTO_INCREMENT COMMENT '自增主键, 无业务意义',
		name varchar(255) NOT NULL COMMENT 'job name, 便于查询',
		task_id varchar(255) NOT NULL COMMENT 'task id',
		task_name varchar(255) NOT NULL COMMENT 'task name',
		status varchar(255) NOT NULL COMMENT 'status',
		payload blob NOT NULL COMMENT 'payload',
		failed_times varchar(64) NOT NULL DEFAULT '0' COMMENT 'failed_times',
		commit_id varchar(255) NOT NULL DEFAULT '0' COMMENT 'commit id: 乐观锁',
		created_at varchar(64) NOT NULL DEFAULT '0' COMMENT '创建时间',
		updated_at varchar(64) NOT NULL DEFAULT '0' COMMENT '更新时间',
		PRIMARY KEY (id),
		KEY idx_status (status(32)),
		KEY idx_task_id (task_id(32)),
		KEY idx_updated_at (updated_at(64)),
		KEY idx_name (name(32))
	) ENGINE=InnoDB AUTO_INCREMENT=11 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='common task job';`

	insertSql = `INSERT INTO %s (name, task_id, task_name, status, payload, failed_times, commit_id, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?);`
	queryJobByName            = `SELECT id, name, task_id, task_name, status, payload, failed_times, commit_id, created_at, updated_at FROM %s WHERE name=? and task_id=? ORDER BY id DESC LIMIT ?;`
	queryJobById              = `SELECT id, name, task_id, task_name, status, payload, failed_times, commit_id, created_at, updated_at FROM %s WHERE id=? and status=? and task_id=?;`
	queryJobByIdWithoutStatus = `SELECT id, name, task_id, task_name, status, payload, failed_times, commit_id, created_at, updated_at FROM %s WHERE id=? and task_id=?;`
	queryZombieJobs           = `SELECT id, name, task_id, task_name, status, payload, failed_times, commit_id, created_at, updated_at FROM %s WHERE updated_at<? and status=? and task_id=? ORDER BY id ASC LIMIT ?;`
	resetJobStatus            = `UPDATE %s SET status=?, updated_at=? WHERE id=? and status=? and updated_at=? and commit_id=?;`
	commitJobZombie           = `UPDATE %s SET status=?, commit_id=commit_id+1 WHERE id=? and status=? and updated_at=? and commit_id=?;`
	commitJobSuccess          = `UPDATE %s SET status=?, updated_at=?, commit_id=commit_id+1 WHERE id=? and status=? and updated_at=? and commit_id=?;`
	commitJobFailure          = `UPDATE %s SET status=?, failed_times=failed_times+1, updated_at=?, commit_id=commit_id+1 WHERE id=? and status=? and updated_at=? and commit_id=?;`
)

type taskImpl struct {
	ctx                           context.Context
	mysql                         *sql.DB
	tableName                     string
	redis                         *redis.Client
	listName                      string
	taskID                        string
	taskName                      string
	jobFunc                       func(context.Context, int64, int64, int64, []byte) error
	maxFailedTimes                int64
	parallelNum                   int64
	panicTaskChan                 chan struct{}
	handleZombieFuncPanicTaskChan chan struct{}
	handleZombieJobsConfig        handleZombieJobsConfig
	dDL                           bool
	disableAutoCommit             bool
}

type handleZombieJobsConfig struct {
	// reset timeout todo job status to another
	// if status="todo", it will push this zombie job back to queue
	// if status="failed" or status="done", it will updated this zombie job status and close it
	// defualtly, zombie job status will be updated to 'zombie'
	status JobStatus
	// period: query db for zombie jobs period
	period time.Duration
	// timeout: timeout to judge a todo job to become a zombie
	timeout time.Duration
	// limit: the limit for query db once period
	limit int64
}

type mysqlJob struct {
	Id          int64  `json:"id"`
	Name        string `json:"name"`
	TaskID      string `json:"task_id"`
	TaskName    string `json:"task_name"`
	Status      string `json:"status"`
	Payload     []byte `json:"payload"`
	FailedTimes int64  `json:"failed_times"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	CommitID    int64  `json:"commit_id"` // 乐观锁
}

type Option func(options *taskImpl)

// set mysql task job table name
func SetTableName(n string) Option {
	return func(options *taskImpl) {
		options.tableName = n
	}
}

// set redis list name prefix
func SetListNamePrefix(n string) Option {
	return func(options *taskImpl) {
		options.listName = n
	}
}

// set task name.
func SetTaskName(n string) Option {
	return func(options *taskImpl) {
		options.taskName = n
	}
}

// set max failed times. Defualt 5. failed will auto redo when failed times less than it
func SetMaxFailedTimes(n int64) Option {
	return func(options *taskImpl) {
		options.maxFailedTimes = n
	}
}

// HandleZombieJobs 是用来处理系统极端情况下丢的消息，如redis宕机、pop重启
// 默认会将超时的 ‘todo’ job 设置为 ‘zombie’ 状态，不作任何其他行为
// SetHandleZombieJobsDstStatus 可以配置将超时的 ‘todo’ job 设置为 ‘failed’或者‘done’状态
// 如果SetHandleZombieJobsDstStatus 配置将超时的 ‘todo’ job 设置为 ‘todo’ 状态， 则系统会重新将job Lpush 到队列中， 如果超时时间设置的过小，可能存在重复消费
func SetHandleZombieJobsDstStatus(dst JobStatus) Option {
	return func(options *taskImpl) {
		options.handleZombieJobsConfig.status = dst
	}
}

// set handle zombie jobs task period. defualt 1 hour
func SetHandleZombieJobsPeriod(t time.Duration) Option {
	return func(options *taskImpl) {
		options.handleZombieJobsConfig.period = t
	}
}

// set timeout to descide a 'todo' job to be zombie. Defualt 24 hours
func SetHandleZombieJobsTimeout(t time.Duration) Option {
	return func(options *taskImpl) {
		options.handleZombieJobsConfig.timeout = t
	}
}

// set handle zombie jobs task once query mysql 'zombie' job limit. Defualt 100
func SetHandleZombieJobsLimit(n int64) Option {
	return func(options *taskImpl) {
		options.handleZombieJobsConfig.limit = n
	}
}

// set whether init DB or not. true means to task try to create mysql table. false means not, which is defualt.
func SetInitDB(e bool) Option {
	return func(options *taskImpl) {
		options.dDL = e
	}
}

// set auto commit. true means task commit job when JobFunc done self(defualt true), consumer can not commit. false means consumer commit job and task never.
func SetAutoCommit(e bool) Option {
	return func(options *taskImpl) {
		options.disableAutoCommit = !e
	}
}

// set consumer numbers to run JobFunc.Defualt 0, which means not to run JobFunc as consumer. Both JobFunc!=nil && ParallelNum>0, it will run JobFunc as consumer.
func SetParallelNum(n int64) Option {
	return func(options *taskImpl) {
		options.parallelNum = n
	}
}

// set JobFunc. Defualt nil, which means not to run JobFunc as consumer. Both JobFunc!=nil && ParallelNum>0, it will run JobFunc as consumer.
// JobFunc params:
// input params: ctx context.Context, jobID int64, failedTimes int64, commitID int64, payload []byte
// return params: err error, when use auto_commit true(defualt), if JobFunc return err not nil, it thinks job failed.
func SetJobFunc(f func(context.Context, int64, int64, int64, []byte) error) Option {
	return func(options *taskImpl) {
		options.jobFunc = f
	}
}

func MustNewTask(ctx context.Context, taskID string, mysql *sql.DB, redis *redis.Client, opts ...Option) Task {
	t := &taskImpl{
		taskID: taskID,
		mysql:  mysql,
		redis:  redis,
	}

	// required
	if t.taskID == "" {
		glog.Fatalf("taskID is empty")
	}
	if t.mysql == nil {
		glog.Fatalf("mysql conn is nil")
	}
	if t.redis == nil {
		glog.Fatalf("redis conn is nil")
	}

	// optional
	for _, op := range opts {
		op(t)
	}

	if t.tableName == "" {
		t.tableName = "t_common_job_" + t.taskID
	}
	if t.listName == "" {
		t.listName = "l_common_job_" + t.taskID
	} else {
		t.listName = t.listName + "_" + t.taskID
	}
	if t.taskName == "" {
		t.taskName = t.taskID
	}
	if t.ctx == nil {
		t.ctx = context.Background()
	}
	if t.maxFailedTimes == 0 {
		t.maxFailedTimes = 5
	}

	if t.handleZombieJobsConfig.status == "" {
		t.handleZombieJobsConfig.status = jobStatusZombie
	}
	if t.handleZombieJobsConfig.period == 0 {
		t.handleZombieJobsConfig.period = time.Hour
	}
	if t.handleZombieJobsConfig.timeout == 0 {
		t.handleZombieJobsConfig.timeout = time.Hour * 24
	}
	if t.handleZombieJobsConfig.limit == 0 {
		t.handleZombieJobsConfig.limit = 100
	}

	// init db
	if t.dDL {
		t.mustInitDB()
	}

	// run task consumers
	if t.parallelNum > 0 && t.jobFunc != nil {
		go t.runHandleZombieJobsFunc()
		go t.run()
	}

	return t
}

func (t *taskImpl) getCtx() context.Context {
	return t.ctx
}

// 将一个job 重新Lpush 到队列. 无论该job当前是什么状态
func (t *taskImpl) Repush(id int64) error {
	if id <= 0 {
		return nil
	}

	job := new(mysqlJob)

	// id, name, task_id, task_name, status, payload, failed_times, created_at, updated_at
	err := t.mysql.QueryRowContext(t.ctx, fmt.Sprintf(queryJobByIdWithoutStatus, t.tableName), id, t.taskID).
		Scan(&job.Id, &job.Name, &job.TaskID, &job.TaskName, &job.Status, &job.Payload, &job.FailedTimes, &job.CommitID, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return fmt.Errorf("query job by id<%d> error: %v", id, err)
	}

	now := time.Now().Unix()
	result, err := t.mysql.ExecContext(t.ctx, fmt.Sprintf(resetJobStatus, t.tableName),
		JobStatusTodo, now, id, job.Status, job.UpdatedAt, job.CommitID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if affected != 1 {
		return fmt.Errorf("job Repush failed: Optimistic lock fail. PreStatus:%s, PreUpdatedAt:%d, PreCommitID:%d", job.Status, job.UpdatedAt, job.CommitID)
	}

	job.Status = string(JobStatusTodo)
	job.UpdatedAt = now
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}

	_, err = t.redis.LPush(t.listName, string(b)).Result()
	if err != nil {
		return err
	}

	return nil
}

// 发送一条消息，返回唯一自增id,如果id==0 或者 err!=nil, 需要重新发消息
func (t *taskImpl) Push(payload []byte) (int64, error) {
	name := misc.RandString(16)
	now := time.Now().Unix()
	_, err := t.mysql.ExecContext(t.ctx, fmt.Sprintf(insertSql, t.tableName), name, t.taskID, t.taskName, JobStatusTodo, payload, 0, 0, now, now)
	if err != nil {
		return 0, err
	}

	job := new(mysqlJob)
	// id, name, task_id, task_name, status, payload, failed_times, created_at, updated_at
	err = t.mysql.QueryRowContext(t.ctx, fmt.Sprintf(queryJobByName, t.tableName), name, t.taskID, 1).
		Scan(&job.Id, &job.Name, &job.TaskID, &job.TaskName, &job.Status, &job.Payload, &job.FailedTimes, &job.CommitID, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return 0, err
	}

	b, err := json.Marshal(job)
	if err != nil {
		return 0, err
	}

	// 错误则db多条脏数据，不影响消息准确性。Producer收到error应该重新发送消息
	out, err := t.redis.LPush(t.listName, string(b)).Result()
	if err != nil {
		glog.Errorf("TaskID(%s), Name(%s): Push call redis.LPush, out: %v\n", t.taskID, t.taskName, out)
		return 0, err
	}

	return job.Id, nil
}

// return id, failed_times, commit_id, payload, err
func (t *taskImpl) pop() (int64, int64, int64, []byte, error) {
	result, err := t.redis.BRPop(time.Minute, t.listName).Result()
	if err != nil {
		glog.Infof("TaskID(%s), Name(%s): Pop call redis.BRPop, out: %v\n", t.taskID, t.taskName, result)
		return 0, 0, 0, nil, err
	}

	if len(result) >= 2 {
		job := new(mysqlJob)
		b := []byte(result[1])
		err := json.Unmarshal(b, job)
		if err != nil {
			return 0, 0, 0, nil, err
		}

		return job.Id, job.FailedTimes, job.CommitID, job.Payload, nil
	}

	return 0, 0, 0, nil, nil

}

// todo -> done
// todo -> todo
// todo -> failed
func (t *taskImpl) commit(id, commitID int64, status JobStatus) error {
	if id <= 0 {
		return nil
	}

	job := new(mysqlJob)

	// id, name, task_id, task_name, status, payload, failed_times, created_at, updated_at
	err := t.mysql.QueryRowContext(t.ctx, fmt.Sprintf(queryJobById, t.tableName), id, JobStatusTodo, t.taskID).
		Scan(&job.Id, &job.Name, &job.TaskID, &job.TaskName, &job.Status, &job.Payload, &job.FailedTimes, &job.CommitID, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return fmt.Errorf("query job by id<%d> error: %v", id, err)
	}

	if commitID != job.CommitID {
		return fmt.Errorf("commit job by id<%d> error: Optimistic lock fail. CommitID has changed. expected:%d, actual:%d", id, job.CommitID, commitID)
	}

	// call
	_, err = t.commitImpl(id, job.UpdatedAt, commitID, status, JobStatusTodo)
	if err != nil {
		return err
	}

	return nil
}

// todo -> any; id: job ID; commitID: 乐观锁; status: dst status
func (t *taskImpl) Commit(id, commitID int64, status JobStatus) error {
	if !t.disableAutoCommit {
		return fmt.Errorf("please set task 'AutoCommit' false")
	}

	return t.commit(id, commitID, status)
}

// any -> any
func (t *taskImpl) commitImpl(id, preUpdatedAt, preCommitID int64, status, preStatus JobStatus) (int64, error) {
	if id <= 0 {
		return 0, fmt.Errorf("invalid id:%d", id)
	}

	if preStatus == JobStatusTodo || preStatus == jobStatusZombie || preStatus == JobStatusDone || preStatus == JobStatusFailed {
		now := time.Now().Unix()

		if status == JobStatusFailed || status == JobStatusTodo {
			result, err := t.mysql.ExecContext(t.ctx, fmt.Sprintf(commitJobFailure, t.tableName), status, now, id, preStatus, preUpdatedAt, preCommitID)
			if err != nil {
				return 0, err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}

			// 将todo job重新放回redis list
			// 乐观锁：重新返回redis list前需要判定是否有其他线程更新了db job的 updated_at 和 staus
			// 如果 affected = 1 则说明本次更新 todo job 成功，且期间无其他线程更新该job
			// 如果 affected = 0 则说明本次没有更新 todo job, 因为期间存在其他线程更新了该job
			if status == JobStatusTodo && affected == 1 {
				job := new(mysqlJob)

				// id, name, task_id, task_name, status, payload, failed_times, created_at, updated_at
				err = t.mysql.QueryRowContext(t.ctx, fmt.Sprintf(queryJobById, t.tableName), id, JobStatusTodo, t.taskID).
					Scan(&job.Id, &job.Name, &job.TaskID, &job.TaskName, &job.Status, &job.Payload, &job.FailedTimes, &job.CommitID, &job.CreatedAt, &job.UpdatedAt)
				if err != nil {
					return affected, err
				}

				if job.Id == 0 {
					return affected, fmt.Errorf("maybe it is not a todo job now, id:%d", id)
				}

				b, err := json.Marshal(job)
				if err != nil {
					return affected, err
				}

				out, err := t.redis.LPush(t.listName, string(b)).Result()
				if err != nil {
					glog.Errorf("TaskID(%s), Name(%s): Push call redis.LPush, out: %v\n", t.taskID, t.taskName, out)
					return affected, err
				}
			}

			return affected, nil
		} else if status == JobStatusDone {
			result, err := t.mysql.ExecContext(t.ctx, fmt.Sprintf(commitJobSuccess, t.tableName), status, now, id, preStatus, preUpdatedAt, preCommitID)
			if err != nil {
				return 0, err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}

			return affected, nil
		} else if status == jobStatusZombie {
			result, err := t.mysql.ExecContext(t.ctx, fmt.Sprintf(commitJobZombie, t.tableName), status, id, preStatus, preUpdatedAt, preCommitID)
			if err != nil {
				return 0, err
			}

			affected, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}

			return affected, nil
		}

		return 0, fmt.Errorf("invalid status: %s", status)
	}
	return 0, fmt.Errorf("invalid prestatus: %s", preStatus)
}

func (t *taskImpl) run() {
	glog.Infof("TaskID(%s), Name(%s): task.run is running ... \n", t.taskID, t.taskName)

	go func() {
		select {
		case <-t.ctx.Done():
			glog.Infof("TaskID(%s), Name(%s): Task guard well quit! \n", t.taskID, t.taskName)
			return
		case <-t.panicTaskChan:
			glog.Infof("TaskID(%s), Name(%s): Task guard restart a task! \n", t.taskID, t.taskName)
			t.run()
		}
	}()

	defer func() {
		if err := recover(); err != nil {
			glog.Infof("TaskID(%s), Name(%s): Task panic recover! error: %v\n", t.taskID, t.taskName, err)
			t.panicTaskChan <- struct{}{}
		}
	}()

	jobChan := make(chan struct{}, t.parallelNum)
	for {
		select {
		case <-t.ctx.Done():
			glog.Infof("TaskID(%s), Name(%s): Task well quit! \n", t.taskID, t.taskName)
			return
		default:
			jobChan <- struct{}{}
			glog.Infof("TaskID(%s), Name(%s): Task consuming one job! \n", t.taskID, t.taskName)

			var err error
			succ := false
			id, fts, commitID, payload := int64(0), int64(0), int64(0), make([]byte, 0)

			go func() {
				defer func() {
					if err := recover(); err != nil {
						glog.Errorf("TaskID(%s), Name(%s), JobID(%d):  Recover Job! error: %v. \n", t.taskID, t.taskName, id, err)
					}

					// disableAutoCommit=true 则由consumer自己commit,如果不commit,一定时间后会重新Lpush, 时间>=redis sync db周期
					if id != 0 && !t.disableAutoCommit {
						// commit job
						if succ {
							err = t.commit(id, commitID, JobStatusDone)
							if err != nil {
								glog.Errorf("TaskID(%s), Name(%s), JobID(%d):  commit job done! error: %v. \n", t.taskID, t.taskName, id, err)
							}

							glog.Infof("TaskID(%s), Name(%s), JobID(%d):  commit job done! \n", t.taskID, t.taskName, id)
						} else if t.maxFailedTimes > 0 && fts+1 >= t.maxFailedTimes {
							err = t.commit(id, commitID, JobStatusFailed)
							if err != nil {
								glog.Errorf("TaskID(%s), Name(%s), JobID(%d):  commit job failed! error: %v. \n", t.taskID, t.taskName, id, err)
							}

							glog.Infof("TaskID(%s), Name(%s), JobID(%d):  commit job failed! \n", t.taskID, t.taskName, id)
						} else {

							err = t.commit(id, commitID, JobStatusTodo)
							if err != nil {
								glog.Errorf("TaskID(%s), Name(%s), JobID(%d):  commit job todo! error: %v. \n", t.taskID, t.taskName, id, err)

							}

							glog.Infof("TaskID(%s), Name(%s), JobID(%d):  commit job todo!  \n", t.taskID, t.taskName, id)
						}
					}

					<-jobChan
					glog.Infof("TaskID(%s), Name(%s): Finish Job! \n", t.taskID, t.taskName)
				}()

				// pop
				glog.Infof("TaskID(%s), Name(%s): pop Job! \n", t.taskID, t.taskName)
				id, fts, commitID, payload, err = t.pop()
				if err != nil {
					glog.Errorf("TaskID(%s), Name(%s): pop Job! error: %v. \n", t.taskID, t.taskName, err)
					return
				}

				if id == 0 {
					return
				}

				// consume
				glog.Infof("TaskID(%s), Name(%s), JobID(%d), failed_times(%d): start Job! \n", t.taskID, t.taskName, id, fts)
				err = t.jobFunc(t.getCtx(), id, fts, commitID, payload)
				if err != nil {
					glog.Errorf("TaskID(%s), Name(%s), JobID(%d): jobFunc error: %v. \n", t.taskID, t.taskName, id, err)
					return
				}

				succ = true
			}()
		}
	}
}

func (t *taskImpl) mustInitDB() {
	defer glog.Infof("TaskID(%s), Name(%s): task init done!\n", t.taskID, t.taskName)

	glog.Infof("TaskID(%s), Name(%s): task init try to create table\n", t.taskID, t.taskName)

	_, err := t.mysql.ExecContext(t.ctx, fmt.Sprintf(createTableSql, t.tableName))
	if err != nil {
		glog.Fatalf("init error: %v", err)
	}
}

func (t *taskImpl) runHandleZombieJobsFunc() {
	glog.Infof("TaskID(%s), Name(%s): task.handleZombieJobsFunc is running ... \n", t.taskID, t.taskName)

	go func() {
		select {
		case <-t.ctx.Done():
			glog.Infof("TaskID(%s), Name(%s): handleZombieJobsFunc guard well quit! \n", t.taskID, t.taskName)
			return
		case <-t.handleZombieFuncPanicTaskChan:
			glog.Infof("TaskID(%s), Name(%s): handleZombieJobsFunc guard restart a task! \n", t.taskID, t.taskName)
			t.handleZombieJobsFunc()
		}
	}()

	defer func() {
		if err := recover(); err != nil {
			glog.Infof("TaskID(%s), Name(%s): handleZombieJobsFunc panic recover! error: %v. \n", t.taskID, t.taskName, err)
			t.handleZombieFuncPanicTaskChan <- struct{}{}
		}
	}()

	t.handleZombieJobsFunc()
}

func (t *taskImpl) handleZombieJobsFunc() {
	t.defaultHandleZombieJobsImpl(
		t.handleZombieJobsConfig.status,
		t.handleZombieJobsConfig.period,
		t.handleZombieJobsConfig.timeout,
		t.handleZombieJobsConfig.limit)
}

func (t *taskImpl) defaultHandleZombieJobsImpl(status JobStatus, period, timeout time.Duration, limit int64) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	times := int64(1)
	for {
		select {
		case <-t.ctx.Done():
			glog.Infof("TaskID(%s), Name(%s): DefaualtHandlerZombieJobs quit! \n", t.taskID, t.taskName)
			return
		case <-ticker.C:
			glog.Infof("TaskID(%s), Name(%s): DefaualtHandlerZombieJobs handling zombie jobs start: NO.%d! \n", t.taskID, t.taskName, times)

			err := t.redisSyncDBTodoJob(timeout, status, limit)
			if err != nil {
				glog.Infof("TaskID(%s), Name(%s): DefaualtHandlerZombieJobs handling zombie jobs error: %v! \n", t.taskID, t.taskName, err)
			}

			glog.Infof("TaskID(%s), Name(%s): DefaualtHandlerZombieJobs handling zombie jobs done: NO.%d! \n", t.taskID, t.taskName, times)
			times++
		}
	}
}

// sync db 'todo' job to redis
func (t *taskImpl) redisSyncDBTodoJob(before time.Duration, dstStatus JobStatus, limit int64) error {
	list := make([]*mysqlJob, 0)
	rows, err := t.mysql.QueryContext(t.ctx, fmt.Sprintf(queryZombieJobs, t.tableName), time.Now().Unix()-int64(before)/1000/1000000, JobStatusTodo, t.taskID, limit)
	if err != nil {

		return err
	}

	for rows.Next() {
		job := new(mysqlJob)
		// id, name, task_id, task_name, status, payload, failed_times, created_at, updated_at
		if err = rows.Scan(&job.Id, &job.Name, &job.TaskID, &job.TaskName, &job.Status, &job.Payload, &job.FailedTimes, &job.CommitID, &job.CreatedAt, &job.UpdatedAt); err != nil {
			glog.Errorf("TaskID(%s), Name(%s): redisSyncDBTodoJob call rows.Scan error: %v\n", t.taskID, t.taskName, err)

			return err
		}

		list = append(list, job)
	}
	rows.Close()

	for _, job := range list {
		_, err := t.commitImpl(job.Id, job.UpdatedAt, job.CommitID, dstStatus, JobStatusTodo) // 利用 commitID、prestatus 做乐观锁
		if err != nil {
			return err
		}
	}

	return nil
}
