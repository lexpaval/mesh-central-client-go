package meshcentral

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// notesServer keeps notes like MeshCentral: no reply to a refused getNotes,
// none ever to setNotes. hidden can't be read, readOnly can't be changed.
func notesServer(t *testing.T, hidden, readOnly string) {
	t.Helper()
	var mu sync.Mutex
	notes := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			var req struct{ Action, ID, Notes string }
			if ws.ReadJSON(&req) != nil {
				return
			}
			mu.Lock()
			switch {
			case req.ID == hidden:
			case req.Action == "setNotes" && req.ID != readOnly:
				notes[req.ID] = req.Notes
			case req.Action == "getNotes":
				reply := map[string]any{"action": "getNotes", "id": req.ID, "notes": nil}
				if n, ok := notes[req.ID]; ok && n != "" {
					reply["notes"] = n
				}
				ws.WriteJSON(reply)
			}
			mu.Unlock()
		}
	}))
	t.Cleanup(server.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	settings.wsMu.Lock()
	settings.WebSocket = ws
	settings.wsMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var m map[string]interface{}
			if ws.ReadJSON(&m) != nil {
				return
			}
			if m["action"] == "getNotes" {
				handleNotesCommand(m)
			}
		}
	}()
	t.Cleanup(func() {
		ws.Close()
		<-done
		settings.wsMu.Lock()
		settings.WebSocket = nil
		settings.wsMu.Unlock()
	})
}

func TestNotes(t *testing.T) {
	notesServer(t, "mesh//hidden", "node//ro")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if n, err := GetNotes(ctx, "node//a"); n != "" || err != nil {
		t.Fatalf("notes of a device without any: %q, %v", n, err)
	}
	if err := SetNotes(ctx, "node//a", "rack 4\nUPS on port 2"); err != nil {
		t.Fatal(err)
	}
	if n, err := GetNotes(ctx, "node//a"); n != "rack 4\nUPS on port 2" || err != nil {
		t.Errorf("read back %q, %v", n, err)
	}
	if err := SetNotes(ctx, "node//a", ""); err != nil {
		t.Errorf("removing the notes: %v", err)
	}

	// Concurrent requests for the same ID all get the reply.
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := GetNotes(ctx, "node//a"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	if err := SetNotes(ctx, "node//ro", "x"); !errors.Is(err, ErrNotesNotSaved) {
		t.Errorf("refused change returned %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := GetNotes(short, "mesh//hidden"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("refused read returned %v", err)
	}
	notesRequests.Lock()
	defer notesRequests.Unlock()
	if len(notesRequests.pending) != 0 {
		t.Errorf("requests left pending: %v", notesRequests.pending)
	}
}
