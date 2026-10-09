package broker

import (
	"errors"
	"testing"

	"fx-bot/backend/internal/port"
)

// errorFromEnvelope must wrap GMO's ERR-254 ("Not found position") in the
// shared port.ErrBrokerPositionNotFound sentinel so the close saga can detect
// the benign already-closed race (settle leg filled first) via errors.Is and
// skip emergency_stop. Other error codes must NOT carry the sentinel.
func TestErrorFromEnvelope_ERR254_WrapsPositionNotFoundSentinel(t *testing.T) {
	env := &gmoEnvelope{Status: 1}
	env.Messages = []struct {
		MessageCode   string `json:"message_code"`
		MessageString string `json:"message_string"`
	}{{MessageCode: "ERR-254", MessageString: "Not found position."}}

	err := errorFromEnvelope(env)
	if err == nil {
		t.Fatal("expected an error for status=1")
	}
	if !errors.Is(err, port.ErrBrokerPositionNotFound) {
		t.Errorf("ERR-254 must wrap ErrBrokerPositionNotFound; got %v", err)
	}

	// A different code must not be mistaken for "position not found".
	other := &gmoEnvelope{Status: 1}
	other.Messages = []struct {
		MessageCode   string `json:"message_code"`
		MessageString string `json:"message_string"`
	}{{MessageCode: "ERR-5003", MessageString: "Requests are too many."}}
	if errors.Is(errorFromEnvelope(other), port.ErrBrokerPositionNotFound) {
		t.Error("ERR-5003 must NOT wrap ErrBrokerPositionNotFound")
	}
}
