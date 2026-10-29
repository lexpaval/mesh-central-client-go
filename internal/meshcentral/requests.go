package meshcentral

import (
	"context"
	"slices"
	"sync"
)

// pendingReplies holds the requests waiting for the server's answer, by
// replyKey. The server answers some requests only when it accepts them, so
// they're bounded by their context.
var pendingReplies = struct {
	sync.Mutex
	m map[string][]chan map[string]interface{}
}{m: make(map[string][]chan map[string]interface{})}

// replyKey names a reply by its action and the ID it carries back, the
// node or responseid the request sent.
func replyKey(action, id string) string { return action + " " + id }

// request sends b and returns the reply delivered for key.
func request(ctx context.Context, key string, b []byte) (map[string]interface{}, error) {
	reply := make(chan map[string]interface{}, 1)
	pendingReplies.Lock()
	pendingReplies.m[key] = append(pendingReplies.m[key], reply)
	pendingReplies.Unlock()
	defer func() {
		pendingReplies.Lock()
		pendingReplies.m[key] = slices.DeleteFunc(pendingReplies.m[key], func(c chan map[string]interface{}) bool { return c == reply })
		if len(pendingReplies.m[key]) == 0 {
			delete(pendingReplies.m, key)
		}
		pendingReplies.Unlock()
	}()
	if err := send(b); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case m := <-reply:
		return m, nil
	}
}

// deliver hands a reply to every request waiting for key.
func deliver(key string, m map[string]interface{}) {
	pendingReplies.Lock()
	defer pendingReplies.Unlock()
	for _, c := range pendingReplies.m[key] {
		c <- m
	}
	delete(pendingReplies.m, key)
}

// handleReply hands the answers to request on, and the device list to
// handleNodesCommand. The list is asked for without a responseid, one
// device for GetDeviceInfo with one.
func handleReply(m map[string]interface{}) {
	action, _ := m["action"].(string)
	var id string
	switch action {
	case "nodes":
		if id, _ = m["responseid"].(string); id == "" {
			handleNodesCommand(m)
			return
		}
	case "getNotes":
		id, _ = m["id"].(string)
	default:
		id, _ = m["nodeid"].(string)
	}
	deliver(replyKey(action, id), m)
}
