package pgbackrest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/state"
)

// configMapServer is a fake API server holding the argus-state ConfigMap.
type configMapServer struct {
	mu      sync.Mutex
	data    string
	version int
	puts    int
	// beforePut may change the stored document or reject the request.
	beforePut func(attempt int) (status int)
	// slowCommit delays the response to a committed PUT past the client timeout.
	slowCommit bool
}

func (s *configMapServer) object() []byte {
	data, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "argus-state", "namespace": "argus", "resourceVersion": strconv.Itoa(s.version), "labels": map[string]string{"keep": "me"}},
		"data":     map[string]string{state.Key: s.data}})
	return data
}

func (s *configMapServer) set(data string) {
	s.data = data
	s.version++
}

func (s *configMapServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/namespaces/argus/configmaps/argus-state" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.mu.Lock()
	if r.Method == http.MethodGet {
		body := s.object()
		s.mu.Unlock()
		_, _ = w.Write(body)
		return
	}
	attempt := s.puts
	s.puts++
	if s.beforePut != nil {
		if status := s.beforePut(attempt); status != 0 {
			s.mu.Unlock()
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"secret-conflict-body"}`))
			return
		}
	}
	raw, _ := io.ReadAll(r.Body)
	var object struct {
		Metadata struct {
			ResourceVersion string            `json:"resourceVersion"`
			Labels          map[string]string `json:"labels"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal(raw, &object) != nil || object.Metadata.ResourceVersion != strconv.Itoa(s.version) || object.Metadata.Labels["keep"] != "me" {
		s.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		return
	}
	s.set(object.Data[state.Key])
	body := s.object()
	slow := s.slowCommit && attempt == 0
	s.mu.Unlock()
	if slow {
		time.Sleep(300 * time.Millisecond)
	}
	_, _ = w.Write(body)
}

func (s *configMapServer) stored(t *testing.T) (state.Ledger, error) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return state.Decode([]byte(s.data))
}

func apiStore(t *testing.T, server *configMapServer) *state.Store {
	t.Helper()
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	client, err := kube.New(httpServer.URL, "", "", "test", 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return state.New(&state.ConfigMap{Client: client, Namespace: "argus", Name: "argus-state"})
}

func TestConfigMapBackendConvergence(t *testing.T) {
	tombstone := state.Entry{UID: "other-uid", Created: time.Now().Add(-48 * time.Hour).UTC(), RepositoryID: "missing", NotBefore: time.Now().Add(-time.Hour).UTC()}
	other, _ := state.Encode(state.Ledger{"old/old/repo1": tombstone})
	for name, tc := range map[string]struct {
		server     *configMapServer
		wantState  string
		wantPuts   int
		wantStored func(state.Ledger) bool
	}{
		"409 from another writer": {
			server:    &configMapServer{data: emptyLedger, version: 1},
			wantState: "ok", wantPuts: 2,
			wantStored: func(l state.Ledger) bool {
				return l["old/old/repo1"].UID == "other-uid" && !l["db/db/repo1"].LastSuccess.IsZero()
			},
		},
		"timeout then read-back": {
			server:    &configMapServer{data: emptyLedger, version: 1, slowCommit: true},
			wantState: "ok", wantPuts: 1,
			wantStored: func(l state.Ledger) bool { return !l["db/db/repo1"].LastSuccess.IsZero() },
		},
		"corrupted underneath": {
			server:    &configMapServer{data: emptyLedger, version: 1},
			wantState: "invalid", wantPuts: 1,
		},
		"forbidden update": {
			server:    &configMapServer{data: emptyLedger, version: 1, beforePut: func(int) int { return http.StatusForbidden }},
			wantState: "unpersisted", wantPuts: writeAttempts,
			wantStored: func(l state.Ledger) bool { return len(l) == 0 },
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := tc.server
			switch name {
			case "409 from another writer":
				s.beforePut = func(attempt int) int {
					if attempt == 0 {
						s.set(string(other))
					}
					return 0
				}
			case "corrupted underneath":
				s.beforePut = func(int) int {
					s.set(`{"version":7,"entries":{}}`)
					return http.StatusConflict
				}
			}
			r := harness(New(testConfig(), &fakeAPI{sample: Sample{Clusters: []kube.Cluster{liveCluster(t)}}}, apiStore(t, s), false, discard))
			r.Refresh(t.Context(), "pgbackrest")
			_, e := lookup(t, r, "db/db/repo1", time.Now())
			if e.Results["state"] != tc.wantState || s.puts != tc.wantPuts {
				t.Fatal(e.Results, s.puts)
			}
			if strings.Contains(e.Summary, "secret") {
				t.Fatal("response body leaked")
			}
			if tc.wantStored != nil {
				stored, err := s.stored(t)
				if err != nil || !tc.wantStored(stored) {
					t.Fatal(stored, err)
				}
			}
			if name == "corrupted underneath" && s.data != `{"version":7,"entries":{}}` {
				t.Fatal("unknown version overwritten")
			}
		})
	}
}

func TestConfigMapBackendRejectsInvalidDocuments(t *testing.T) {
	for name, data := range map[string]string{
		"corrupt":         `{"version":2,"entries":`,
		"unknown version": `{"version":3,"entries":{}}`,
		"oversized":       `{"version":2,"entries":{}}` + strings.Repeat(" ", state.MaxBytes),
	} {
		t.Run(name, func(t *testing.T) {
			s := &configMapServer{data: data, version: 1}
			r := harness(New(testConfig(), &fakeAPI{sample: Sample{Clusters: []kube.Cluster{liveCluster(t)}}}, apiStore(t, s), false, discard))
			r.Refresh(t.Context(), "pgbackrest")
			if _, e := lookup(t, r, "db/db/repo1", time.Now()); e.Results["state"] != "invalid" || s.puts != 0 || s.data != data {
				t.Fatal(e.Results, s.puts)
			}
		})
	}
}
