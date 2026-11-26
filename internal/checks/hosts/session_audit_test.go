package hosts

import "testing"

func TestRejectedSessionReportsSessionFailed(t *testing.T) {
	for _, phase := range []string{"session", "shell"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			s := startServer(t, f.host, f.client.PublicKey(), behavior{rejectAt: phase})
			f.pin(t, s.address, f.host.PublicKey())
			item := probe(t, f, s.address)
			if item.Results["ssh"] != "session_failed" || item.Results["script"] != "unknown" {
				t.Fatalf("ssh = %q, script = %q; want ssh session_failed, script unknown",
					item.Results["ssh"], item.Results["script"])
			}
			for _, dimension := range fields {
				if got := item.Results[dimension]; got != "unknown" {
					t.Errorf("%s = %q; want unknown", dimension, got)
				}
			}
		})
	}
}
