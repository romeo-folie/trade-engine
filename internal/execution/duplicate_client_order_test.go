package execution

import "testing"

func TestIsDuplicateClientOrderError(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		message string
		want    bool
	}{
		{"legacy duplicate wording", 422, "duplicate client order id", true},
		{"alpaca existing wording", 422, "order with client_order_id abc123 already exists", true},
		{"alpaca unique wording", 422, "client_order_id must be unique", true},
		{"spaced client id unique wording", 422, "client order id must be unique", true},
		{"unrelated unique validation", 422, "symbol must be unique", false},
		{"unrelated existing validation", 422, "order already exists", false},
		{"client identifier without conflict", 422, "client_order_id is invalid", false},
		{"wrong status", 400, "client_order_id must be unique", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := &BrokerRequestError{StatusCode: test.status, Message: test.message}
			if got := IsDuplicateClientOrderError(err); got != test.want {
				t.Fatalf("IsDuplicateClientOrderError(%q)=%v, want %v", test.message, got, test.want)
			}
		})
	}
}
