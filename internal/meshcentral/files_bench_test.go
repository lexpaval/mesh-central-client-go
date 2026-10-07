package meshcentral

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral/fakeagent"
)

// delayed relays a websocket to target, holding each message for half of
// rtt in each direction while the next ones keep flowing, like a link with
// that round trip.
func delayed(t testing.TB, target string, rtt time.Duration) string {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		a, _, err := websocket.DefaultDialer.Dial(target, nil)
		if err != nil {
			return
		}
		defer a.Close()
		pipe := func(from, to *websocket.Conn) {
			type msg struct {
				t    int
				b    []byte
				when time.Time
			}
			q := make(chan msg, 1<<16)
			go func() {
				defer close(q)
				for {
					mt, b, err := from.ReadMessage()
					if err != nil {
						return
					}
					q <- msg{mt, b, time.Now().Add(rtt / 2)}
				}
			}()
			for m := range q {
				time.Sleep(time.Until(m.when))
				if to.WriteMessage(m.t, m.b) != nil {
					return
				}
			}
			to.Close()
		}
		go pipe(a, c)
		pipe(c, a)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func BenchmarkFilesOverRelay(b *testing.B) {
	root := b.TempDir()
	data := bytes.Repeat([]byte("0123456789abcdef"), 8<<20/16) // 8 MiB
	os.WriteFile(filepath.Join(root, "down"), data, 0o644)
	agent := httptest.NewServer(fakeagent.Files(root))
	b.Cleanup(agent.Close)
	for _, rtt := range []time.Duration{20 * time.Millisecond, 80 * time.Millisecond} {
		url := delayed(b, "ws"+strings.TrimPrefix(agent.URL, "http"), rtt)
		ws, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			b.Fatal(err)
		}
		s, err := NewFileSession(ws, nil)
		if err != nil {
			b.Fatal(err)
		}
		b.Run("download-rtt"+rtt.String(), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for range b.N {
				if err := s.Download(context.Background(), "/down", io.Discard, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("upload-rtt"+rtt.String(), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for range b.N {
				if err := s.Upload(context.Background(), "/up", bytes.NewReader(data), nil); err != nil {
					b.Fatal(err)
				}
			}
		})
		s.Close()
	}
}
