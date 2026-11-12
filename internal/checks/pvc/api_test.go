package pvc

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/kube"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAPIReadsClaimsNodesAndKubeletSummary(t *testing.T) {
	for _, scenario := range []string{"healthy", "forbidden", "bad identity", "missing pods", "malformed", "unexpected node"} {
		t.Run(scenario, func(t *testing.T) {
			requested := []string{}
			client := &kube.Client{BaseURL: "https://api.example", HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				requested = append(requested, r.URL.String())
				body := ""
				code := 200
				switch r.URL.Path {
				case "/api/v1/persistentvolumeclaims":
					body = `{"metadata":{"resourceVersion":"1"},"items":[{"metadata":{"namespace":"db","name":"pgdata"},"spec":{"storageClassName":"longhorn"},"status":{"phase":"Bound"}}]}`
				case "/api/v1/nodes":
					body = `{"metadata":{"resourceVersion":"2"},"items":[{"metadata":{"name":"worker"},"status":{"addresses":[{"type":"InternalIP","address":"192.0.2.1"}]}}]}`
				case "/apis/storage.k8s.io/v1/volumeattachments":
					body = `{"metadata":{"resourceVersion":"3"},"items":[]}`
				case "/stats/summary":
					if r.URL.Host != "192.0.2.1:10250" || r.URL.Scheme != "https" {
						t.Fatal(r.URL)
					}
					body = `{"node":{"nodeName":"worker"},"pods":[{"volume":[{"pvcRef":{"namespace":"db","name":"pgdata"},"time":"2026-11-12T12:00:00Z","usedBytes":81,"capacityBytes":100}]}]}`
					switch scenario {
					case "forbidden":
						code = 403
						body = "secret-response-body"
					case "bad identity":
						body = strings.ReplaceAll(body, "worker", "other")
					case "missing pods":
						body = `{"node":{"nodeName":"worker"}}`
					case "malformed":
						body = `{"node":`
					}
				default:
					t.Fatal(r.URL)
				}
				if scenario == "unexpected node" && r.URL.Path == "/api/v1/nodes" {
					body = strings.Replace(body, `}}]}`, `}},{"metadata":{"name":"unlisted"},"status":{"addresses":[{"type":"InternalIP","address":"192.0.2.2"}]}}]}`, 1)
				}
				body = strings.ReplaceAll(body, "2026-11-12T12:00:00Z", now.Format(time.RFC3339))
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}}
			s, err := (API{Client: client, ExpectedNodes: []string{"worker"}, CheckAttachments: true}).Collect(t.Context())
			if scenario != "healthy" && scenario != "unexpected node" {
				if err == nil || strings.Contains(err.Error(), "secret-response-body") {
					t.Fatal(err)
				}
				return
			}
			if err != nil || len(requested) != 4 || len(s.Stats) != 1 {
				t.Fatal(s, err, requested)
			}
			e := New(cfg(), nil).Evaluate(s, now).Aggregate("ok", now, now)
			data, _ := json.Marshal(e)
			if scenario == "unexpected node" && !strings.Contains(e.Results["inventory"], "unlisted") {
				t.Fatal(e)
			}
			if e.Healthy || !strings.Contains(string(data), "pgdata") {
				t.Fatal(e)
			}
			if h := client.Health(time.Now().Add(-time.Minute)); h.OKPaths != 3 {
				t.Fatal("kubelet request recorded as API call", h)
			}
		})
	}
}
