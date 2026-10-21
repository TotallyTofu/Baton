package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"baton/internal/telemetry"
	"baton/internal/win"
)

func TestSlotsOf(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slots" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `[{"id":0,"is_processing":true,"n_ctx":32768,"n_prompt_tokens":4217,"n_prompt_tokens_cache":4000},
			{"id":1,"state":"idle","n_ctx":32768}]`)
	}))
	defer srv.Close()
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(p)

	st := Status{procs: []win.Proc{{PID: 10, Name: "python.exe"}, {PID: 11, Name: "llama-server.exe"}}}
	snap := &telemetry.Snapshot{Listeners: map[int]int{8888: 10, port: 11}}
	got := slotsOf(context.Background(), snap, st, 8888)
	if len(got) != 2 {
		t.Fatalf("slots = %+v", got)
	}
	if s := got[0]; s.Window != 32768 || s.Used != 4217 || s.Cached != 4000 || !s.Busy {
		t.Fatalf("slot 0: %+v", s)
	}
	if got[1].Busy || got[1].Used != 0 {
		t.Fatalf("slot 1: %+v", got[1])
	}
	// No llama-server among the engine's processes: no slot data.
	st.procs = st.procs[:1]
	if got := slotsOf(context.Background(), snap, st, 8888); got != nil {
		t.Fatalf("without llama-server: %+v", got)
	}
}
