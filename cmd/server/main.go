package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"gocmdreq/internal/jobs"
)

type submitRequest struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Workdir string   `json:"workdir"`
}

type jobResponse struct {
	Job       *jobs.Job `json:"job"`
	LastLines []string  `json:"last_lines,omitempty"`
	ServerNow time.Time `json:"server_time"`
}

func main() {
	var (
		addr          = flag.String("addr", getenvDefault("ADDR", ":8443"), "listen address")
		dataDir       = flag.String("data-dir", getenvDefault("DATA_DIR", "./data"), "directory to persist jobs")
		token         = flag.String("token", os.Getenv("AUTH_TOKEN"), "bearer token required for access")
		tlsCert       = flag.String("tls-cert", os.Getenv("TLS_CERT_FILE"), "path to TLS certificate (PEM)")
		tlsKey        = flag.String("tls-key", os.Getenv("TLS_KEY_FILE"), "path to TLS private key (PEM)")
		allowInsecure = flag.Bool("allow-insecure-http", false, "allow plain HTTP (not recommended)")
		tailLines     = flag.Int("tail-lines", 20, "number of lines to return from job output")
	)
	flag.Parse()

	mgr, err := jobs.NewManager(jobs.Config{
		DataDir:   *dataDir,
		TailLines: *tailLines,
	})
	if err != nil {
		log.Fatalf("init job manager: %v", err)
	}

	authToken := strings.TrimSpace(*token)
	requireAuth := authToken != ""

	mux := http.NewServeMux()
	mux.Handle("/jobs", withAuth(requireAuth, authToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req submitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		job, err := mgr.CreateJob(req.Command, req.Args, req.Workdir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"job_id": job.ID, "status": string(job.Status)})
	})))

	mux.Handle("/jobs/", withAuth(requireAuth, authToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/jobs/")
		if id == "" {
			http.Error(w, "missing job id", http.StatusBadRequest)
			return
		}
		job, err := mgr.Get(id)
		if err != nil {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		lines, _ := mgr.Tail(id)
		job.OutputPath = ""
		resp := jobResponse{
			Job:       job,
			LastLines: lines,
			ServerNow: time.Now().UTC(),
		}
		writeJSON(w, http.StatusOK, resp)
	})))

	mux.Handle("/healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))

	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	if !*allowInsecure && (*tlsCert == "" || *tlsKey == "") {
		log.Fatalf("TLS is required for encrypted transport. Provide --tls-cert and --tls-key or use --allow-insecure-http for local testing.")
	}

	log.Printf("starting server on %s (TLS: %t)", *addr, *tlsCert != "" && *tlsKey != "")
	if *tlsCert != "" && *tlsKey != "" {
		log.Fatal(server.ListenAndServeTLS(*tlsCert, *tlsKey))
	}
	log.Print("WARNING: running without TLS")
	log.Fatal(server.ListenAndServe())
}

func withAuth(require bool, token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if require {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func getenvDefault(key, fallback string) string {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	return val
}
