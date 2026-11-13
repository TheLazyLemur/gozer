package gozer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type wsClient struct {
	t       *testing.T
	conn    *websocket.Conn
	dom     *fakeDOM
	applied int
}

func dialCircuit(t *testing.T, srv *httptest.Server, id string, seq int, dom *fakeDOM) *wsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/_gozer/ws?c=" + id + "&s=" + itoa(seq)
	conn, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &wsClient{t: t, conn: conn, dom: dom, applied: seq}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// until reads messages, applying batches and acking them, until the DOM text contains want.
func (w *wsClient) until(want string) {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for !strings.Contains(textF(w.dom.root), want) {
		_, data, err := w.conn.Read(ctx)
		if err != nil {
			w.t.Fatalf("waiting for %q (have %q): %v", want, textF(w.dom.root), err)
		}
		var m struct {
			T string          `json:"t"`
			N int             `json:"n"`
			O json.RawMessage `json:"o"`
		}
		must0(json.Unmarshal(data, &m))
		if m.T != "b" || m.N <= w.applied {
			continue
		}
		if m.N != w.applied+1 {
			w.t.Fatalf("batch gap: got %d after %d", m.N, w.applied)
		}
		w.dom.apply(m.O)
		w.applied = m.N
		w.send(map[string]any{"t": "a", "s": m.N})
	}
}

func (w *wsClient) send(v any) {
	data, _ := json.Marshal(v)
	must0(w.conn.Write(context.Background(), websocket.MessageText, data))
}

func TestPrerenderConnectClickAndReconnect(t *testing.T) {
	// given
	// ... an app whose root is a counter, served over HTTP
	app := New()
	app.Root = func() Component { return &counter{Start: 1} }
	srv := httptest.NewServer(app)
	defer srv.Close()

	// when
	// ... the page is fetched, the socket connects, the button is clicked,
	// ... and the client reconnects having applied only the first batch
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	id := regexp.MustCompile(`data-circuit="([0-9a-f]+)"`).FindSubmatch(page)[1]
	dom := newFakeDOM()
	c1 := dialCircuit(t, srv, string(id), 0, dom)
	c1.until("count 1")
	btn := dom.root.kids[0]
	c1.send(map[string]any{"t": "e", "i": btn.id, "n": "click", "d": map[string]any{}})
	c1.send(map[string]any{"t": "e", "i": btn.id, "n": "click", "d": map[string]any{}})
	c1.conn.CloseNow()
	c2 := dialCircuit(t, srv, string(id), c1.applied, dom)
	c2.until("count 3")

	// then
	// ... the prerendered HTML held the first render and the circuit kept its state
	if !strings.Contains(string(page), "<button>count 1</button>") {
		t.Fatalf("prerender missing: %s", page)
	}
	stale := dialCircuit(t, srv, "unknown", 0, newFakeDOM())
	_, _, err = stale.conn.Read(context.Background())
	if websocket.CloseStatus(err) != closeExpired {
		t.Fatalf("unknown circuit: want close 4001, got %v", err)
	}
}
