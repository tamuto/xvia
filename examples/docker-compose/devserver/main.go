// Command devserver is a tiny HTTP server used by the Docker Compose example.
// Like most dev servers it binds to loopback only, so it is unreachable from
// outside the container without xvia.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8001", "listen address")
	flag.Parse()
	host, _ := os.Hostname()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from devserver in container %s (%s %s)\n", host, r.Method, r.URL.Path)
	})
	log.Printf("devserver listening on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
