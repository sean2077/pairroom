package main

import (
	"github.com/sean2077/pairroom/internal/enterpriseservice"
	"net"
)

func newEnterpriseLimitedListener(inner net.Listener, limit int) (net.Listener, error) {
	return enterpriseservice.LimitListener(inner, limit)
}
