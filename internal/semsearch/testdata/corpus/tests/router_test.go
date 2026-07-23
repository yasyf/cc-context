package tests

import "testing"

func TestFindRouteSelectsMethodAndPath(t *testing.T) {
	router := NewRouter([]Route{{
		Method:  "GET",
		Pattern: "/health",
		Handle: func(Request) Response {
			return Response{Status: 200, Body: "healthy"}
		},
	}})

	route, err := router.FindRoute(Request{Method: "GET", Path: "/health/"})
	if err != nil {
		t.Fatalf("FindRoute returned an error: %v", err)
	}
	if response := route.Handle(Request{}); response.Status != 200 {
		t.Fatalf("status = %d, want 200", response.Status)
	}
}
