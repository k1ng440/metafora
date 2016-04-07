package m_etcd

import (
	"fmt"
	"path"
	"testing"
	"time"

	"golang.org/x/net/context"

	"github.com/coreos/etcd/client"
	"github.com/lytics/metafora"
)

/*
	Running the Integration Test:

ETCDTESTS=1 go test -v ./...
*/

func TestCoordinatorFirstNodeJoiner(t *testing.T) {
	t.Parallel()
	ctx := setupEtcd(t)
	defer ctx.Cleanup()

	if err := ctx.Coord.Init(newCtx(t, "coordinator1")); err != nil {
		t.Fatalf("Unexpected error initialzing coordinator: %v", err)
	}
	defer ctx.Coord.Close()

	tpath := path.Join(ctx.Conf.Namespace, TasksPath)
	_, err := ctx.EtcdClient.Get(context.TODO(), tpath, nil)
	if err != nil {
		if client.IsKeyNotFound(err) {
			t.Fatalf("The tasks path wasn't created when the first node joined: %s", tpath)
		}
		t.Fatalf("Unknown error trying to test: err: %s", err.Error())
	}

	npath := path.Join(ctx.Conf.Namespace, NodesPath)
	if _, err = ctx.EtcdClient.Get(context.TODO(), npath, nil); err != nil {
		if client.IsKeyNotFound(err) {
			t.Fatalf("The nodes path wasn't created when the first node joined: %s", tpath)
		}
		t.Fatalf("Unknown error trying to test: err: %s", err.Error())
	}
}

