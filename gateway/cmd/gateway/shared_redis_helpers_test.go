package main

import (
	"crypto/tls"
	"net"
)

func redisTLSForTest() *tls.Config { return &tls.Config{MinVersion: tls.VersionTLS12} }

func splitHostPortForTest(addr string) (host, port string, err error) { return net.SplitHostPort(addr) }
