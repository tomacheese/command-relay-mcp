package proto

import (
	"encoding/json"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	req, err := NewRequest(MethodDevicePing, map[string]string{"device_id": "pine"})
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.RequestID = "req-1"

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got Request
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Type != "request" || got.Method != MethodDevicePing || got.RequestID != "req-1" {
		t.Fatalf("got %+v", got)
	}

	var params map[string]string
	if err := json.Unmarshal(got.Params, &params); err != nil {
		t.Fatalf("params unmarshal: %v", err)
	}
	if params["device_id"] != "pine" {
		t.Fatalf("params = %+v", params)
	}
}

func TestResponseErrorRoundTrip(t *testing.T) {
	resp := Response{Type: "response", RequestID: "req-1", Error: &RPCError{Code: ErrProcessNotFound, Message: "no such process"}}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got Response
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Error == nil || got.Error.Code != ErrProcessNotFound {
		t.Fatalf("got %+v", got)
	}
}