// Ensure that Watch() picks up new tasks and returns them.
func TestCoordinatorWatch(t *testing.T) {
	t.Parallel()
	ctx := setupEtcd(t)
	defer ctx.Cleanup()
	if err := ctx.Coord.Init(newCtx(t, "coordinator1")); err != nil {
		t.Fatalf("Unexpected error initialzing coordinator: %v", err)
	}
	defer ctx.Coord.Close()

	tasks := make(chan metafora.Task)
	task := DefaultTaskFunc("test-task", "")
	errc := make(chan error)

	go func() {
		// Watch blocks, so we need to test it in its own go routine.
		errc <- ctx.Coord.Watch(tasks)
	}()

	if err := ctx.MClient.SubmitTask(task); err != nil {
		t.Fatalf("Error submitting task: %v", err)
	}

	select {
	case newtask := <-tasks:
		if newtask.ID() != task.ID() {
			t.Fatalf("Received the incorrect task.  Got=%q Expected=%q", newtask, task)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Watch failed to see task after 5 seconds")
	}

	ctx.Coord.Close()
	err := <-errc
	if err != nil {
		t.Fatalf("Watch() returned an err: %v", err)
	}
}

// TestCoordinatorWatchClaim tests watching and claiming multiple tasks.
//
// Each task should be received from Watch at least once.
func TestCoordinatorWatchClaim(t *testing.T) {
	t.Parallel()
	ctx := setupEtcd(t)
	defer ctx.Cleanup()
	if err := ctx.Coord.Init(newCtx(t, "coordinator1")); err != nil {
		t.Fatalf("Unexpected error initialzing coordinator: %v", err)
	}
	defer ctx.Coord.Close()

	testTasks := []string{"test1", "test2", "test3"}

	tasks := make(chan metafora.Task)
	errc := make(chan error)
	go func() {
		//Watch blocks, so we need to test it in its own go routine.
		errc <- ctx.Coord.Watch(tasks)
	}()

	submitted := map[string]bool{}
	claimed := map[string]bool{}
	for _, taskid := range testTasks {
		err := ctx.MClient.SubmitTask(DefaultTaskFunc(taskid, ""))
		if err != nil {
			t.Fatalf("Error submitting a task to metafora via the client: %v", err)
		}
		submitted[taskid] = true
		t.Logf("submitted %s", taskid)

		select {
		case recvd := <-tasks:
			recvdid := recvd.ID()
			if !submitted[recvdid] {
				t.Fatalf("Received task %q when it hadn't been submitted yet!", recvdid)
			}
			if newlyclaimed := ctx.Coord.Claim(recvd); newlyclaimed == claimed[recvdid] {
				t.Fatalf("Claim() -> %t but already claimed=%t", newlyclaimed, claimed[recvdid])
			}
			claimed[recvdid] = true
		case err := <-errc:
			t.Fatalf("Watch returned an error instead of a task: %v", err)
		}
	}

	ctx.Coord.Close()
	err := <-errc
	if err != nil {
		t.Fatalf("Watch() returned an err: %v", err)
	}
}

// TestCoordinatorMulti starts two coordinators to ensure that fighting over
// claims results in only one coordinator winning (and the other not crashing).
func TestCoordinatorMulti(t *testing.T) {
	t.Parallel()
	ctx := setupEtcd(t)
	defer ctx.Cleanup()
	if err := ctx.Coord.Init(newCtx(t, "coordinator1")); err != nil {
		t.Fatalf("Unexpected error initialzing coordinator: %v", err)
	}
	defer ctx.Coord.Close()
	conf2 := ctx.Conf.Copy()
	conf2.Name = "node2"
	coordinator2, _ := NewEtcdCoordinator(conf2)
	if err := coordinator2.Init(newCtx(t, "coordinator2")); err != nil {
		t.Fatalf("Unexpected error initialzing coordinator: %v", err)
	}
	defer coordinator2.Close()

	testTasks := []string{"test-claiming-task0001", "test-claiming-task0002", "test-claiming-task0003"}

	// Start the watchers
	errc := make(chan error, 2)
	c1tasks := make(chan metafora.Task)
	c2tasks := make(chan metafora.Task)
	go func() {
		errc <- ctx.Coord.Watch(c1tasks)
	}()
	go func() {
		errc <- coordinator2.Watch(c2tasks)
	}()

	// Submit the tasks
	for _, tid := range testTasks {
		err := ctx.MClient.SubmitTask(DefaultTaskFunc(tid, ""))
		if err != nil {
			t.Fatalf("Error submitting task=%q to metafora via the client: %v", tid, err)
		}
	}

	//XXX This assumes tasks are sent by watchers in the order they were
	//    submitted to etcd which, while /possible/ to guarantee, isn't a gurantee
	//    we're interested in making.
	//    We only want to guarantee that exactly one coordinator can claim a task.
	c1t := <-c1tasks
	c2t := <-c2tasks
	if c1t.ID() != c2t.ID() {
		t.Logf("Watchers didn't receive the same task %s != %s. It's fine; watch order isn't guaranteed", c1t, c2t)
	}

	// Make sure c1 can claim and c2 cannot
	if ok := ctx.Coord.Claim(c1t); !ok {
		t.Fatalf("coordinator1.Claim() unable to claim the task=%q", c1t)
	}
	if ok := coordinator2.Claim(c1t); ok {
		t.Fatalf("coordinator2.Claim() succeeded for task=%q when it shouldn't have!", c2t)
	}

	// Make sure coordinators close down properly and quickly
	ctx.Coord.Close()
	if err := <-errc; err != nil {
		t.Errorf("Error shutting down coordinator1: %v", err)
	}
	coordinator2.Close()
	if err := <-errc; err != nil {
		t.Errorf("Error shutting down coordinator2: %v", err)
	}
}

// TestCoordinatorRelease ensures the following behavior:
//  1. Tasks submitted before coordinators are running get received once a
//     coordinator is started.
//  2. Released tasks are picked up by a coordinator.
func TestCoordinatorRelease(t *testing.T) {
	t.Parallel()
	ctx := setupEtcd(t)
	defer ctx.Cleanup()

	task := "testtask4"

	err := ctx.MClient.SubmitTask(DefaultTaskFunc(task, ""))
	if err != nil {
		t.Fatalf("Error submitting a task to metafora via the client: %v", err)
	}

	// Don't start up the coordinator until after the metafora client has submitted work.
	if err := ctx.Coord.Init(newCtx(t, "coordinator1")); err != nil {
		t.Fatalf("Unexpected error initialzing coordinator: %v", err)
	}
	defer ctx.Coord.Close()

	errc := make(chan error)
	c1tasks := make(chan metafora.Task)
	go func() {
		errc <- ctx.Coord.Watch(c1tasks)
	}()

	tid := <-c1tasks

	if ok := ctx.Coord.Claim(tid); !ok {
		t.Fatal("coordinator1.Claim() unable to claim the task")
	}

	// Startup a second
	conf2 := ctx.Conf.Copy()
	conf2.Name = "node2"
	coordinator2, _ := NewEtcdCoordinator(conf2)
	if err := coordinator2.Init(newCtx(t, "coordinator2")); err != nil {
		t.Fatalf("Unexpected error initialzing coordinator: %v", err)
	}
	defer coordinator2.Close()

	c2tasks := make(chan metafora.Task)
	go func() {
		errc <- coordinator2.Watch(c2tasks)
	}()

	// coordinator 2 shouldn't be able to claim anything yet
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case recvd := <-c2tasks:
			if coordinator2.Claim(recvd) {
				t.Fatalf("coordinator2.Claim(%s) succeeded when task should have already been claimed", recvd.ID())
			}
		case <-time.After(deadline.Sub(time.Now())):
		}
	}

	// Now release the task from coordinator1 and claim it with coordinator2
	ctx.Coord.Release(tid)
	tid = <-c2tasks
	if ok := coordinator2.Claim(tid); !ok {
		t.Fatalf("coordinator2.Claim(tid) should have succeded on released task", tid)
	}

	ctx.Coord.Close()
	coordinator2.Close()
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil {
			t.Errorf("coordinator returned an error after closing: %v", err)
		}
	}
}

