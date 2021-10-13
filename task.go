package task

const (
	JobStatusTodo   JobStatus = "todo"
	JobStatusDone   JobStatus = "done"
	JobStatusFailed JobStatus = "failed"
	jobStatusZombie JobStatus = "zombie"
)

type JobStatus string

type Task interface {
	// param payload, return id, error
	Push([]byte) (int64, error)
	// commit a job to a status under manual commit mode, and will not work under auto commit mode
	Commit(int64, int64, JobStatus) error
	// repush a job by id to redo tash
	Repush(int64) error
}
