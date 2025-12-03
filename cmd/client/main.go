package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type job struct {
	ID          string     `json:"id"`
	Command     string     `json:"command"`
	Args        []string   `json:"args"`
	Workdir     string     `json:"workdir"`
	Status      string     `json:"status"`
	ExitCode    *int       `json:"exit_code"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	LastError   string     `json:"last_error"`
}

type jobResponse struct {
	Job       job       `json:"job"`
	LastLines []string  `json:"last_lines"`
	ServerNow time.Time `json:"server_time"`
}

type multiArg []string

func (m *multiArg) String() string { return strings.Join(*m, " ") }
func (m *multiArg) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}

	cmd := os.Args[1]
	switch cmd {
	case "submit":
		submit(os.Args[2:])
	case "status":
		status(os.Args[2:])
	default:
		usage()
	}
}

func submit(args []string) {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	server := fs.String("server", getenvDefault("SERVER_URL", "https://localhost:8443"), "server URL")
	token := fs.String("token", os.Getenv("AUTH_TOKEN"), "bearer token")
	workdir := fs.String("workdir", "", "working directory on server")
	cmd := fs.String("command", "", "command to execute")
	var cmdArgs multiArg
	fs.Var(&cmdArgs, "arg", "command argument (repeatable)")
	insecure := fs.Bool("insecure", false, "skip TLS verification (self-signed)")
	_ = fs.Parse(args)

	if strings.TrimSpace(*cmd) == "" {
		log.Fatal("command is required")
	}

	body := map[string]interface{}{
		"command": *cmd,
		"args":    []string(cmdArgs),
		"workdir": *workdir,
	}
	payload, _ := json.Marshal(body)

	resp, err := doRequest("POST", *server+"/jobs", *token, *insecure, bytes.NewReader(payload))
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		log.Fatalf("submit failed: %s", strings.TrimSpace(string(msg)))
	}
	var decoded map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		log.Fatalf("decode response: %v", err)
	}
	fmt.Printf("job submitted: %s (status=%s)\n", decoded["job_id"], decoded["status"])
}

func status(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	server := fs.String("server", getenvDefault("SERVER_URL", "https://localhost:8443"), "server URL")
	token := fs.String("token", os.Getenv("AUTH_TOKEN"), "bearer token")
	insecure := fs.Bool("insecure", false, "skip TLS verification (self-signed)")
	_ = fs.Parse(args)

	if fs.NArg() != 1 {
		log.Fatal("usage: status <job-id>")
	}
	jobID := fs.Arg(0)

	resp, err := doRequest("GET", *server+"/jobs/"+jobID, *token, *insecure, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		log.Fatalf("status failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var decoded jobResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		log.Fatalf("decode response: %v", err)
	}

	fmt.Printf("job: %s\n", decoded.Job.ID)
	fmt.Printf("  command: %s %s\n", decoded.Job.Command, strings.Join(decoded.Job.Args, " "))
	if decoded.Job.Workdir != "" {
		fmt.Printf("  workdir: %s\n", decoded.Job.Workdir)
	}
	fmt.Printf("  status: %s\n", decoded.Job.Status)
	if decoded.Job.ExitCode != nil {
		fmt.Printf("  exit code: %d\n", *decoded.Job.ExitCode)
	}
	if decoded.Job.LastError != "" {
		fmt.Printf("  error: %s\n", decoded.Job.LastError)
	}
	if decoded.Job.StartedAt != nil {
		fmt.Printf("  started: %s\n", decoded.Job.StartedAt.Format(time.RFC3339))
	}
	if decoded.Job.CompletedAt != nil {
		fmt.Printf("  completed: %s\n", decoded.Job.CompletedAt.Format(time.RFC3339))
	}
	if len(decoded.LastLines) > 0 {
		fmt.Println("  output tail:")
		for _, line := range decoded.LastLines {
			fmt.Printf("    %s\n", line)
		}
	}
}

func doRequest(method, url, token string, insecure bool, body io.Reader) (*http.Response, error) {
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, // opt-in for self-signed dev certs
		},
		Timeout: 30 * time.Second,
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}

func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func usage() {
	fmt.Println("usage:")
	fmt.Println("  client submit --command <cmd> [--arg value]... [--workdir path] [--server url] [--token token] [--insecure]")
	fmt.Println("  client status <job-id> [--server url] [--token token] [--insecure]")
}