// TestNodeCleanup ensures the coordinator properly cleans up its node entry
// upon exit.
func TestNodeCleanup(t *testing.T) {
	t.Parallel()
	ctx := setupEtcd(t)
	defer ctx.Cleanup()
	if err := ctx.Coord.Init(newCtx(t, "coordinator1")); err != nil {
		t.Fatalf("Unexpected error initialzing coordinator: %v", err)
	}
	defer ctx.Coord.Close()

	conf2 := ctx.Conf.Copy()
	conf2.Name = "node2"
	c2, _ := NewEtcdCoordinator(conf2)
	if err := c2.Init(newCtx(t, "coordinator2")); err != nil {
		t.Fatalf("Unexpected error initialzing coordinator: %v", err)
	}
	defer c2.Close()

	// Make sure node directories were created
	c1nodep := path.Join(ctx.Conf.Namespace, NodesPath, ctx.Conf.Name)
	resp, err := ctx.EtcdClient.Get(context.TODO(), c1nodep, nil)
	if err != nil {
		t.Fatalf("Error retrieving node key from etcd: %v", err)
	}
	if !resp.Node.Dir {
		t.Error(resp.Node.Key + " isn't a directory!")
	}

	c2nodep := path.Join(conf2.Namespace, NodesPath, conf2.Name)
	resp, err = ctx.EtcdClient.Get(context.TODO(), c2nodep, nil)
	if err != nil {
		t.Fatalf("Error retrieving node key from etcd: %v", err)
	}
	if !resp.Node.Dir {
		t.Error(resp.Node.Key + " isn't a directory!")
	}

	// Shutdown one and make sure its node directory is gone
	ctx.Coord.Close()

	resp, err = ctx.EtcdClient.Get(context.TODO(), c1nodep, nil)
	if !client.IsKeyNotFound(err) {
		t.Errorf("Expected node directory to be missing but error: %v", err)
	}

	// Make sure c2 is untouched
	resp, err = ctx.EtcdClient.Get(context.TODO(), c2nodep, nil)
	if err != nil {
		t.Fatalf("Error retrieving node key from etcd: %v", err)
	}
	if !resp.Node.Dir {
		t.Error(resp.Node.Key + " isn't a directory!")
	}
}

