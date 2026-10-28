package meshcentral

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"
)

// ErrNotesNotSaved means the server kept other notes than SetNotes sent.
// It drops changes the user has no right to make without saying so: device
// notes need the "Edit notes" right, group notes "Edit group".
var ErrNotesNotSaved = errors.New("the server did not save the notes")

// notesRequests holds the GetNotes calls waiting for their reply, by ID.
var notesRequests = struct {
	sync.Mutex
	pending map[string][]chan string
}{pending: make(map[string][]chan string)}

func handleNotesCommand(m map[string]interface{}) {
	id, _ := m["id"].(string)
	notes, _ := m["notes"].(string) // null without notes
	notesRequests.Lock()
	defer notesRequests.Unlock()
	for _, c := range notesRequests.pending[id] {
		c <- notes
	}
	delete(notesRequests.pending, id)
}

// GetNotes returns the notes of a device (node ID) or group (mesh ID), the
// ones the web UI shows. The server doesn't answer requests it refuses,
// devices the user can't see and groups without the "Edit group" right, so
// ctx needs a deadline.
func GetNotes(ctx context.Context, id string) (string, error) {
	reply := make(chan string, 1)
	notesRequests.Lock()
	notesRequests.pending[id] = append(notesRequests.pending[id], reply)
	notesRequests.Unlock()
	defer func() {
		notesRequests.Lock()
		notesRequests.pending[id] = slices.DeleteFunc(notesRequests.pending[id], func(c chan string) bool { return c == reply })
		if len(notesRequests.pending[id]) == 0 {
			delete(notesRequests.pending, id)
		}
		notesRequests.Unlock()
	}()
	b, _ := json.Marshal(map[string]string{"action": "getNotes", "id": id})
	if err := send(b); err != nil {
		return "", err
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case notes := <-reply:
		return notes, nil
	}
}

// notesSettle is how long SetNotes gives the server to store the notes before
// reading them back again.
const notesSettle = 300 * time.Millisecond

// SetNotes replaces the notes of a device or group, empty notes remove them.
// The server doesn't confirm or refuse a change, they're read back instead,
// returning ErrNotesNotSaved if it didn't keep them.
func SetNotes(ctx context.Context, id, notes string) error {
	b, _ := json.Marshal(map[string]string{"action": "setNotes", "id": id, "notes": notes})
	if err := send(b); err != nil {
		return err
	}
	// The server stores and reads asynchronously, a read right after the
	// write can still see the old notes.
	for range 3 {
		got, err := GetNotes(ctx, id)
		if err != nil {
			return err
		}
		if got == notes {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(notesSettle):
		}
	}
	return ErrNotesNotSaved
}
