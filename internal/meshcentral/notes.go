package meshcentral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ErrNotesNotSaved means the server kept other notes than SetNotes sent.
// It drops changes the user has no right to make without saying so: device
// notes need the "Edit notes" right, group notes "Edit group".
var ErrNotesNotSaved = errors.New("the server did not save the notes")

// GetNotes returns the notes of a device (node ID) or group (mesh ID), the
// ones the web UI shows. The server doesn't answer requests it refuses,
// devices the user can't see and groups without the "Edit group" right, so
// ctx needs a deadline.
func GetNotes(ctx context.Context, id string) (string, error) {
	b, _ := json.Marshal(map[string]string{"action": "getNotes", "id": id})
	m, err := request(ctx, replyKey("getNotes", id), b)
	if err != nil {
		return "", err
	}
	notes, _ := m["notes"].(string) // null without notes
	return decodeNotes(notes), nil
}

// notesSettle is how long SetNotes gives the server to store the notes before
// reading them back again.
const notesSettle = 300 * time.Millisecond

// SetNotes replaces the notes of a device or group, empty notes remove them.
// The server doesn't confirm or refuse a change, they're read back instead,
// returning ErrNotesNotSaved if it didn't keep them.
func SetNotes(ctx context.Context, id, notes string) error {
	b, _ := json.Marshal(map[string]string{"action": "setNotes", "id": id, "notes": encodeNotes(notes)})
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

// encodeNotes encodes notes the way the web UI stores them, with
// encodeURIComponent and ' as %27. The server keeps them as sent, the web UI
// decodes them to show them.
func encodeNotes(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*()", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// decodeNotes undoes encodeNotes (decodeURIComponent, + stays a +). Notes
// that don't decode, such as ones earlier versions of mcc saved unencoded,
// are returned as they are.
func decodeNotes(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}
