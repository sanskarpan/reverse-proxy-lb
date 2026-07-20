package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Args[1]
	id := os.Args[2]

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Backend %s response at %s", id, time.Now().Format(time.RFC3339)) // #nosec G705 -- id is a CLI arg, not user input
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")
	})

	fmt.Printf("Starting backend server %s on port %s\n", id, port)
	if err := http.ListenAndServe(":"+port, nil); err != nil { // #nosec G114 -- test-only server, no timeout needed
		fmt.Printf("Error: %v\n", err)
	}
}
