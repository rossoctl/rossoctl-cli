package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fastWaitPolling shortens the readiness poll interval for the duration of the
// test, so a test that exercises several polls does not sleep for seconds.
func fastWaitPolling(t *testing.T, interval time.Duration) {
	t.Helper()
	saved := waitPollInterval
	waitPollInterval = interval
	t.Cleanup(func() { waitPollInterval = saved })
}

// waitStep is one scripted response from a fake readiness server: either a
// readyStatus to report with 200, or an HTTP status to fail with.
type waitStep struct {
	status     string // readyStatus to report, when code is 0 or 200
	code       int    // HTTP status to send instead; 0 means 200
	bodyIsJSON bool   // false => an error body, which apiclient reports as-is
}

// ready is a step reporting readyStatus.
func ready(status string) waitStep { return waitStep{status: status} }

// failing is a step answering with an HTTP error code.
func failing(code int) waitStep { return waitStep{code: code} }

// waitServer is a fake backend that answers a resource path with a scripted
// sequence of readiness responses, counting only the requests to that path.
//
// Once the script is exhausted the last step repeats, so a test that waits for a
// timeout does not have to enumerate every poll.
type waitServer struct {
	*httptest.Server

	mu    sync.Mutex
	steps []waitStep
	calls int
}

// requests reports how many times the resource path was fetched. Setup traffic
// (/namespaces) is excluded, so the count is exactly the number of polls.
func (s *waitServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newWaitServer serves steps at resourcePath and a fixed namespace list at
// /api/v1/namespaces, which set-context validates against during setup.
//
// Any other path is an error: `wait` should make exactly one request per poll,
// so a stray fetch (route-status, a list call) is a bug worth failing on rather
// than absorbing.
func newWaitServer(t *testing.T, resourcePath string, steps ...waitStep) *waitServer {
	t.Helper()
	if len(steps) == 0 {
		t.Fatal("newWaitServer needs at least one step")
	}

	s := &waitServer{steps: steps}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"namespaces":["team1","team2"]}`))

		case resourcePath:
			s.mu.Lock()
			i := s.calls
			if i >= len(s.steps) {
				i = len(s.steps) - 1
			}
			step := s.steps[i]
			s.calls++
			s.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			if step.code != 0 {
				w.WriteHeader(step.code)
				_, _ = fmt.Fprintf(w, `{"detail":"scripted %d"}`, step.code)
				return
			}
			_, _ = fmt.Fprintf(w, `{"metadata":{"name":"orders","namespace":"team1"},`+
				`"workloadType":"deployment","readyStatus":%q}`, step.status)

		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestClassifyReadyStatus(t *testing.T) {
	// Every value the backend produces, plus the in-process cortex's "Running",
	// the empty string a server sends before it has computed one, and a value no
	// build knows. Read from the backend's routers/agents.py and routers/tools.py.
	tests := []struct {
		status string
		want   readyState
	}{
		{"Ready", stateReady},
		{"Running", stateReady}, // in-process cortex; see serve.instanceStatus
		{"Not Ready", statePending},
		{"Progressing", statePending},
		{"Pending", statePending},
		{"Building", statePending},
		{"Unknown", statePending},
		{"Failed", stateFailed},
		{"Build Failed", stateFailed},
		{"", statePending},            // not yet computed
		{"Rebalancing", statePending}, // a status this build predates
		{"  Ready  ", stateReady},     // surrounding space is not significant
	}

	names := map[readyState]string{
		stateReady:   "ready",
		stateFailed:  "failed",
		statePending: "pending",
	}

	for _, tc := range tests {
		if got := classifyReadyStatus(tc.status); got != tc.want {
			t.Errorf("classifyReadyStatus(%q) = %s, want %s",
				tc.status, names[got], names[tc.want])
		}
	}
}
