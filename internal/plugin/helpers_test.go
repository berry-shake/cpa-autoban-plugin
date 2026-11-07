package plugin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

var testNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func newTestService() *Service {
	s := New("1.2.3-test", nil)
	s.now = func() time.Time { return testNow }
	return s
}

// callRPC runs one RPC method and decodes the envelope result into out.
func callRPC(t *testing.T, s *Service, method string, payload any, out any) {
	t.Helper()
	var request []byte
	if payload != nil {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			t.Fatalf("marshal %s request: %v", method, errMarshal)
		}
		request = raw
	}
	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(s.Call(method, request), &env); errUnmarshal != nil {
		t.Fatalf("decode %s envelope: %v", method, errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("%s returned error: %+v", method, env.Error)
	}
	if out != nil {
		if errUnmarshal := json.Unmarshal(env.Result, out); errUnmarshal != nil {
			t.Fatalf("decode %s result: %v", method, errUnmarshal)
		}
	}
}
