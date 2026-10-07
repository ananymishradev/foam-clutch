package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	slurmrest "github.com/ananymishradev/foam-clutch"
	"github.com/ananymishradev/foam-clutch/internal/preflight"
	"github.com/ananymishradev/foam-clutch/internal/service"
	"github.com/ananymishradev/foam-clutch/internal/store"
	"github.com/ananymishradev/foam-clutch/internal/supervisor"
	"github.com/ananymishradev/foam-clutch/internal/telemetry"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "validate":
		validate(os.Args[2:])
	case "submit":
		submit(os.Args[2:])
	case "serve":
		serve(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}

}

func submit(args []string) {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	manifestPath := fs.String("manifest", "manifest.yaml", "path to manifest")
	dbPath := fs.String("db", "foam-clutch.db", "SQLite database path")
	watch := fs.Bool("watch", false, "watch Slurm state until the job reaches a terminal state")
	fs.Parse(args)
	baseURL := os.Getenv("SLURM_REST_URL")
	apiVersion := os.Getenv("SLURM_API_VERSION")
	user := os.Getenv("SLURM_USER")
	token := os.Getenv("SLURM_JWT")
	if baseURL == "" || apiVersion == "" || user == "" || token == "" {
		log.Fatal("SLURM_REST_URL, SLURM_API_VERSION, SLURM_USER, and SLURM_JWT are required")
	}
	scheduler, err := slurmrest.NewClient(slurmrest.Config{
		BaseURL: baseURL, APIVersion: apiVersion, User: user,
		Token: func(context.Context) (string, error) { return token, nil },
	})
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	metrics := &telemetry.Metrics{}
	s := service.Service{Scheduler: scheduler, Store: st, Preflight: preflight.Runner{}, Metrics: metrics}
	result, err := s.Submit(context.Background(), *manifestPath)
	if err != nil {
		log.Fatal(err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	if *watch {
		w := supervisor.Supervisor{Scheduler: scheduler, Store: st, Metrics: metrics}
		if err := w.Watch(context.Background(), result.ID, result.SlurmID); err != nil {
			log.Fatal(err)
		}
	}
}
func validate(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	manifestPath := fs.String("manifest", "manifest.yaml", "path to manifest")
	fs.Parse(args)
	s := service.Service{Preflight: preflight.Runner{}}
	m, report, err := s.Validate(context.Background(), *manifestPath)
	out := struct {
		Manifest any    `json:"manifest"`
		Report   any    `json:"report"`
		Error    string `json:"error,omitempty"`
	}{m, report, ""}
	if err != nil {
		out.Error = err.Error()
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	if err != nil {
		os.Exit(1)
	}
}
func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dbPath := fs.String("db", "foam-clutch.db", "SQLite database path")
	fs.Parse(args)
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	metrics := &telemetry.Metrics{}
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		jobs, err := st.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jobs)
	})
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	log.Printf("foam-clutch listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}
func usage() {
	fmt.Fprintln(os.Stderr, "usage: foam-clutch validate -manifest manifest.yaml | submit -manifest manifest.yaml [-db foam-clutch.db] | serve [-addr :8080] [-db foam-clutch.db]")
}
