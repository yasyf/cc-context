package protocol

import (
	"fmt"
	"strconv"
	"strings"
)

type HTTPResponse struct {
	StatusCode int
	Headers    map[string]string
}

func ParseHTTPResponse(rawResponse string) (HTTPResponse, error) {
	lines := strings.Split(rawResponse, "\n")
	statusParts := strings.Fields(lines[0])
	if len(statusParts) < 2 {
		return HTTPResponse{}, fmt.Errorf("missing HTTP status code")
	}
	statusCode, err := strconv.Atoi(statusParts[1])
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("parse HTTP status code: %w", err)
	}
	headers := map[string]string{}
	for _, line := range lines[1:] {
		name, value, found := strings.Cut(line, ":")
		if found {
			headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	return HTTPResponse{StatusCode: statusCode, Headers: headers}, nil
}
