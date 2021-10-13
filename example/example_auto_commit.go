package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	task "meepo"

	"github.com/go-redis/redis"
	_ "github.com/go-sql-driver/mysql"
)

const (
	userName = "root"
	password = "******"
	ip       = "127.0.0.1"
	port     = "3306"
	dbName   = "*****"
)

var DB *sql.DB
var Client *redis.Client

func init() {
	flag.Parse()

	path := strings.Join([]string{userName, ":", password, "@tcp(", ip, ":", port, ")/", dbName, "?parseTime=true&charset=utf8"}, "")

	db, err := sql.Open("mysql", path)
	if err != nil {
		panic(err)
	}

	db.SetConnMaxLifetime(100)
	db.SetMaxIdleConns(10)
	if err := db.Ping(); err != nil {
		fmt.Println("open database fail")
		return
	}
	fmt.Println("connect success")
	DB = db

	client := redis.NewClient(&redis.Options{
		Addr:     "127.0.0.1:6379",
		Password: "",
		DB:       0,
	})

	pong, err := client.Ping().Result()
	fmt.Println(pong, err)
	Client = client
}

func main() {

	mytask := task.MustNewTask(context.Background(),
		"my_task_0831",
		DB,
		Client,
		task.SetJobFunc(MyJob),
		task.SetMaxFailedTimes(3),
		task.SetParallelNum(10),
		task.SetHandleZombieJobsDstStatus(task.JobStatusTodo),
		task.SetHandleZombieJobsPeriod(time.Second*20),
		task.SetHandleZombieJobsTimeout(time.Second*20),
		task.SetHandleZombieJobsLimit(1000),
		task.SetTableName("t_my_job_0831"),
		task.SetListNamePrefix("l_my_job_0831"),
		task.SetInitDB(true),
	)

	// run consumer 2 to mock another process
	task.MustNewTask(context.Background(),
		"my_task_0831",
		DB,
		Client,
		task.SetJobFunc(MyJob),
		task.SetMaxFailedTimes(3),
		task.SetParallelNum(10),
		task.SetHandleZombieJobsDstStatus(task.JobStatusTodo),
		task.SetHandleZombieJobsPeriod(time.Second*20),
		task.SetHandleZombieJobsTimeout(time.Second*20),
		task.SetHandleZombieJobsLimit(1000),
		task.SetTableName("t_my_job_0831"),
		task.SetListNamePrefix("l_my_job_0831"),
		task.SetInitDB(true),
	)

	// run consumer 3 to mock another process
	task.MustNewTask(context.Background(),
		"my_task_0831",
		DB,
		Client,
		task.SetJobFunc(MyJob),
		task.SetMaxFailedTimes(3),
		task.SetParallelNum(10),
		task.SetHandleZombieJobsDstStatus(task.JobStatusTodo),
		task.SetHandleZombieJobsPeriod(time.Second*20),
		task.SetHandleZombieJobsTimeout(time.Second*20),
		task.SetHandleZombieJobsLimit(1000),
		task.SetTableName("t_my_job_0831"),
		task.SetListNamePrefix("l_my_job_0831"),
		task.SetInitDB(true),
	)

	// mock push data
	totalJobs := 300
	go func() {
		for {
			for i := 0; i < totalJobs; i++ {
				fmt.Println("push", i)
				fmt.Println(mytask.Push([]byte("hello")))
			}
			time.Sleep(time.Second * 60)
		}
	}()

	if err := http.ListenAndServe(":9091", nil); err != nil {
		panic(err)
	}
}

// JobFunc example
func MyJob(ctx context.Context, id, fts, cID int64, payload []byte) error {

	fmt.Println("MyJob assigned job: ", id, string(payload))

	// then, do something
	fmt.Println("do something ...")

	// return if error with succ=false
	if id%99 == 0 {
		return fmt.Errorf("mock error")
	}

	return nil

}
