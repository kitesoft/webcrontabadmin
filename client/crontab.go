package main

import (
	"context"
	"fmt"
	"jiacrontab/client/store"
	"jiacrontab/libs"
	"jiacrontab/libs/proto"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func newCrontab(taskChanSize int) *crontab {
	return &crontab{
		taskChan:     make(chan *proto.TaskArgs, taskChanSize),
		delTaskChan:  make(chan *proto.TaskArgs, taskChanSize),
		killTaskChan: make(chan *proto.TaskArgs, taskChanSize),
		handleMap:    make(map[string]*handle),
	}
}

type taskEntity struct {
	id       string
	pid      string
	name     string
	command  string
	taskArgs *proto.TaskArgs
	state    int
	timeout  int64
	sync     bool
	cancel   context.CancelFunc

	ready   chan struct{}
	depends []dependScript
}

type dependScript struct {
	pid        string
	from       string
	dest       string
	done       bool
	logContent []byte
}

func (d *dependScript) exec() {

}

func newTaskEntity(t *proto.TaskArgs) *taskEntity {
	var depends []dependScript
	id := fmt.Sprintf("%d", time.Now().Unix())
	for _, v := range t.Depends {
		depends = append(depends, dependScript{
			pid:  id,
			from: v.From,
			dest: v.Dest,
			done: false,
		})
	}
	return &taskEntity{
		id:      id,
		pid:     t.Id,
		name:    t.Name,
		command: t.Command,
		sync:    t.Sync,
		ready:   make(chan struct{}),
		depends: depends,
	}
}

func (t *taskEntity) exec() {
	now := time.Now()
	atomic.AddInt32(&t.taskArgs.NumberProcess, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	args := strings.Split(t.taskArgs.Args, " ")
	start := now.UnixNano()
	state := t.taskArgs.State
	t.taskArgs.State = 2
	var logContent *[]byte

	go func() {
		// TODO 这里设置脚本依赖超时
		if len(t.taskArgs.Depends) > 0 {
			log.Println("have depends")
			if t.sync {
				for _, v := range t.depends {
					v.exec()
				}
				t.ready <- struct{}{}
			} else {
				for _, v := range t.depends {
					go v.exec()
				}
			}

		} else {
			t.ready <- struct{}{}
		}
	}()

	// 等待所有依赖执行完毕
	<-t.ready

	// 执行脚本
	flag := true

	if t.taskArgs.Timeout != 0 {
		time.AfterFunc(time.Duration(t.taskArgs.Timeout)*time.Second, func() {
			if flag {
				switch t.taskArgs.OpTimeout {
				case "email":
					sendMail(t.taskArgs.MailTo, globalConfig.addr+"提醒脚本执行超时", fmt.Sprintf(
						"任务名：%s\n详情：%s %v\n开始时间：%s\n超时：%ds",
						t.taskArgs.Name, t.taskArgs.Command, t.taskArgs.Args, now.Format("2006-01-02 15:04:05"), t.taskArgs.Timeout))
				case "kill":
					cancel()

				case "email_and_kill":
					cancel()
					sendMail(t.taskArgs.MailTo, globalConfig.addr+"提醒脚本执行超时", fmt.Sprintf(
						"任务名：%s\n详情：%s %v\n开始时间：%s\n超时：%ds",
						t.taskArgs.Name, t.taskArgs.Command, t.taskArgs.Args, now.Format("2006-01-02 15:04:05"), t.taskArgs.Timeout))
				case "ignore":
				default:
				}
			}

		})
	}

	err := wrapExecScript(ctx, fmt.Sprintf("%s-%s.log", t.taskArgs.Name, t.taskArgs.Id), t.taskArgs.Command, globalConfig.logPath, logContent, args...)

	flag = false
	if err != nil && t.taskArgs.UnexpectedExitMail {
		sendMail(t.taskArgs.MailTo, globalConfig.addr+"提醒脚本异常退出", fmt.Sprintf(
			"任务名：%s\n详情：%s %v\n开始时间：%s\n异常：%s",
			t.taskArgs.Name, t.taskArgs.Command, t.taskArgs.Args, now.Format("2006-01-02 15:04:05"), err.Error()))

	}
	atomic.AddInt32(&t.taskArgs.NumberProcess, -1)

	t.taskArgs.LastCostTime = time.Now().UnixNano() - start
	if t.taskArgs.NumberProcess == 0 {
		t.taskArgs.State = state

	} else {
		t.taskArgs.State = 2
	}
	globalStore.Sync()

	log.Printf("%s:%s %v %s %.3fs %v", t.taskArgs.Name, t.taskArgs.Command, t.taskArgs.Args, t.taskArgs.OpTimeout, float64(t.taskArgs.LastCostTime)/1000000000, err)

}

type handle struct {
	cancel         context.CancelFunc   // 取消定时器
	cancelCmdArray []context.CancelFunc // 取消正在执行的脚本
	readyDepends   chan proto.MScriptContent
	clockChan      chan time.Time
	taskPool       []*taskEntity
}

type crontab struct {
	taskChan     chan *proto.TaskArgs
	delTaskChan  chan *proto.TaskArgs
	killTaskChan chan *proto.TaskArgs
	handleMap    map[string]*handle
	lock         sync.RWMutex
	sliceLock    sync.RWMutex
}

func (c *crontab) add(t *proto.TaskArgs) {
	c.taskChan <- t
}

func (c *crontab) quickStart(t *proto.TaskArgs, content *[]byte) {
	c.execTask(t, content, 0)
}

// stop 停止计划任务并杀死正在执行的脚本进程
func (c *crontab) stop(t *proto.TaskArgs) {
	c.kill(t)
	c.delTaskChan <- t
}

// 杀死正在执行的脚本进程
func (c *crontab) kill(t *proto.TaskArgs) {
	c.killTaskChan <- t
}

// 删除计划任务
func (c *crontab) delete(t *proto.TaskArgs) {
	globalStore.Update(func(s *store.Store) {
		delete(s.TaskList, t.Id)
	})
	c.kill(t)
	c.delTaskChan <- t
	log.Println("delete", t.Name, t.Id)
}

func (c *crontab) ids() []string {
	var sli []string
	c.lock.Lock()
	for k := range c.handleMap {
		sli = append(sli, k)
	}

	c.lock.Unlock()
	return sli
}

func (c *crontab) run() {
	// initialize
	go func() {
		globalStore.Update(func(s *store.Store) {
			for _, v := range s.TaskList {
				if v.State != 0 {
					c.add(v)
				}
			}
		}).Sync()

	}()
	// global clock
	go func() {
		t := time.Tick(1 * time.Minute)
		for {
			now := <-t

			// broadcast
			c.lock.Lock()
			for _, v := range c.handleMap {
				select {
				case v.clockChan <- now:
				case <-time.After(2 * time.Second):
				}

			}
			c.lock.Unlock()
		}
	}()

	// add task
	go func() {
		for {
			select {
			case t := <-c.taskChan:
				c.lock.Lock()
				if t.State == 0 {
					t.State = 1
					ctx, cancel := context.WithCancel(context.Background())

					taskPool := make([]*taskEntity, 0)
					c.handleMap[t.Id] = &handle{
						cancel:       cancel,
						readyDepends: make(chan proto.MScriptContent, 10),
						clockChan:    make(chan time.Time),
						taskPool:     taskPool,
					}
					c.lock.Unlock()
					go c.deal(t, ctx)
					log.Printf("add task %s %s", t.Name, t.Id)
				} else {
					c.lock.Unlock()
					log.Printf("task %s %s exists", t.Name, t.Id)
				}
			}
		}
	}()
	// remove task
	go func() {
		for {
			select {
			case task := <-c.delTaskChan:
				c.lock.Lock()
				if handle, ok := c.handleMap[task.Id]; ok {
					if handle.cancel != nil {
						handle.cancel()
						log.Printf("start stop %s", task.Name)
					} else {
						log.Printf("start stop %s failed cancel is nil", task.Name)
					}

				} else {
					log.Printf("can not found %s", task.Name)
					task.State = 0
				}
				c.lock.Unlock()
			}
		}
	}()

	// kill task
	go func() {
		for {
			select {
			case task := <-c.killTaskChan:
				c.lock.Lock()
				if handle, ok := c.handleMap[task.Id]; ok {
					c.lock.Unlock()
					if handle.taskPool != nil {
						for _, v := range handle.taskPool {
							v.cancel()
						}
						log.Println("kill", task.Name, task.Id)
					}
				} else {
					c.lock.Unlock()
				}
			}
		}
	}()

}

func (c *crontab) deal(task *proto.TaskArgs, ctx context.Context) {
	var wgroup sync.WaitGroup
	for {
		c.lock.Lock()
		h := c.handleMap[task.Id]
		c.lock.Unlock()
		select {
		case now := <-h.clockChan:

			go func(now time.Time) {
				defer func() {
					libs.MRecover()
					wgroup.Done()
				}()

				wgroup.Add(1)
				check := task.C
				if checkMonth(check, now.Month()) &&
					checkWeekday(check, now.Weekday()) &&
					checkDay(check, now.Day()) &&
					checkHour(check, now.Hour()) &&
					checkMinute(check, now.Minute()) {

					taskEty := newTaskEntity(task)
					h.taskPool = append(h.taskPool, taskEty)
					taskEty.exec()
				}
			}(now)
		case <-ctx.Done():
			// 等待所有的计划任务执行完毕
			wgroup.Wait()
			task.State = 0
			log.Printf("stop %s %s ok", task.Name, task.Id)

			for k := range task.Depends {
				task.Depends[k].Queue = make([]proto.MScriptContent, 0)
			}
			c.lock.Lock()
			close(c.handleMap[task.Id].clockChan)
			close(c.handleMap[task.Id].readyDepends)
			delete(c.handleMap, task.Id)
			c.lock.Unlock()
			globalStore.Sync()
			return
		}

	}

}

func (c *crontab) resolvedDepends(t *proto.TaskArgs, logContent []byte, taskTime int64, err string) {
	c.lock.Lock()
	if handle, ok := c.handleMap[t.Id]; ok {
		c.lock.Unlock()
		// handle.resolvedDepends <- logContent
		select {
		case handle.readyDepends <- proto.MScriptContent{
			TaskTime:   taskTime,
			LogContent: logContent,
			Err:        err,
			Done:       true,
		}:
		case <-time.After(5 * time.Second):
			log.Printf("taskTime %d failed to write to readyDepends chan", taskTime)
		}

	} else {
		c.lock.Unlock()
		log.Printf("depends: can not found %s", t.Id)
	}

}

func (c *crontab) waitDependsDone(ctx context.Context, taskId string, dpds *[]proto.MScript, logContent *[]byte, taskTime int64, sync bool) bool {
	defer func() {
		// 结束时修改执行状态
		if len(*dpds) > 0 {
			for k, v := range *dpds {
				for key, val := range v.Queue {
					if val.TaskTime == taskTime {
						(*dpds)[k].Queue[key].Done = true
					}
				}
			}
		}

	}()
	flag := true

	if len(*dpds) == 0 {
		log.Printf("taskId:%s depend length %d", taskId, len(*dpds))
		return true
	}

	// 一个脚本开始执行时把时间标志放入队列
	// 并显式声明执行未完成
	curQueueI := proto.MScriptContent{
		TaskTime: taskTime,
		Done:     false,
	}
	for k := range *dpds {
		(*dpds)[k].Queue = append((*dpds)[k].Queue, curQueueI)
	}

	syncFlag := true
	if sync {
		// 同步模式
		syncFlag = pushPipeDepend(*dpds, "", curQueueI)
	} else {
		// 并发模式
		// syncFlag = pushDepends(copyDpds)
		syncFlag = pushDepends(*dpds, curQueueI)
	}
	if !syncFlag {
		prefix := fmt.Sprintf("[%s %s]>>  ", time.Now().Format("2006-01-02 15:04:05"), globalConfig.addr)
		*logContent = []byte(prefix + "failed to exec depends push depends error!\n")
		return syncFlag
	}

	c.lock.Lock()
	// 任务在停止状态下需要手动构造依赖接受通道
	if _, ok := c.handleMap[taskId]; !ok {
		c.handleMap[taskId] = &handle{
			readyDepends: make(chan proto.MScriptContent, 10),
		}
	}

	if handle, ok := c.handleMap[taskId]; ok {
		c.lock.Unlock()

		// 默认所有依赖最终总超时3600
		t := time.Tick(3600 * time.Second)
		for {

			select {
			case <-ctx.Done():
				return false
			case <-t:
				log.Printf("failed to exec depends wait timeout!")
				return false
			// case *logContent = <-handle.resolvedDepends:
			case val := <-handle.readyDepends:
				if val.TaskTime != taskTime {

					checkFlag := false
					for _, v := range (*dpds)[0].Queue {
						if v.TaskTime == val.TaskTime {
							checkFlag = true
						}
					}

					if checkFlag {
						handle.readyDepends <- val
						log.Printf("task %s depend<%d> return to readyDepends chan", taskId, val.TaskTime)
						// 防止重复接收
						time.Sleep(1 * time.Second)
					}

				} else {
					*logContent = val.LogContent
					if val.Err != "" {
						flag = false
					}
					goto end
				}

			}
		}

	} else {
		c.lock.Unlock()
		log.Printf("depends: can not found task %s", taskId)
		return false
	}
end:
	log.Printf("task:%s exec all depends done", taskId)
	return flag
}

// 删除
func (c *crontab) execTask(task *proto.TaskArgs, logContent *[]byte, state int) {
	var err error
	now2 := time.Now()
	start := now2.UnixNano()
	args := strings.Split(task.Args, " ")
	task.LastExecTime = now2.Unix()
	task.State = 2
	atomic.AddInt32(&task.NumberProcess, 1)
	ctx, cancel := context.WithCancel(context.Background())

	// 保存并发执行的终止句柄
	c.lock.Lock()
	if hdl, ok := c.handleMap[task.Id]; ok {
		c.lock.Unlock()
		if len(hdl.cancelCmdArray) >= task.MaxConcurrent {
			hdl.cancelCmdArray[0]()
			hdl.cancelCmdArray = hdl.cancelCmdArray[1:]
		}
		hdl.cancelCmdArray = append(hdl.cancelCmdArray, cancel)
	} else {
		c.lock.Unlock()
	}

	log.Printf("start task %s %s %s %s", task.Name, task.Id, task.Command, task.Args)

	if ok := c.waitDependsDone(ctx, task.Id, &task.Depends, logContent, now2.Unix(), task.Sync); !ok {
		cancel()
		errMsg := fmt.Sprintf("[%s %s %s]>>  failded to exec depends\n", time.Now().Format("2006-01-02 15:04:05"), globalConfig.addr, task.Name)
		*logContent = append(*logContent, []byte(errMsg)...)
		writeLog(globalConfig.logPath, fmt.Sprintf("%s-%s.log", task.Name, task.Id), logContent)
		if task.UnexpectedExitMail {
			costTime := time.Now().UnixNano() - start
			sendMail(task.MailTo, globalConfig.addr+"提醒脚本依赖异常退出", fmt.Sprintf(
				"任务名：%s\n详情：%s %v\n开始时间：%s\n耗时：%.4f\n异常：%s",
				task.Name, task.Command, task.Args, now2.Format("2006-01-02 15:04:05"), float64(costTime)/1000000000, err.Error()))
		}

	} else {
		flag := true

		if task.Timeout != 0 {
			time.AfterFunc(time.Duration(task.Timeout)*time.Second, func() {
				if flag {
					switch task.OpTimeout {
					case "email":
						sendMail(task.MailTo, globalConfig.addr+"提醒脚本执行超时", fmt.Sprintf(
							"任务名：%s\n详情：%s %v\n开始时间：%s\n超时：%ds",
							task.Name, task.Command, task.Args, now2.Format("2006-01-02 15:04:05"), task.Timeout))
					case "kill":
						cancel()

					case "email_and_kill":
						cancel()
						sendMail(task.MailTo, globalConfig.addr+"提醒脚本执行超时", fmt.Sprintf(
							"任务名：%s\n详情：%s %v\n开始时间：%s\n超时：%ds",
							task.Name, task.Command, task.Args, now2.Format("2006-01-02 15:04:05"), task.Timeout))
					case "ignore":
					default:
					}
				}

			})
		}

		err = wrapExecScript(ctx, fmt.Sprintf("%s-%s.log", task.Name, task.Id), task.Command, globalConfig.logPath, logContent, args...)

		flag = false
		if err != nil && task.UnexpectedExitMail {
			sendMail(task.MailTo, globalConfig.addr+"提醒脚本异常退出", fmt.Sprintf(
				"任务名：%s\n详情：%s %v\n开始时间：%s\n异常：%s",
				task.Name, task.Command, task.Args, now2.Format("2006-01-02 15:04:05"), err.Error()))

		}
	}
	atomic.AddInt32(&task.NumberProcess, -1)
	task.LastCostTime = time.Now().UnixNano() - start
	if task.NumberProcess == 0 {

		task.State = state

		// 垃圾回收
		c.sliceLock.Lock()
		for k := range task.Depends {
			task.Depends[k].Queue = make([]proto.MScriptContent, 0)
		}
		c.sliceLock.Unlock()

	} else {
		task.State = 2
	}
	globalStore.Sync()

	log.Printf("%s:%s %v %s %.3fs %v", task.Name, task.Command, task.Args, task.OpTimeout, float64(task.LastCostTime)/1000000000, err)
}
