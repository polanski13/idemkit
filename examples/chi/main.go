package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/polanski13/idemkit"
	"github.com/polanski13/idemkit/store/mem"
)

type tenantKey struct{}

func main() {
	store := mem.New(mem.Config{
		TTL:         time.Hour,
		LockTimeout: 30 * time.Second,
	})

	cfg := idemkit.Config{
		KeyScope: func(r *http.Request) string {
			tenant, _ := r.Context().Value(tenantKey{}).(string)
			return tenant
		},
	}

	r := chi.NewRouter()
	r.Use(fakeAuth)
	r.Use(idemkit.Middleware(store, cfg))
	r.Post("/v1/charges", createResource("ch"))
	r.Post("/v1/refunds", createResource("re"))

	addr := ":8080"
	log.Printf("idemkit chi example listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, r))
}

func fakeAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Header.Get("X-Tenant-ID")
		if tenant == "" {
			tenant = "tenant_demo"
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantKey{}, tenant)))
	})
}

func createResource(prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id":     fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()),
			"status": "succeeded",
		})
	}
}