// TestNodeRefresher ensures the node refresher properly updates the TTL on the
// node directory in etcd and shuts down the entire consumer on error.
func TestNodeRefresher(t *testing.T) {
	t.Parallel()
	ctx := setupEtcd(t)
	defer ctx.Cleanup()

	// Use a custom node path ttl
	ctx.Conf.NodeTTL = 3 * time.Second
	coord, err := NewEtcdCoordinator(ctx.Conf)
	if err != nil {
		t.Fatalf("Error creating coordinator: %v", err)
	}

	hf := metafora.HandlerFunc(nil) // we won't be handling any tasks
	consumer, err := metafora.NewConsumer(coord, hf, metafora.DumbBalancer)
	if err != nil {
		t.Fatalf("Error creating consumer: %+v", err)
	}

	defer consumer.Shutdown()
	runDone := make(chan struct{})
	go func() {
		consumer.Run()
		close(runDone)
	}()

	nodePath := path.Join(ctx.Conf.Namespace, NodesPath, ctx.Conf.Name)
	ttl := int64(-1)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, _ := ctx.EtcdClient.Get(context.TODO(), nodePath, nil)
		if resp != nil && resp.Node.Dir {
			ttl = resp.Node.TTL
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ttl == -1 {
		t.Fatalf("Node path %s not found.", nodePath)
	}
	if ttl < 1 || ttl > 3 {
		t.Fatalf("Expected TTL to be between 1 and 3, found: %d", ttl)
	}

	// Let it refresh once to make sure that works
	time.Sleep(time.Duration(ttl) * time.Second)
	ttl = -1
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, _ := ctx.EtcdClient.Get(context.TODO(), nodePath, nil)
		if resp != nil && resp.Node.Dir {
			ttl = resp.Node.TTL
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ttl == -1 {
		t.Fatalf("Node path %s not found.", nodePath)
	}

	// Make sure the coordinator is still running fine
	select {
	case <-runDone:
		t.Fatalf("Coordinator unexpectedly exited!")
	default:
	}

	// Now remove the node out from underneath the refresher to cause it to fail
	if _, err := ctx.EtcdClient.Delete(context.TODO(), nodePath, &client.DeleteOptions{Dir: true, Recursive: true}); err != nil {
		t.Fatalf("Unexpected error deleting %s: %v", nodePath, err)
	}

	select {
	case <-runDone:
		// success! run exited
	case <-time.After(10 * time.Second):
		fmt.Println("poof")
		t.Error("Consumer didn't exit even though node directory disappeared!")
		<-chan (int)(nil)
	}
}

// TestExpiration ensures that expired claims get reclaimed properly.
func TestExpiration(t *testing.T) {
	t.Parallel()
	ctx := setupEtcd(t)
	defer ctx.Cleanup()

	claims := make(chan int, 10)
	hf := metafora.HandlerFunc(metafora.SimpleHandler(func(_ metafora.Task, stop <-chan bool) bool {
		claims <- 1
		<-stop
		return true
	}))
	consumer, err := metafora.NewConsumer(ctx.Coord, hf, metafora.DumbBalancer)
	if err != nil {
		t.Fatalf("Error creating consumer: %+v", err)
	}

	opts := &client.SetOptions{TTL: 1, PrevExist: client.PrevNoExist}
	_, err = ctx.EtcdClient.Set(context.TODO(), path.Join(ctx.Conf.Namespace, TasksPath, "abc", OwnerMarker), `{"node":"--"}`, opts)
	if err != nil {
		t.Fatalf("Error creating fake claim: %v", err)
	}

	defer consumer.Shutdown()
	go consumer.Run()

	// Wait for claim to expire and coordinator to pick up task
	select {
	case <-claims:
		// Task claimed!
	case <-time.After(5 * time.Second):
		t.Fatal("Task not claimed long after it should have been.")
	}

	tasks := consumer.Tasks()
	if len(tasks) != 1 {
		t.Fatalf("Expected 1 task to be claimed but found: %v", tasks)
	}
}
