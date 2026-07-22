package protocol

import "testing"

func TestParseHTTPResponseReadsStatusAndHeaders(t *testing.T) {
	response, err := ParseHTTPResponse("HTTP/1.1 204 No Content\nRequest-Id: abc")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 204 || response.Headers["Request-Id"] != "abc" {
		t.Fatalf("unexpected response: %#v", response)
	}
}
