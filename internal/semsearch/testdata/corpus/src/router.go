package source

import (
	"fmt"
	"sort"
	"strings"
)

type Handler func(Request) Response

type Request struct {
	Method string
	Path   string
	Body   string
}

type Response struct {
	Status int
	Body   string
}

type Route struct {
	Method  string
	Pattern string
	Handle  Handler
}

type Router struct {
	routes []Route
}

func NewRouter(routes []Route) *Router {
	ordered := append([]Route(nil), routes...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Pattern < ordered[j].Pattern
	})
	return &Router{routes: ordered}
}

func (r *Router) FindRoute(request Request) (Route, error) {
	cleanPath := normalizePath(request.Path)
	for _, route := range r.routes {
		if route.Method != request.Method {
			continue
		}
		if normalizePath(route.Pattern) == cleanPath {
			return route, nil
		}
	}
	return Route{}, fmt.Errorf("route not found: %s %s", request.Method, cleanPath)
}

func (r *Router) DispatchRequest(request Request) Response {
	route, err := r.FindRoute(request)
	if err != nil {
		return Response{Status: 404, Body: err.Error()}
	}
	return route.Handle(request)
}

func normalizePath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "/"
	}
	return "/" + strings.Trim(trimmed, "/")
}
