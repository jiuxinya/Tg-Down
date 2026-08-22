package telegram

import (
	"sync"
	"testing"
)

func TestMonitorSnapshotIsConsistentDuringTaskSwitch(t *testing.T) {
	c := newTestClient(t)
	first := monitorState{chatID: 101, taskID: "task-a", chatTitle: "chat-a"}
	second := monitorState{chatID: 202, taskID: "task-b", chatTitle: "chat-b"}
	c.SetMonitorTask(first.taskID, first.chatID, first.chatTitle)

	const iterations = 10_000
	start := make(chan struct{})
	badSnapshot := make(chan monitorState, 1)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			state := first
			if i%2 != 0 {
				state = second
			}
			c.SetMonitorTask(state.taskID, state.chatID, state.chatTitle)
		}
	}()

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				state := c.monitorSnapshot()
				if state != first && state != second {
					select {
					case badSnapshot <- state:
					default:
					}
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
	close(badSnapshot)
	if state, ok := <-badSnapshot; ok {
		t.Fatalf("读到混合监控状态: %+v", state)
	}
}
