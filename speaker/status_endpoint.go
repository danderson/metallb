package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"

	gokitlog "github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"go.universe.tf/metallb/internal/config"
)

// serveStatus initializes a status endpoint for each protocol in the protocols
// map at the specified host and port and returns a pointer to an http.Server.
func serveStatus(l gokitlog.Logger, protocols map[config.Proto]Protocol, host string, port int, wg *sync.WaitGroup) *http.Server {
	for p := range protocols {
		http.HandleFunc(fmt.Sprintf("/status/%s", p), protocols[p].StatusHandler())
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	server := &http.Server{Addr: addr}

	go func() {
		defer wg.Done()
		level.Info(l).Log("op", "startup", "msg", fmt.Sprintf("starting status endpoint at %s", addr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			level.Error(l).Log("op", "startup", "error", err, "msg", "listening for status requests")
			os.Exit(1)
		}
		level.Info(l).Log("op", "shutdown", "msg", "status endpoint shutdown complete")
	}()

	return server
}
