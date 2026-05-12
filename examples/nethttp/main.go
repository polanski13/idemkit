package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/polanski13/idemkit"
	"github.com/polanski13/idemkit/store/mem"
)

func main() {
	store := mem.New(mem.Config{
		TTL:         time.Hour,
		LockTimeout: 30 * time.Second,
	})

	mw := idemkit.Middleware(store, idemkit.Config{})

	charges := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":"ch_%d","status":"succeeded"}`, time.Now().UnixNano())
	})

	mux := http.NewServeMux()
	mux.Handle("/v1/charges", mw(charges))

	addr := ":8080"
	log.Printf("idemkit example listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
