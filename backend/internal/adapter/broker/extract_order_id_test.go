package broker

import "testing"

// Failure mode: a live MARKET trade hits ERR-5106 "Invalid request
// parameter" on the follow-up GET /v1/executions call if the PlaceOrder
// response parser only handles `data: <scalar>` and `data: {orderId}`
// shapes, because GMO Forex /v1/order returns
//
//	"data": [{ "orderId": 123, "rootOrderId": 123, ..., "status": "EXECUTED" }]
//
// — an ARRAY of order-detail objects. Falling through to the empty-string
// fallback means ResolveExecution is called with orderId="" → GMO 5106.
// Worst part: by then the MARKET order has already filled at the broker
// (an orphaned position). This file pins the array branch and the three
// other known shapes so the next refactor cannot regress.

func TestExtractOrderID_ArrayOfOrderDetails(t *testing.T) {
	// Real /v1/order MARKET response shape captured from fxdocs samples.
	data := []byte(`[{"rootOrderId":123456789,"clientOrderId":"abc","orderId":123456789,"symbol":"USD_JPY","side":"BUY","orderType":"NORMAL","executionType":"MARKET","settleType":"OPEN","size":"1000","status":"EXECUTED","timestamp":"2026-05-21T00:00:00.000Z"}]`)
	if got := extractOrderID(data); got != "123456789" {
		t.Errorf("array data: got %q, want 123456789", got)
	}
}

func TestExtractOrderID_ArrayWithRootOrderIdOnly(t *testing.T) {
	data := []byte(`[{"rootOrderId":555,"status":"WAITING"}]`)
	if got := extractOrderID(data); got != "555" {
		t.Errorf("array w/ rootOrderId only: got %q, want 555", got)
	}
}

func TestExtractOrderID_BareScalar(t *testing.T) {
	if got := extractOrderID([]byte(`12345`)); got != "12345" {
		t.Errorf("scalar number: got %q, want 12345", got)
	}
	if got := extractOrderID([]byte(`"abc-id"`)); got != "abc-id" {
		t.Errorf("scalar string: got %q, want abc-id", got)
	}
}

func TestExtractOrderID_SingleObject(t *testing.T) {
	if got := extractOrderID([]byte(`{"orderId":42}`)); got != "42" {
		t.Errorf("object orderId: got %q, want 42", got)
	}
	if got := extractOrderID([]byte(`{"rootOrderId":777}`)); got != "777" {
		t.Errorf("object rootOrderId: got %q, want 777", got)
	}
}

func TestExtractOrderID_EmptyAndUnknown(t *testing.T) {
	if got := extractOrderID([]byte{}); got != "" {
		t.Errorf("empty: got %q, want empty", got)
	}
	if got := extractOrderID([]byte(`null`)); got != "" {
		t.Errorf("null: got %q, want empty", got)
	}
	if got := extractOrderID([]byte(`[]`)); got != "" {
		t.Errorf("empty array: got %q, want empty", got)
	}
	if got := extractOrderID([]byte(`{"unknown":"shape"}`)); got != "" {
		t.Errorf("unknown object: got %q, want empty", got)
	}
}
